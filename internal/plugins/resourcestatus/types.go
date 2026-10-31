// Package resourcestatus implements the independent, compiled-in live Slurm
// resource-status plugin. Only allowlisted scheduling metadata leaves a slave.
package resourcestatus

import "time"

const ID = "resource-status"
const SnapshotPath = "/sync/v2/resource-status"
const MaxPayloadBytes = 1 << 20
const MaxJobs = 2000

type Capacity struct {
	Total     int64  `json:"total"`
	Available int64  `json:"available"`
	Allocated *int64 `json:"allocated,omitempty"`
}

type Resources struct {
	CPU       Capacity `json:"cpu"`
	MemoryMiB Capacity `json:"memory_mib"`
	GPU       Capacity `json:"gpu"`
}

// Nil means unknown, not zero. Values are allocation/request totals, not
// measured CPU activity, resident memory or GPU utilization.
type JobResources struct {
	CPU       *int64 `json:"cpu"`
	MemoryMiB *int64 `json:"memory_mib"`
	GPU       *int64 `json:"gpu"`
}

type Job struct {
	ID               string       `json:"id"`
	Name             string       `json:"name"`
	User             string       `json:"user"`
	State            string       `json:"state"`
	Partition        string       `json:"partition"`
	Allocated        JobResources `json:"allocated"`
	Requested        JobResources `json:"requested"`
	ElapsedSeconds   *int64       `json:"elapsed_seconds"`
	TimeLimitSeconds *int64       `json:"time_limit_seconds"`
	Reason           string       `json:"reason"`
	NodeCount        *int64       `json:"node_count"`
}

// Report is a slave's resource and queue report.
type Report struct {
	Version       int        `json:"version"`
	ClusterID     string     `json:"cluster_id"`
	ObservedAt    time.Time  `json:"observed_at"`
	Health        string     `json:"health"`
	Error         *string    `json:"error"`
	Resources     *Resources `json:"resources"`
	Jobs          []Job      `json:"jobs"`
	JobsTotal     int        `json:"jobs_total"`
	JobsTruncated bool       `json:"jobs_truncated"`
}

// ErrorReport is the report sent when collection failed.
func ErrorReport(cluster string, at time.Time) Report {
	reason := "collection_failed"
	return Report{Version: 1, ClusterID: cluster, ObservedAt: at.UTC(), Health: "error", Error: &reason, Jobs: []Job{}}
}
