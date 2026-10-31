// Package currentstatus publishes each cluster's live resource and node status as one private
// R2 object; browsers read it through short-lived presigned GET links, so status reads never
// reach the Worker or D1.
package currentstatus

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/clusterstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcestatus"
)

const (
	ID              = "current-status"
	IngestPath      = "/sync/v2/current-status"
	LinksPath       = "/api/v2/current-status/links"
	MaxPayloadBytes = 4 << 20
	MaxItems        = 20000
	// CollectionTimeout bounds both Slurm collectors of one round.
	CollectionTimeout = 30 * time.Second
)

// Report is the document a slave publishes; both sections share one observation time.
type Report struct {
	Version        int                    `json:"version"`
	ClusterID      string                 `json:"cluster_id"`
	ObservedAt     time.Time              `json:"observed_at"`
	ResourceStatus *resourcestatus.Report `json:"resource_status"`
	ClusterStatus  *clusterstatus.Report  `json:"cluster_status"`
}

// ResourceCollector produces the resource half of a current-status object.
type ResourceCollector interface {
	Collect(context.Context, time.Time) (resourcestatus.Report, error)
}

// ClusterCollector produces the node half of a current-status object.
type ClusterCollector interface {
	Collect(context.Context, time.Time) (clusterstatus.Report, error)
}

// Collector samples both projections without the per-plugin 2000 item truncation.
type Collector struct {
	cluster   string
	resources ResourceCollector
	nodes     ClusterCollector
}

// NewCollector builds the slave-side collector from the resource-status and cluster-status settings.
func NewCollector(cluster string, rc resourcestatus.Config, cc clusterstatus.Config) (*Collector, error) {
	limits := struct{ items, bytes int }{MaxItems, MaxPayloadBytes}
	res, err := resourcestatus.NewCollectorWithLimits(cluster, rc, nil, resourcestatus.CollectionLimits{MaxItems: limits.items, MaxPayloadBytes: limits.bytes})
	if err != nil {
		return nil, err
	}
	nodes, err := clusterstatus.NewCollectorWithLimits(cluster, cc, nil, nil, clusterstatus.CollectionLimits{MaxItems: limits.items, MaxPayloadBytes: limits.bytes})
	if err != nil {
		return nil, err
	}
	return NewCollectorWithSources(cluster, res, nodes)
}

// SetUsernames passes a UID → username lookup to the resource collector, for job owners squeue
// reports only by number.
func (c *Collector) SetUsernames(lookup func(uid int64) (string, bool)) {
	if r, ok := c.resources.(interface {
		SetUsernames(func(int64) (string, bool))
	}); ok {
		r.SetUsernames(lookup)
	}
}

// NewCollectorWithSources builds a collector from explicit sources (used by tests).
func NewCollectorWithSources(cluster string, res ResourceCollector, nodes ClusterCollector) (*Collector, error) {
	if cluster == "" || res == nil || nodes == nil {
		return nil, errors.New("current-status: cluster and both collectors are required")
	}
	return &Collector{cluster: cluster, resources: res, nodes: nodes}, nil
}

func clusterFailure(cluster string, at time.Time, prev clusterstatus.Report) clusterstatus.Report {
	reason := "collection_failed"
	ips, health := prev.CurrentIPs, prev.AddressHealth
	if prev.Address == nil || health == "" {
		ips, health = nil, "not_configured"
	}
	if ips == nil {
		ips = []string{}
	}
	return clusterstatus.Report{Version: 1, ClusterID: cluster, ObservedAt: at, Health: "error", Error: &reason,
		Address: prev.Address, CurrentIPs: ips, AddressHealth: health, Nodes: []clusterstatus.Node{}, NodeCounts: clusterstatus.EmptyCounts()}
}

// Collect samples both sections at one observation time. A failed or truncated section becomes an
// explicit error section; a sample over the payload budget becomes a bounded failure for this round.
func (c *Collector) Collect(ctx context.Context, at time.Time) (Report, error) {
	at = at.UTC().Truncate(time.Millisecond)
	bounded, cancel := context.WithTimeout(ctx, CollectionTimeout)
	defer cancel()
	type rres struct {
		r   resourcestatus.Report
		err error
	}
	type cres struct {
		r   clusterstatus.Report
		err error
	}
	rc, nc := make(chan rres, 1), make(chan cres, 1)
	go func() { r, err := c.resources.Collect(bounded, at); rc <- rres{r, err} }()
	go func() { r, err := c.nodes.Collect(bounded, at); nc <- cres{r, err} }()
	r, n := <-rc, <-nc
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if r.err != nil || r.r.Version != 1 || r.r.ClusterID != c.cluster || !r.r.ObservedAt.Equal(at) || r.r.Health != "ok" || r.r.Resources == nil || r.r.JobsTruncated || r.r.JobsTotal != len(r.r.Jobs) {
		r.r = resourcestatus.ErrorReport(c.cluster, at)
	}
	if n.r.Version != 1 || n.r.ClusterID != c.cluster || !n.r.ObservedAt.Equal(at) {
		// DNS metadata from a differently bound source must never cross the cluster boundary.
		n.r = clusterstatus.Report{}
	}
	if n.err != nil || n.r.Health != "ok" || n.r.NodesTruncated || n.r.NodesTotal != len(n.r.Nodes) {
		n.r = clusterFailure(c.cluster, at, n.r)
	}
	if r.r.Jobs == nil {
		r.r.Jobs = []resourcestatus.Job{}
	}
	if n.r.CurrentIPs == nil {
		n.r.CurrentIPs = []string{}
	}
	rep := Report{Version: 1, ClusterID: c.cluster, ObservedAt: at, ResourceStatus: &r.r, ClusterStatus: &n.r}
	if b, err := json.Marshal(rep); err != nil || len(b) > MaxPayloadBytes {
		res, nodes := resourcestatus.ErrorReport(c.cluster, at), clusterFailure(c.cluster, at, n.r)
		rep.ResourceStatus, rep.ClusterStatus = &res, &nodes
	}
	return rep, nil
}
