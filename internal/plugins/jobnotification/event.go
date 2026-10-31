package jobnotification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maxSafeInteger      = 1<<53 - 1
	metadataStringBytes = 8192
	metadataTotalBytes  = 32768
)

var (
	states         = set("COMPLETED", "CANCELLED", "FAILED", "TIMEOUT", "NODE_FAIL", "PREEMPTED", "BOOT_FAIL", "DEADLINE", "OUT_OF_MEMORY")
	sluidPattern   = regexp.MustCompile(`^s[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{13}$`)
	timeLimitPat   = regexp.MustCompile(`^[0-9]+ min$`)
	stringFields   = []string{"requested_tres", "allocated_tres", "stdout_expanded", "standard_output", "batch_host", "command", "working_directory", "submit_line", "nodes", "partition", "account", "qos", "stderr_expanded", "standard_error", "stdin_expanded", "standard_input", "cpus_per_tres", "memory_per_tres", "tres_per_node", "user_name", "group_name"}
	integerFields  = []string{"cpus", "node_count", "tasks", "cpus_per_task", "memory_per_node_mib", "memory_per_cpu_mib", "priority", "restart_count", "group_id", "submit_time", "start_time", "end_time"}
	timeFields     = []string{"submitted_at", "started_at", "ended_at"}
	metadataFields = slices.Concat(stringFields, integerFields, []string{"time_limit"}, timeFields)
	metadataPaths  = func() map[string]bool {
		m := map[string]bool{}
		for _, f := range metadataFields {
			m["job."+f] = true
		}
		return m
	}()
	jobFields      = set(slices.Concat([]string{"job_id", "sluid", "uid", "name", "array", "heterogeneous", "metadata_complete", "metadata_omitted"}, metadataFields)...)
	errEvent       = invalid("invalid completion event")
	errMetadata    = invalid("invalid job metadata")
	errUsage       = invalid("invalid resource usage")
	errMalformed   = invalid("malformed JSON body")
	errSingleValue = invalid("request body must contain a single JSON object")
)

type gpuUsage struct {
	AllocatedGPUSeconds             *float64  `json:"allocated_gpu_seconds"`
	ObservedGPUSeconds              *float64  `json:"observed_gpu_seconds"`
	UtilizationPercentSeconds       *float64  `json:"utilization_percent_seconds"`
	MemoryUtilizationPercentSeconds *float64  `json:"memory_utilization_percent_seconds"`
	MeanUtilizationPercent          *float64  `json:"mean_utilization_percent"`
	MeanMemoryUtilizationPercent    *float64  `json:"mean_memory_utilization_percent"`
	Complete                        *bool     `json:"complete"`
	MissingReasons                  *[]string `json:"missing_reasons"`
}

type wireEvent struct {
	Version   int                        `json:"version"`
	EventID   string                     `json:"event_id"`
	ClusterID string                     `json:"cluster_id"`
	Job       map[string]json.RawMessage `json:"job"`
	Result    *struct {
		State    string          `json:"state"`
		ExitCode json.RawMessage `json:"exit_code"`
	} `json:"result"`
	Duration *struct {
		ActiveSeconds   *int64 `json:"active_seconds"`
		Scope           string `json:"scope"`
		ExcludesSuspend bool   `json:"excludes_suspend"`
	} `json:"duration"`
	Raw   map[string]json.RawMessage `json:"raw"`
	Usage *struct {
		GPU *gpuUsage `json:"gpu"`
	} `json:"usage"`
	ObservedAt string `json:"observed_at"`
}

// event is a validated completion plus the generic view templates render from.
type event struct {
	ID            string
	JobID         int64
	SLUID         string
	UID           int64
	ActiveSeconds int64
	Context       map[string]any
}

func (e event) fingerprint(cluster string) string {
	b, _ := json.Marshal([]any{cluster, e.JobID, e.SLUID, e.UID})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func decodeStrict(body []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errMalformed
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return errSingleValue
	}
	return nil
}

func decodeGeneric(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, errMalformed
	}
	return m, nil
}

func isNull(raw json.RawMessage) bool { return raw == nil || string(bytes.TrimSpace(raw)) == "null" }

func objectRaw(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && t[0] == '{'
}

