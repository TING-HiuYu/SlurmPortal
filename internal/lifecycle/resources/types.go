// Package resources implements the compile-time resource-monitor plugin.
// Only finalized summaries are persisted by master. Live measurements stay in RAM.
package resources

import "time"

const ID = "resource-monitor"
const SummaryPath = "/sync/v2/resource-summaries"

type Job struct {
	JobID     uint32   `json:"job_id"`
	SLUID     string   `json:"sluid"`
	UID       uint32   `json:"uid"`
	Name      string   `json:"name"`
	CPUs      int64    `json:"cpus"`
	GPUs      int64    `json:"gpus"`
	MemoryMiB int64    `json:"memory_mib"`
	Nodes     []string `json:"nodes"`
}

type GPUUsage struct {
	AllocatedGPUSeconds             float64  `json:"allocated_gpu_seconds"`
	ObservedGPUSeconds              float64  `json:"observed_gpu_seconds"`
	UtilizationPercentSeconds       float64  `json:"utilization_percent_seconds"`
	MemoryUtilizationPercentSeconds float64  `json:"memory_utilization_percent_seconds"`
	MeanUtilizationPercent          *float64 `json:"mean_utilization_percent"`
	MeanMemoryUtilizationPercent    *float64 `json:"mean_memory_utilization_percent"`
	Complete                        bool     `json:"complete"`
	MissingReasons                  []string `json:"missing_reasons"`
}

type Usage struct {
	GPU GPUUsage `json:"gpu"`
}

// Summary is a finished job's resource usage, sent to master's resource-monitor plugin.
type Summary struct {
	Version   int    `json:"version"`
	EventID   string `json:"event_id"`
	ClusterID string `json:"cluster_id"`
	Job       Job    `json:"job"`
	Result    struct {
		State string `json:"state"`
	} `json:"result"`
	Duration struct {
		ActiveSeconds int64  `json:"active_seconds"`
		Scope         string `json:"scope"`
	} `json:"duration"`
	EndedAt time.Time `json:"ended_at"`
	Usage   Usage     `json:"usage"`
}

// Device identity must be stable across reordering of the backend's GPU array.
type Device struct {
	Node string `json:"node"`
	UUID string `json:"uuid"`
}

// Allocation is the nodes and GPUs a job was given.
type Allocation struct {
	Key       string    `json:"key"`
	StartedAt time.Time `json:"started_at"`
	Devices   []Device  `json:"devices"`
	GPUCount  int64     `json:"gpu_count"`
}

type DeviceSample struct {
	UUID           *string `json:"uuid"`
	Index          int     `json:"index"`
	Util           float64 `json:"util"`
	MemoryUsedMiB  float64 `json:"mem_used_mb"`
	MemoryTotalMiB float64 `json:"mem_total_mb"`
}

// NodeSample is one round of utilisation readings from a node.
type NodeSample struct {
	Hostname    string         `json:"hostname"`
	TimestampMS int64          `json:"timestamp_ms"`
	Stale       bool           `json:"stale"`
	Error       *string        `json:"error"`
	Devices     []DeviceSample `json:"gres"`
}
