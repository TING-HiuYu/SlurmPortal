// Package clusterstatus implements an independent compile-time cluster-status
// plugin. It exposes configured access metadata and safe Slurm node states.
package clusterstatus

import "time"

const ID = "cluster-status"
const SnapshotPath = "/sync/v2/cluster-status"
const MaxPayloadBytes = 1 << 20
const MaxNodes = 2000

var Statuses = []string{"idle", "full", "running", "offline", "drain", "maintenance", "unknown"}

// Node is one compute node's state.
type Node struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	SlurmState string `json:"slurm_state"`
}

// Report is a slave's node status report.
type Report struct {
	Version        int            `json:"version"`
	ClusterID      string         `json:"cluster_id"`
	ObservedAt     time.Time      `json:"observed_at"`
	Health         string         `json:"health"`
	Error          *string        `json:"error"`
	Address        *string        `json:"address"`
	CurrentIPs     []string       `json:"current_ips"`
	AddressHealth  string         `json:"address_health"`
	Nodes          []Node         `json:"nodes"`
	NodeCounts     map[string]int `json:"node_counts"`
	NodesTotal     int            `json:"nodes_total"`
	NodesTruncated bool           `json:"nodes_truncated"`
}

func EmptyCounts() map[string]int {
	out := map[string]int{}
	for _, status := range Statuses {
		out[status] = 0
	}
	return out
}
