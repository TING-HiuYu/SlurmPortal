// Package jobnotify implements best-effort Slurm completion notifications.
// It deliberately has no database, filesystem queue, or LDAP dependency.
package jobnotify

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

const (
	CallbackPath    = "/internal/slurm/job-ended"
	CompletionPath  = "/sync/v2/job-completions"
	SlurmAPIVersion = "v0.0.45" // Slurm 26.05 data_parser, not an unversioned online schema.
	// MinActiveSeconds is an exclusive threshold for the final execution only.
	// The controller hook, slave normalizer, and Worker must all enforce >60s.
	MinActiveSeconds int64 = 60
)

var (
	sluidPattern   = regexp.MustCompile(`^[sS][0-9A-Fa-f][0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{12}$`)
	clusterPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	ErrRetry       = errors.New("completion is not yet verifiable")
	ErrFiltered    = errors.New("completion does not qualify")
	ErrSuperseded  = errors.New("completion belongs to another execution")
)

// Callback is what the Slurm hook reports about a job.
type Callback struct {
	JobID uint32 `json:"job_id"`
	SLUID string `json:"sluid"`
}

// Slurm 26.05 print_sluid emits a lower-case prefix and upper-case cb32 digits.
func canonicalSLUID(value string) string {
	if len(value) != 14 {
		return value
	}
	return "s" + strings.ToUpper(value[1:])
}

// Validate checks the callback fields.
func (c Callback) Validate() error {
	// Slurm reserves UINT32_MAX and UINT32_MAX-1 for sentinel values.
	if c.JobID == 0 || c.JobID >= ^uint32(0)-1 || !sluidPattern.MatchString(c.SLUID) {
		return errors.New("invalid completion identity")
	}
	return nil
}

type Job struct {
	JobID         uint32 `json:"job_id"`
	SLUID         string `json:"sluid"`
	UID           uint32 `json:"uid"`
	Name          string `json:"name"`
	Array         any    `json:"array"`
	Heterogeneous any    `json:"heterogeneous"`
	// Optional v1 summary fields remain available even if raw is omitted.
	// Declaration order is retention priority: resource and output details first.
	RequestedTRES    *string  `json:"requested_tres,omitempty" slurm:"tres_req_str"`
	AllocatedTRES    *string  `json:"allocated_tres,omitempty" slurm:"tres_alloc_str"`
	StdoutExpanded   *string  `json:"stdout_expanded,omitempty" slurm:"stdout_expanded"`
	StandardOutput   *string  `json:"standard_output,omitempty" slurm:"standard_output"`
	BatchHost        *string  `json:"batch_host,omitempty" slurm:"batch_host"`
	Command          *string  `json:"command,omitempty" slurm:"command"`
	WorkingDirectory *string  `json:"working_directory,omitempty" slurm:"current_working_directory"`
	SubmitLine       *string  `json:"submit_line,omitempty" slurm:"submit_line"`
	Nodes            *string  `json:"nodes,omitempty" slurm:"nodes"`
	Partition        *string  `json:"partition,omitempty" slurm:"partition"`
	Account          *string  `json:"account,omitempty" slurm:"account"`
	QOS              *string  `json:"qos,omitempty" slurm:"qos"`
	StderrExpanded   *string  `json:"stderr_expanded,omitempty" slurm:"stderr_expanded"`
	StandardError    *string  `json:"standard_error,omitempty" slurm:"standard_error"`
	StdinExpanded    *string  `json:"stdin_expanded,omitempty" slurm:"stdin_expanded"`
	StandardInput    *string  `json:"standard_input,omitempty" slurm:"standard_input"`
	CPUsPerTRES      *string  `json:"cpus_per_tres,omitempty" slurm:"cpus_per_tres"`
	MemoryPerTRES    *string  `json:"memory_per_tres,omitempty" slurm:"memory_per_tres"`
	TRESPerNode      *string  `json:"tres_per_node,omitempty" slurm:"tres_per_node"`
	UserName         *string  `json:"user_name,omitempty" slurm:"user_name"`
	GroupName        *string  `json:"group_name,omitempty" slurm:"group_name"`
	CPUs             *int64   `json:"cpus,omitempty" slurm:"cpus"`
	NodeCount        *int64   `json:"node_count,omitempty" slurm:"node_count"`
	Tasks            *int64   `json:"tasks,omitempty" slurm:"tasks"`
	CPUsPerTask      *int64   `json:"cpus_per_task,omitempty" slurm:"cpus_per_task"`
	MemoryPerNodeMiB *int64   `json:"memory_per_node_mib,omitempty" slurm:"memory_per_node"`
	MemoryPerCPUMiB  *int64   `json:"memory_per_cpu_mib,omitempty" slurm:"memory_per_cpu"`
	Priority         *int64   `json:"priority,omitempty" slurm:"priority"`
	RestartCount     *int64   `json:"restart_count,omitempty" slurm:"restart_cnt"`
	GroupID          *int64   `json:"group_id,omitempty" slurm:"group_id"`
	TimeLimit        *string  `json:"time_limit,omitempty" slurm:"time_limit" format:"minutes"`
	SubmitTime       *int64   `json:"submit_time,omitempty" slurm:"submit_time"`
	StartTime        *int64   `json:"start_time,omitempty" slurm:"start_time"`
	EndTime          *int64   `json:"end_time,omitempty" slurm:"end_time"`
	SubmittedAt      *string  `json:"submitted_at,omitempty" slurm:"submit_time" format:"timestamp"`
	StartedAt        *string  `json:"started_at,omitempty" slurm:"start_time" format:"timestamp"`
	EndedAt          *string  `json:"ended_at,omitempty" slurm:"end_time" format:"timestamp"`
	MetadataComplete *bool    `json:"metadata_complete,omitempty"`
	MetadataOmitted  []string `json:"metadata_omitted,omitempty"`
}

type Result struct {
	State    string          `json:"state"`
	ExitCode json.RawMessage `json:"exit_code,omitempty"`
}

type Duration struct {
	ActiveSeconds   int64  `json:"active_seconds"`
	Scope           string `json:"scope"`
	ExcludesSuspend bool   `json:"excludes_suspend"`
}

type Raw struct {
	Controller        json.RawMessage `json:"controller"`
	InitialController json.RawMessage `json:"initial_controller,omitempty"`
	Accounting        any             `json:"accounting"`
	Complete          bool            `json:"complete"`
	OmittedReason     string          `json:"omitted_reason,omitempty"`
}

// Event is the job-completion event sent to master.
type Event struct {
	Version    int       `json:"version"`
	EventID    string    `json:"event_id"`
	ClusterID  string    `json:"cluster_id"`
	Job        Job       `json:"job"`
	Result     Result    `json:"result"`
	Duration   Duration  `json:"duration"`
	Raw        Raw       `json:"raw"`
	Usage      any       `json:"usage,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
}
