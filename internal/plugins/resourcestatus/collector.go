package resourcestatus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxCommandBytes = 16 << 20
const maximumResource = int64(1 << 40)

// Config contains slave-local command identities only, never remote arguments.
// It defaults off so adding the compiled plugin cannot invoke Slurm commands.
type Config struct {
	Enabled        bool   `yaml:"enabled"`
	ScontrolPath   string `yaml:"scontrol-path"`
	SqueuePath     string `yaml:"squeue-path"`
	SlurmConf      string `yaml:"slurm-conf"`
	CommandTimeout string `yaml:"command-timeout"`
}

// WithDefaults fills unset paths and timeouts.
func (c Config) WithDefaults() (Config, error) {
	if c.ScontrolPath == "" {
		c.ScontrolPath = "/usr/bin/scontrol"
	}
	if c.SqueuePath == "" {
		c.SqueuePath = "/usr/bin/squeue"
	}
	if c.CommandTimeout == "" {
		c.CommandTimeout = "5s"
	}
	for _, path := range []string{c.ScontrolPath, c.SqueuePath} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return c, errors.New("status commands must be absolute local paths")
		}
	}
	if c.SlurmConf != "" && (!filepath.IsAbs(c.SlurmConf) || filepath.Clean(c.SlurmConf) != c.SlurmConf || strings.ContainsAny(c.SlurmConf, "\x00\r\n")) {
		return c, errors.New("status Slurm config must be an absolute local path")
	}
	d, err := time.ParseDuration(c.CommandTimeout)
	if err != nil || d < time.Millisecond || d > 20*time.Second {
		return c, errors.New("invalid status command timeout")
	}
	return c, nil
}

// Runner executes a command (squeue, scontrol) and returns its output.
type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type commandRunner struct{ env []string }
type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxCommandBytes-b.Len() {
		return 0, errors.New("status command output exceeds budget")
	}
	return b.Buffer.Write(p)
}
func (r commandRunner) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append([]string{}, r.env...)
	cmd.WaitDelay = 200 * time.Millisecond
	var output boundedBuffer
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("status command failed")
	}
	return output.Bytes(), nil
}

// Collector builds a resource and queue Report on the slave.
type Collector struct {
	cluster string
	cfg     Config
	runner  Runner
	limits  CollectionLimits
	// usernames names a job's numeric user when squeue could not (no NSS for the directory, e.g. in
	// a container); nil leaves the number.
	usernames func(uid int64) (string, bool)
}

// SetUsernames installs the lookup used for jobs whose owner squeue reports only by UID.
func (c *Collector) SetUsernames(lookup func(uid int64) (string, bool)) { c.usernames = lookup }

// CollectionLimits are trusted transport bounds, never remotely supplied
// configuration. Legacy senders keep their existing row and byte budgets.
type CollectionLimits struct {
	MaxItems        int
	MaxPayloadBytes int
}

// NewCollector returns a Collector with default limits.
func NewCollector(cluster string, cfg Config, runner Runner) (*Collector, error) {
	return NewCollectorWithLimits(cluster, cfg, runner, CollectionLimits{MaxItems: MaxJobs, MaxPayloadBytes: MaxPayloadBytes})
}

