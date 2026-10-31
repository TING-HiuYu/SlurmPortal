// Package statusreport stores the latest per-cluster report of each kind pushed by slaves over mTLS.
package statusreport

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// MinStale is the floor for marking a cluster's report stale.
const MinStale = 90 * time.Second

// Row is the stored report of one kind for one cluster.
type Row struct {
	Attempted time.Time
	Received  time.Time
	Health    string
	Observed  *time.Time
	Meta      string
	Payload   *string
}

// Measured is the most conservative time the report describes.
func (r Row) Measured() time.Time {
	m := r.Attempted
	if r.Received.Before(m) {
		m = r.Received
	}
	if r.Observed != nil && r.Observed.Before(m) {
		m = *r.Observed
	}
	return m
}

// Store reads and writes the latest report of one Kind per cluster.
type Store struct {
	DB   store.Store
	Kind string
}

// Put keeps only the newest attempt per cluster; it reports whether this one won.
func (s Store) Put(ctx context.Context, cluster string, attempted, received time.Time, health string, observed *time.Time, meta string, payload *string) (bool, error) {
	var obs any
	if observed != nil {
		obs = observed.UTC()
	}
	var pl any
	if payload != nil {
		pl = *payload
	}
	res, err := s.DB.Exec(ctx, `INSERT INTO v2_cluster_reports (kind, cluster_id, attempted_ms, received_at, health, metadata_json, observed_at, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(kind, cluster_id) DO UPDATE SET attempted_ms = excluded.attempted_ms, received_at = excluded.received_at,
		health = excluded.health, metadata_json = excluded.metadata_json, observed_at = excluded.observed_at, payload_json = excluded.payload_json
		WHERE excluded.attempted_ms > v2_cluster_reports.attempted_ms`,
		s.Kind, cluster, attempted.UnixMilli(), received.UTC(), health, meta, obs, pl)
	if err != nil {
		return false, err
	}
	return res.RowsAffected > 0, nil
}

// All returns the latest report of this kind for every cluster.
func (s Store) All(ctx context.Context) (map[string]Row, error) {
	rows, err := s.DB.Query(ctx, "SELECT cluster_id, attempted_ms, received_at, health, metadata_json, observed_at, payload_json FROM v2_cluster_reports WHERE kind = ?", s.Kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Row{}
	for rows.Next() {
		var id, received string
		var ms int64
		var observed *string
		var r Row
		if err := rows.Scan(&id, &ms, &received, &r.Health, &r.Meta, &observed, &r.Payload); err != nil {
			return nil, err
		}
		r.Attempted = time.UnixMilli(ms).UTC()
		r.Received, _ = time.Parse(time.RFC3339Nano, received)
		if observed != nil {
			if t, err := time.Parse(time.RFC3339Nano, *observed); err == nil {
				r.Observed = &t
			}
		}
		out[id] = r
	}
	return out, rows.Err()
}

// PeerCluster resolves the reporting cluster from the verified client certificate.
func PeerCluster(r *http.Request, cs plugin.Clusters) (plugin.Cluster, error) {
	c, ok := cs.FromPeer(authn.FromContext(r.Context()))
	if !ok {
		return c, apperr.New(http.StatusForbidden, "unknown_cluster", "client certificate is not bound to a cluster")
	}
	httpx.SetTarget(r.Context(), "cluster:"+c.ID)
	return c, nil
}

// Select applies the optional ?clusters=a,b filter.
func Select(r *http.Request, cs plugin.Clusters) (plugin.Clusters, error) {
	q := r.URL.Query()
	for k, v := range q {
		if k != "clusters" || len(v) > 1 {
			return nil, apperr.BadRequest("invalid cluster filter")
		}
	}
	v := q.Get("clusters")
	if v == "" {
		return cs, nil
	}
	var out plugin.Clusters
	for _, id := range strings.Split(v, ",") {
		c, ok := cs.Get(id)
		if !ok {
			return nil, apperr.BadRequest("invalid cluster filter")
		}
		if !slices.ContainsFunc(out, func(x plugin.Cluster) bool { return x.ID == id }) {
			out = append(out, c)
		}
	}
	return out, nil
}
