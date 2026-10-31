// Package resourcemonitor stores finalized per-job resource summaries pushed by slaves and serves 7-day statistics.
package resourcemonitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/statusreport"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const (
	ID          = "resource-monitor"
	SummaryPath = "/sync/v2/resource-summaries"

	maxSafeInteger = 1<<53 - 1
	clockSkew      = 5 * time.Minute
)

// States is ordered; statistics sort per-day entries by this order.
var States = []string{"COMPLETED", "CANCELLED", "FAILED", "TIMEOUT", "NODE_FAIL", "PREEMPTED", "BOOT_FAIL", "DEADLINE", "OUT_OF_MEMORY"}

var (
	sluidPattern   = regexp.MustCompile(`^s[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{13}$`)
	endedAtPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?Z$`)
	errInvalid     = apperr.BadRequest("invalid resource summary")
	errIdentity    = apperr.New(http.StatusConflict, "conflict", "resource summary identity conflict")
	shanghai       = time.FixedZone("Asia/Shanghai", 8*3600)
)

// Plugin is the resource-monitor plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	s := &service{db: h.Store, clusters: h.Clusters, now: time.Now}
	h.Router.Handle(httpx.Route{Method: http.MethodPost, Path: SummaryPath, Permission: authz.Peer(), Handler: s.ingest, Routine: true})
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/resource-monitor/stats", Permission: authz.Policy(ID, "read"), Handler: s.stats})
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/resource-monitor/my-stats", Permission: authz.Authenticated(), Handler: s.myStats})
	return nil
}

type service struct {
	db       store.Store
	clusters plugin.Clusters
	now      func() time.Time
}

type gpuInput struct {
	AllocatedGPUSeconds             *float64  `json:"allocated_gpu_seconds"`
	ObservedGPUSeconds              *float64  `json:"observed_gpu_seconds"`
	UtilizationPercentSeconds       *float64  `json:"utilization_percent_seconds"`
	MemoryUtilizationPercentSeconds *float64  `json:"memory_utilization_percent_seconds"`
	MeanUtilizationPercent          *float64  `json:"mean_utilization_percent"`
	MeanMemoryUtilizationPercent    *float64  `json:"mean_memory_utilization_percent"`
	Complete                        *bool     `json:"complete"`
	MissingReasons                  *[]string `json:"missing_reasons"`
}

// Summary is the wire format of POST /sync/v2/resource-summaries; pointers distinguish absent from zero.
type Summary struct {
	Version   int    `json:"version"`
	EventID   string `json:"event_id"`
	ClusterID string `json:"cluster_id"`
	Job       *struct {
		JobID     int64           `json:"job_id"`
		SLUID     string          `json:"sluid"`
		UID       int64           `json:"uid"`
		Name      *string         `json:"name"`
		CPUs      *int64          `json:"cpus"`
		GPUs      *int64          `json:"gpus"`
		MemoryMiB *float64        `json:"memory_mib"`
		Nodes     json.RawMessage `json:"nodes"`
	} `json:"job"`
	Result *struct {
		State string `json:"state"`
	} `json:"result"`
	Duration *struct {
		ActiveSeconds *int64 `json:"active_seconds"`
		Scope         string `json:"scope"`
	} `json:"duration"`
	EndedAt string `json:"ended_at"`
	Usage   *struct {
		GPU *gpuInput `json:"gpu"`
	} `json:"usage"`
}

type finalJob struct {
	JobID     int64   `json:"job_id"`
	SLUID     string  `json:"sluid"`
	UID       int64   `json:"uid"`
	Name      string  `json:"name"`
	CPUs      int64   `json:"cpus"`
	GPUs      int64   `json:"gpus"`
	MemoryMiB float64 `json:"memory_mib"`
	Nodes     string  `json:"nodes"`
}

type finalGPU struct {
	Allocated      float64  `json:"allocated_gpu_seconds"`
	Observed       float64  `json:"observed_gpu_seconds"`
	Util           float64  `json:"utilization_percent_seconds"`
	MemUtil        float64  `json:"memory_utilization_percent_seconds"`
	Complete       bool     `json:"complete"`
	MissingReasons []string `json:"missing_reasons"`
}

// Final is the normalized summary; its hash detects identity reuse with different content.
type Final struct {
	ClusterID     string    `json:"cluster_id"`
	Job           finalJob  `json:"job"`
	State         string    `json:"state"`
	ActiveSeconds int64     `json:"active_seconds"`
	EndedAt       time.Time `json:"ended_at"`
	EndedDay      string    `json:"ended_day"`
	GPU           finalGPU  `json:"gpu"`
}

func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

func cleanLine(s string, maxLen int) bool {
	return jsLen(s) <= maxLen && !strings.ContainsAny(s, "\r\n\x00")
}

func finite(p *float64, limit float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= limit
}

func integer(p *int64, limit int64) bool { return p != nil && *p >= 0 && *p <= limit }

func normalizeState(s string) string {
	if s == "COMPLETE" {
		return "COMPLETED"
	}
	return s
}

func day(t time.Time) string { return t.In(shanghai).Format(time.DateOnly) }

func nodes(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil && raw[0] == '"' {
		return s, jsLen(s) <= 4096
	}
	var list []string
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &list) != nil || len(list) > 1024 {
		return "", false
	}
	for _, n := range list {
		if jsLen(n) > 253 {
			return "", false
		}
	}
	return strings.Join(list, ","), true
}

// Check validates in for cluster and returns the normalized summary and its content hash.
func (in *Summary) Check(cluster string, now time.Time) (Final, string, error) {
	var f Final
	if in.Version != 1 || in.ClusterID != cluster || in.Job == nil || in.Result == nil || in.Duration == nil ||
		in.Usage == nil || in.Usage.GPU == nil || in.EventID == "" || !cleanLine(in.EventID, 200) {
		return f, "", errInvalid
	}
	j, d, g := in.Job, in.Duration, in.Usage.GPU
	state := normalizeState(in.Result.State)
	if j.JobID < 1 || j.JobID > 4294967294 || j.UID < 1 || j.UID > 4294967295 || !sluidPattern.MatchString(j.SLUID) ||
		j.Name == nil || len(*j.Name) > 4096 || !integer(j.CPUs, 1048576) || !integer(j.GPUs, 65535) ||
		!finite(j.MemoryMiB, 1099511627776) || !slices.Contains(States, state) || !integer(d.ActiveSeconds, 315576000) ||
		d.Scope != "last_attempt" {
		return f, "", errInvalid
	}
	if !endedAtPattern.MatchString(in.EndedAt) || strings.HasPrefix(in.EndedAt, "0000") {
		return f, "", errInvalid
	}
	ended, err := time.Parse(time.RFC3339Nano, in.EndedAt)
	if err != nil || ended.Before(time.Unix(0, 0)) || ended.After(now.Add(clockSkew)) {
		return f, "", errInvalid
	}
	ns, ok := nodes(j.Nodes)
	if !ok {
		return f, "", errInvalid
	}
	if !finite(g.AllocatedGPUSeconds, maxSafeInteger) || !finite(g.ObservedGPUSeconds, maxSafeInteger) ||
		!finite(g.UtilizationPercentSeconds, maxSafeInteger) || !finite(g.MemoryUtilizationPercentSeconds, maxSafeInteger) ||
		g.Complete == nil || g.MissingReasons == nil || len(*g.MissingReasons) > 32 {
		return f, "", errInvalid
	}
	for _, r := range *g.MissingReasons {
		if !cleanLine(r, 200) {
			return f, "", errInvalid
		}
	}
	allocated := float64(*j.GPUs) * float64(*d.ActiveSeconds)
	tol := max(0.000001, allocated*1e-8)
	if math.Abs(*g.AllocatedGPUSeconds-allocated) > tol || *g.ObservedGPUSeconds > allocated+tol ||
		*g.UtilizationPercentSeconds > *g.ObservedGPUSeconds*100+tol || *g.MemoryUtilizationPercentSeconds > *g.ObservedGPUSeconds*100+tol {
		return f, "", errInvalid
	}
	observed := min(*g.ObservedGPUSeconds, allocated)
	for _, m := range []struct{ mean, integral *float64 }{{g.MeanUtilizationPercent, g.UtilizationPercentSeconds}, {g.MeanMemoryUtilizationPercent, g.MemoryUtilizationPercentSeconds}} {
		if m.mean == nil {
			continue
		}
		want := 0.0
		if observed > 0 {
			want = *m.integral / observed
		}
		if !finite(m.mean, 100) || math.Abs(*m.mean-want) > 0.00001 {
			return f, "", errInvalid
		}
	}
	reasons := slices.Compact(slices.Sorted(slices.Values(*g.MissingReasons)))
	ended = ended.Truncate(time.Millisecond).UTC()
	f = Final{ClusterID: cluster,
		Job: finalJob{JobID: j.JobID, SLUID: "s" + strings.ToUpper(j.SLUID[1:]), UID: j.UID, Name: *j.Name,
			CPUs: *j.CPUs, GPUs: *j.GPUs, MemoryMiB: *j.MemoryMiB, Nodes: ns},
		State: state, ActiveSeconds: *d.ActiveSeconds, EndedAt: ended, EndedDay: day(ended),
		GPU: finalGPU{Allocated: allocated, Observed: observed, Util: *g.UtilizationPercentSeconds,
			MemUtil: *g.MemoryUtilizationPercentSeconds, Complete: *g.Complete, MissingReasons: reasons}}
	b, err := json.Marshal(f)
	if err != nil {
		return f, "", errInvalid
	}
	sum := sha256.Sum256(b)
	return f, hex.EncodeToString(sum[:]), nil
}

func (s *service) ingest(w http.ResponseWriter, r *http.Request) error {
	cl, err := statusreport.PeerCluster(r, s.clusters)
	if err != nil {
		return err
	}
	var in Summary
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	f, hash, err := in.Check(cl.ID, s.now().UTC())
	if err != nil {
		return err
	}
	outcome, err := s.store(r.Context(), f, in.EventID, hash)
	if err != nil {
		return err
	}
	// Slave senders read a bare {"outcome": ...} object, as with the v1 protocol.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(map[string]string{"outcome": outcome})
}

func (s *service) store(ctx context.Context, f Final, eventID, hash string) (string, error) {
	j, g := f.Job, f.GPU
	complete := 0
	if g.Complete {
		complete = 1
	}
	res, err := s.db.Exec(ctx, `INSERT INTO v2_resource_summaries (cluster_id, sluid, event_id, job_id, uid, state, ended_at, ended_day,
		active_seconds, cpus, gpus, memory_mib, allocated_gpu_seconds, observed_gpu_seconds, gpu_utilization_percent_seconds,
		gpu_memory_utilization_percent_seconds, complete, payload_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		f.ClusterID, j.SLUID, eventID, j.JobID, j.UID, f.State, f.EndedAt, f.EndedDay, f.ActiveSeconds, j.CPUs, j.GPUs, j.MemoryMiB,
		g.Allocated, g.Observed, g.Util, g.MemUtil, complete, hash)
	if err != nil {
		return "", err
	}
	if res.RowsAffected > 0 {
		return "stored", nil
	}
	rows, err := s.db.Query(ctx, "SELECT payload_hash FROM v2_resource_summaries WHERE cluster_id = ? AND (sluid = ? OR event_id = ?)", f.ClusterID, j.SLUID, eventID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return "", err
		}
		if h != hash {
			return "", errIdentity
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !found {
		return "", apperr.ErrUnavailable.Wrap(errors.New("resource summary insert ignored without a matching row"))
	}
	return "duplicate", nil
}