// NewCollectorWithLimits returns a Collector with explicit size limits.
func NewCollectorWithLimits(cluster string, cfg Config, runner Runner, limits CollectionLimits) (*Collector, error) {
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`).MatchString(cluster) {
		return nil, errors.New("invalid status cluster")
	}
	if limits.MaxItems < 1 || limits.MaxItems > 20000 || limits.MaxPayloadBytes < 4096 || limits.MaxPayloadBytes > 4<<20 {
		return nil, errors.New("invalid status transport limits")
	}
	cfg, err := cfg.WithDefaults()
	if err != nil {
		return nil, err
	}
	if runner == nil {
		env := []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
		if cfg.SlurmConf != "" {
			env = append(env, "SLURM_CONF="+cfg.SlurmConf)
		}
		runner = commandRunner{env: env}
	}
	return &Collector{cluster: cluster, cfg: cfg, runner: runner, limits: limits}, nil
}

func (c *Collector) run(ctx context.Context, path string, args ...string) ([]byte, error) {
	timeout, _ := time.ParseDuration(c.cfg.CommandTimeout)
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := c.runner.Run(commandCtx, path, args...)
	if err != nil || len(body) > maxCommandBytes || commandCtx.Err() != nil {
		return nil, errors.New("status collection failed")
	}
	return body, nil
}

// Collect reads partitions, capacity and the job queue and returns one Report.
func (c *Collector) Collect(ctx context.Context, at time.Time) (Report, error) {
	nodes, err := c.run(ctx, c.cfg.ScontrolPath, "--oneliner", "show", "nodes")
	if err != nil {
		return Report{}, err
	}
	resources, err := parseNodesWithLimit(nodes, max(10000, c.limits.MaxItems))
	if err != nil {
		return Report{}, err
	}
	queue, err := c.run(ctx, c.cfg.SqueuePath, "--json", "--all", "--local")
	if err != nil {
		return Report{}, err
	}
	jobs, total, err := parseJobsWithLimit(queue, at, c.limits.MaxItems)
	if err == nil && c.usernames != nil {
		for i := range jobs {
			if uid, perr := strconv.ParseInt(jobs[i].User, 10, 64); perr == nil {
				if name, ok := c.usernames(uid); ok {
					jobs[i].User = name
				}
			}
		}
	}
	if err != nil {
		return Report{}, err
	}
	report := Report{Version: 1, ClusterID: c.cluster, ObservedAt: at.UTC(), Health: "ok", Resources: &resources, Jobs: jobs, JobsTotal: total, JobsTruncated: len(jobs) != total}
	// Account each already-projected job once rather than repeatedly encoding
	// a large queue while trimming to the transport budget.
	report.Jobs = []Job{}
	header, err := json.Marshal(report)
	if err != nil || len(header) > c.limits.MaxPayloadBytes {
		return Report{}, errors.New("invalid status report")
	}
	remaining := c.limits.MaxPayloadBytes - len(header)
	for _, job := range jobs {
		body, err := json.Marshal(job)
		if err != nil {
			return Report{}, errors.New("invalid status job")
		}
		cost := len(body)
		if len(report.Jobs) > 0 {
			cost++
		}
		if cost > remaining {
			report.JobsTruncated = true
			break
		}
		report.Jobs = append(report.Jobs, job)
		remaining -= cost
	}
	return report, nil
}

func integer(value string) (int64, error) {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 || n > maximumResource {
		return 0, errors.New("invalid status resource count")
	}
	return n, nil
}
func safeAdd(a, b int64) (int64, error) {
	if b < 0 || a > maximumResource-b {
		return 0, errors.New("status resource total exceeds budget")
	}
	return a + b, nil
}
func schedulable(state string) bool {
	// Allow known scheduling base states only. Flags indicating unhealthy,
	// drained or powered-off nodes must never inflate the available capacity.
	parts := strings.Split(strings.ToUpper(state), "+")
	if len(parts) == 0 || (parts[0] != "IDLE" && parts[0] != "MIXED" && parts[0] != "ALLOCATED") {
		return false
	}
	for _, flag := range parts[1:] {
		if flag != "CLOUD" && flag != "DYNAMIC_NORM" {
			return false
		}
	}
	return true
}

func parseNodes(body []byte) (Resources, error) {
	return parseNodesWithLimit(body, 10000)
}

func parseNodesWithLimit(body []byte, maximum int) (Resources, error) {
	var result Resources
	result.CPU.Allocated, result.MemoryMiB.Allocated, result.GPU.Allocated = ptr(0), ptr(0), ptr(0)
	seen := map[string]bool{}
	lines := bytes.Split(bytes.TrimSpace(body), []byte("\n"))
	if len(lines) > maximum {
		return result, errors.New("status node capacity exceeded")
	}
	for _, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		fields := map[string]string{}
		for _, token := range strings.Fields(string(line)) {
			key, value, ok := strings.Cut(token, "=")
			if !ok {
				continue
			}
			if _, exists := fields[key]; exists {
				return Resources{}, errors.New("ambiguous status node field")
			}
			fields[key] = value
		}
		name, ok := fields["NodeName"]
		if !ok || name == "" || seen[name] {
			return Resources{}, errors.New("invalid status node identity")
		}
		seen[name] = true
		cpu, e1 := integer(fields["CPUTot"])
		allocCPU, e2 := integer(fields["CPUAlloc"])
		memory, e3 := integer(fields["RealMemory"])
		allocMemory, e4 := integer(fields["AllocMem"])
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || fields["State"] == "" {
			return Resources{}, errors.New("missing status node resources")
		}
		effective := cpu
		if raw, ok := fields["CPUEfctv"]; ok {
			effective, e1 = integer(raw)
			if e1 != nil || effective > cpu {
				return Resources{}, errors.New("invalid effective CPU count")
			}
		}
		gpu, err := nodeGPU(fields, false)
		if err != nil {
			return Resources{}, err
		}
		allocGPU, err := nodeGPU(fields, true)
		if err != nil {
			return Resources{}, err
		}
		for _, pair := range []struct {
			capacity                    *Capacity
			total, available, allocated int64
		}{{&result.CPU, cpu, effective - allocCPU, allocCPU}, {&result.MemoryMiB, memory, memory - allocMemory, allocMemory}, {&result.GPU, gpu, gpu - allocGPU, allocGPU}} {
			pair.capacity.Total, err = safeAdd(pair.capacity.Total, pair.total)
			if err != nil {
				return Resources{}, err
			}
			*pair.capacity.Allocated, err = safeAdd(*pair.capacity.Allocated, pair.allocated)
			if err != nil {
				return Resources{}, err
			}
			if schedulable(fields["State"]) && pair.available > 0 {
				pair.capacity.Available, err = safeAdd(pair.capacity.Available, pair.available)
				if err != nil {
					return Resources{}, err
				}
			}
		}
	}
	// An empty controller response is not evidence that the cluster has zero
	// capacity. Preserve the master's last successful snapshot instead.
	if len(seen) == 0 {
		return Resources{}, errors.New("empty status node response")
	}
	return result, nil
}

func gpuTRES(value string) (int64, error) {
	var total int64
	aggregate := false
	var whole int64
	for _, token := range strings.Split(value, ",") {
		key, count, ok := strings.Cut(token, "=")
		if !ok || (key != "gres/gpu" && !strings.HasPrefix(key, "gres/gpu:")) {
			continue
		}
		n, err := integer(count)
		if err != nil {
			return 0, err
		}
		if key == "gres/gpu" {
			if aggregate {
				return 0, errors.New("duplicate GPU TRES")
			}
			aggregate = true
			whole = n
		} else {
			total, err = safeAdd(total, n)
			if err != nil {
				return 0, err
			}
		}
	}
	if aggregate {
		return whole, nil
	}
	return total, nil
}

func containsGPUTRES(value string) bool {
	for _, token := range strings.Split(value, ",") {
		key, _, ok := strings.Cut(token, "=")
		if ok && (key == "gres/gpu" || strings.HasPrefix(key, "gres/gpu:")) {
			return true
		}
	}
	return false
}
func nodeGPU(fields map[string]string, allocated bool) (int64, error) {
	key := "CfgTRES"
	if allocated {
		key = "AllocTRES"
	}
	if raw, ok := fields[key]; ok && containsGPUTRES(raw) {
		return gpuTRES(raw)
	}
	if allocated {
		total, err := nodeGPU(fields, false)
		// With GPU configured in CfgTRES, a present AllocTRES which omits
		// GPU is Slurm's explicit zero allocation. Without tracking proof,
		// a GRES-only GPU node must not be advertised as entirely idle.
		_, present := fields["AllocTRES"]
		tracked := containsGPUTRES(fields["CfgTRES"])
		if err != nil || (total > 0 && (!present || !tracked)) {
			return 0, errors.New("missing allocated GPU count")
		}
		return 0, nil
	}
	raw, ok := fields["Gres"]
	if !ok {
		return 0, errors.New("missing configured GPU count")
	}
	var total int64
	for _, token := range strings.Split(raw, ",") {
		if !strings.HasPrefix(token, "gpu:") {
			continue
		}
		name := strings.SplitN(token, "(", 2)[0]
		parts := strings.Split(name, ":")
		if len(parts) < 2 || len(parts) > 3 {
			return 0, errors.New("invalid GPU GRES")
		}
		n, err := integer(parts[len(parts)-1])
		if err != nil {
			return 0, err
		}
		total, err = safeAdd(total, n)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func safeText(value string, maximum int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
func jsonText(raw json.RawMessage) string {
	var text string
	_ = json.Unmarshal(raw, &text)
	return text
}
func ptr(n int64) *int64 { return &n }
func jsonCount(raw json.RawMessage) *int64 {
	n, ok := slurmNumber(raw)
	if !ok || n < 0 || n > maximumResource {
		return nil
	}
	return ptr(n)
}
func memoryMiB(value string) *int64 {
	if value == "" {
		return nil
	}
	multiplier := int64(1)
	last := value[len(value)-1]
	if last < '0' || last > '9' {
		switch last {
		case 'K', 'k':
			multiplier = 0
		case 'M', 'm':
			multiplier = 1
		case 'G', 'g':
			multiplier = 1024
		case 'T', 't':
			multiplier = 1024 * 1024
		default:
			return nil
		}
		value = value[:len(value)-1]
	}
	// Slurm TRES memory can be fractional (e.g. 1.5G). Round down at the
	// MiB boundary; unknown/unlimited values remain null.
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 {
		return nil
	}
	if multiplier == 0 {
		n /= 1024
	} else {
		n *= float64(multiplier)
	}
	if n > float64(maximumResource) || n != n {
		return nil
	}
	return ptr(int64(n))
}
func jobTRES(value string) JobResources {
	result := JobResources{}
	if value == "" {
		return result
	}
	for _, token := range strings.Split(value, ",") {
		key, count, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		switch key {
		case "cpu":
			if n, err := integer(count); err == nil {
				result.CPU = ptr(n)
			}
		case "mem":
			result.MemoryMiB = memoryMiB(count)
		}
	}
	if n, err := gpuTRES(value); err == nil && containsGPUTRES(value) {
		result.GPU = ptr(n)
	}
	return result
}

// jobElapsedSeconds follows the Slurm 26.05 RunTime formula and tuple
// validation in jobnotify/runtime.go. The live SUSPENDED case uses only the
// accumulated pre-suspension runtime; an unproven tuple stays unknown.
func jobElapsedSeconds(row map[string]json.RawMessage, states []string, at time.Time) *int64 {
	running, suspended := false, false
	for _, state := range states {
		running = running || state == "RUNNING"
		suspended = suspended || state == "SUSPENDED"
	}
	if running == suspended {
		return nil
	}
	start, suspend, pre := jsonCount(row["start_time"]), jsonCount(row["suspend_time"]), jsonCount(row["pre_sus_time"])
	end := at.Unix()
	if start == nil || suspend == nil || pre == nil || *start <= 0 || end < *start || end-*start > (1<<53)-1 {
		return nil
	}
	if *suspend != 0 {
		if *suspend < *start || *suspend > end || *pre > *suspend-*start {
			return nil
		}
		if suspended {
			return ptr(*pre)
		}
		return ptr(end - *suspend + *pre)
	}
	if suspended {
		return nil
	}
	return ptr(end - *start)
}

func parseJobs(body []byte, at time.Time) ([]Job, int, error) {
	return parseJobsWithLimit(body, at, MaxJobs)
}

func parseJobsWithLimit(body []byte, at time.Time, maximum int) ([]Job, int, error) {
	var response struct {
		Jobs   []map[string]json.RawMessage `json:"jobs"`
		Errors []json.RawMessage            `json:"errors"`
	}
	if json.Unmarshal(body, &response) != nil || response.Jobs == nil || len(response.Errors) > 0 || len(response.Jobs) > 100000 {
		return nil, 0, errors.New("invalid status queue response")
	}
	jobs := make([]Job, 0, min(len(response.Jobs), maximum))
	seen := map[string]bool{}
	for _, row := range response.Jobs {
		id := jsonCount(row["job_id"])
		if id == nil || *id < 1 || *id >= 4294967294 {
			return nil, 0, errors.New("invalid status job identity")
		}
		identity := strconv.FormatInt(*id, 10)
		if seen[identity] {
			return nil, 0, errors.New("ambiguous status job identity")
		}
		seen[identity] = true
		if len(jobs) >= maximum {
			continue
		}
		var states []string
		if json.Unmarshal(row["job_state"], &states) != nil || len(states) == 0 {
			return nil, 0, errors.New("missing status job state")
		}
		state := safeText(strings.Join(states, "+"), 48)
		job := Job{ID: identity, Name: safeText(jsonText(row["name"]), 256), User: safeText(jsonText(row["user_name"]), 128), State: state, Partition: safeText(jsonText(row["partition"]), 128), Reason: safeText(jsonText(row["state_reason"]), 256), NodeCount: jsonCount(row["node_count"]), Allocated: jobTRES(jsonText(row["tres_alloc_str"])), Requested: jobTRES(jsonText(row["tres_req_str"]))}
		if job.User == "" {
			if uid := jsonCount(row["user_id"]); uid != nil {
				job.User = strconv.FormatInt(*uid, 10)
			}
		}
		pending := false
		for _, s := range states {
			if s == "PENDING" {
				pending = true
			}
		}
		if pending {
			job.ElapsedSeconds = ptr(0)
			job.Allocated = JobResources{CPU: ptr(0), MemoryMiB: ptr(0), GPU: ptr(0)}
		} else {
			job.ElapsedSeconds = jobElapsedSeconds(row, states, at)
		}
		if limit := jsonCount(row["time_limit"]); limit != nil && *limit < 4294967294 && *limit <= maximumResource/60 {
			job.TimeLimitSeconds = ptr(*limit * 60)
		}
		jobs = append(jobs, job)
	}
	return jobs, len(response.Jobs), nil
}