func validUTC(s string) bool {
	if !utcPattern.MatchString(s) || strings.HasPrefix(s, "0000") {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

func parseEvent(body []byte, cluster string) (event, error) {
	var in wireEvent
	if err := decodeStrict(body, &in); err != nil {
		return event{}, err
	}
	var ev event
	j := in.Job
	if in.Version != 1 || in.ClusterID != cluster || j == nil || in.Result == nil || !states[in.Result.State] ||
		in.Duration == nil || in.Duration.ActiveSeconds == nil || *in.Duration.ActiveSeconds < 0 || *in.Duration.ActiveSeconds > maxSafeInteger ||
		in.Duration.Scope != "last_attempt" || !in.Duration.ExcludesSuspend || len(in.ObservedAt) > 64 {
		return ev, errEvent
	}
	if _, err := time.Parse(time.RFC3339Nano, in.ObservedAt); err != nil {
		return ev, errEvent
	}
	var name string
	if json.Unmarshal(j["job_id"], &ev.JobID) != nil || ev.JobID < 1 || ev.JobID > math.MaxUint32 ||
		json.Unmarshal(j["sluid"], &ev.SLUID) != nil || !sluidPattern.MatchString(ev.SLUID) ||
		json.Unmarshal(j["uid"], &ev.UID) != nil || ev.UID < 0 || ev.UID > math.MaxUint32 ||
		isNull(j["name"]) || json.Unmarshal(j["name"], &name) != nil || jsLen(name) > 4096 {
		return ev, errEvent
	}
	if !validRaw(in.Raw) || in.EventID != cluster+"/"+ev.SLUID+"/ended" {
		return ev, errEvent
	}
	if err := checkMetadata(j); err != nil {
		return ev, err
	}
	var usage map[string]any
	if in.Usage != nil {
		var err error
		if usage, err = checkUsage(in.Usage.GPU); err != nil {
			return ev, err
		}
	}
	ctx, err := decodeGeneric(body)
	if err != nil {
		return ev, err
	}
	if usage != nil {
		ctx["usage"] = usage
	} else {
		delete(ctx, "usage")
	}
	ev.ID, ev.ActiveSeconds, ev.Context = in.EventID, *in.Duration.ActiveSeconds, ctx
	return ev, nil
}

func validRaw(raw map[string]json.RawMessage) bool {
	if raw == nil {
		return false
	}
	if objectRaw(raw["controller"]) {
		return true
	}
	var complete *bool
	var reason *string
	c, ok := raw["controller"]
	return ok && isNull(c) && json.Unmarshal(raw["complete"], &complete) == nil && complete != nil && !*complete &&
		json.Unmarshal(raw["omitted_reason"], &reason) == nil && reason != nil && *reason != "" && jsLen(*reason) <= 200
}

func checkMetadata(j map[string]json.RawMessage) error {
	for k := range j {
		if !jobFields[k] {
			return errEvent
		}
	}
	present := func(name string) (json.RawMessage, bool) {
		v, ok := j[name]
		return v, ok && !isNull(v)
	}
	var s string
	var n int64
	for _, f := range stringFields {
		if v, ok := present(f); ok && (json.Unmarshal(v, &s) != nil || len(s) > metadataStringBytes) {
			return errMetadata
		}
	}
	for _, f := range integerFields {
		if v, ok := present(f); ok && (json.Unmarshal(v, &n) != nil || n < 0 || n > maxSafeInteger) {
			return errMetadata
		}
	}
	if v, ok := present("time_limit"); ok {
		if json.Unmarshal(v, &s) != nil || len(s) > metadataStringBytes {
			return errMetadata
		}
		if s != "" && s != "UNLIMITED" {
			m, err := strconv.ParseInt(strings.TrimSuffix(s, " min"), 10, 64)
			if !timeLimitPat.MatchString(s) || err != nil || m > maxSafeInteger {
				return errMetadata
			}
		}
	}
	for _, f := range timeFields {
		if v, ok := present(f); ok && (json.Unmarshal(v, &s) != nil || (s != "" && !validUTC(s))) {
			return errMetadata
		}
	}
	var b bool
	if v, ok := present("metadata_complete"); ok && json.Unmarshal(v, &b) != nil {
		return errMetadata
	}
	if v, ok := present("metadata_omitted"); ok {
		var list []string
		if json.Unmarshal(v, &list) != nil || list == nil || len(list) > len(metadataPaths) {
			return errMetadata
		}
		seen := map[string]bool{}
		for _, p := range list {
			if !metadataPaths[p] || seen[p] {
				return errMetadata
			}
			seen[p] = true
		}
	}
	size := 2
	for _, f := range append(slices.Clone(metadataFields), "metadata_complete", "metadata_omitted") {
		v, ok := j[f]
		if !ok {
			continue
		}
		var c bytes.Buffer
		if json.Compact(&c, v) != nil {
			return errMetadata
		}
		size += len(strconv.Quote(f)) + 2 + c.Len()
	}
	if size > metadataTotalBytes {
		return errMetadata
	}
	return nil
}

func finite(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= maxSafeInteger
}

func checkUsage(g *gpuUsage) (map[string]any, error) {
	const tol = 0.00001
	if g == nil || !finite(g.AllocatedGPUSeconds) || !finite(g.ObservedGPUSeconds) || !finite(g.UtilizationPercentSeconds) ||
		!finite(g.MemoryUtilizationPercentSeconds) || g.Complete == nil || g.MissingReasons == nil || len(*g.MissingReasons) > 32 {
		return nil, errUsage
	}
	for _, r := range *g.MissingReasons {
		if jsLen(r) > 200 || strings.ContainsAny(r, "\r\n\x00") {
			return nil, errUsage
		}
	}
	obs := *g.ObservedGPUSeconds
	if obs > *g.AllocatedGPUSeconds+tol || *g.UtilizationPercentSeconds > obs*100+tol || *g.MemoryUtilizationPercentSeconds > obs*100+tol {
		return nil, errUsage
	}
	reasons := make([]any, len(*g.MissingReasons))
	for i, r := range *g.MissingReasons {
		reasons[i] = r
	}
	out := map[string]any{
		"allocated_gpu_seconds": *g.AllocatedGPUSeconds, "observed_gpu_seconds": obs,
		"utilization_percent_seconds": *g.UtilizationPercentSeconds, "memory_utilization_percent_seconds": *g.MemoryUtilizationPercentSeconds,
		"complete": *g.Complete, "missing_reasons": reasons,
	}
	for _, m := range []struct {
		name     string
		given    *float64
		integral float64
	}{{"mean_utilization_percent", g.MeanUtilizationPercent, *g.UtilizationPercentSeconds}, {"mean_memory_utilization_percent", g.MeanMemoryUtilizationPercent, *g.MemoryUtilizationPercentSeconds}} {
		var mean any
		if obs > 0 {
			mean = m.integral / obs
		}
		if m.given != nil && (!finite(m.given) || *m.given > 100 || mean == nil || math.Abs(*m.given-mean.(float64)) > tol) {
			return nil, errUsage
		}
		out[m.name] = mean
	}
	return map[string]any{"gpu": out}, nil
}

func previewEvent() map[string]any {
	m, err := decodeGeneric([]byte(previewEventJSON))
	if err != nil {
		panic(err)
	}
	return m
}
