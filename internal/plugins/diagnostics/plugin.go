// Package diagnostics reports dependency health from evidence master already holds: a database round
// trip, each cluster's latest authenticated status reports, gateway registrations, mail providers and
// recent delivery failures. It never contacts a slave or gateway and never sends mail, so reading it
// cannot disturb production; a check that has no evidence says "missing" rather than guessing.
package diagnostics

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/currentstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const ID = "diagnostics"

// MasterConfig sets how old evidence may be before it counts as stale.
type MasterConfig struct {
	StaleSeconds int `yaml:"stale-seconds"`
}

// Plugin is the diagnostics plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	cfg := MasterConfig{StaleSeconds: 180}
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	if cfg.StaleSeconds < 60 || cfg.StaleSeconds > 3600 {
		return errors.New("diagnostics: stale-seconds must be within [60, 3600]")
	}
	s := &service{db: h.Store, clusters: h.Clusters, services: h.Services, stale: time.Duration(cfg.StaleSeconds) * time.Second, now: time.Now}
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/diagnostics", Permission: authz.Policy(ID, "read"), Handler: s.report})
	return nil
}

type service struct {
	db       store.Store
	clusters plugin.Clusters
	services *plugin.Services
	stale    time.Duration
	now      func() time.Time
}

// Check is one dependency observation.
type Check struct {
	ID         string     `json:"id"`
	Component  string     `json:"component"`
	ClusterID  string     `json:"clusterId,omitempty"`
	Status     string     `json:"status"` // ok, stale, missing, error, disabled
	Detail     string     `json:"detail"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	LatencyMs  *int64     `json:"latencyMs,omitempty"`
}

func (s *service) report(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	now := s.now().UTC()
	checks := []Check{s.database(ctx)}
	snapshots, _ := s.services.Lookup(currentstatus.ServiceName)
	reader, _ := snapshots.(currentstatus.Reader)
	for _, c := range s.clusters {
		if reader != nil {
			checks = append(checks, s.snapshotChecks(ctx, reader, c.ID, now)...)
			continue
		}
		for _, kind := range []string{"cluster-status", "resource-status"} {
			checks = append(checks, s.statusReport(ctx, c.ID, kind, now))
		}
	}
	checks = append(checks, s.gateways(ctx, now)...)
	checks = append(checks, s.mail(ctx)...)
	checks = append(checks, s.deliveries(ctx, now))
	type cluster struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	clusters := []cluster{}
	for _, c := range s.clusters {
		clusters = append(clusters, cluster{c.ID, cmp.Or(c.Name, c.ID)})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"checkedAt": now, "staleAfterSeconds": int(s.stale.Seconds()), "clusters": clusters, "checks": checks})
	return nil
}

func (s *service) database(ctx context.Context) Check {
	c := Check{ID: "master:database", Component: "database", Status: "ok", Detail: "read_ok"}
	start := s.now()
	var n int64
	if err := store.QueryRow(ctx, s.db, "SELECT COUNT(*) FROM v2_role_permissions", nil, &n); err != nil {
		c.Status, c.Detail = "error", "read_failed"
	}
	ms := s.now().Sub(start).Milliseconds()
	c.LatencyMs = &ms
	return c
}

func parseTime(v string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil
	}
	return &t
}

// statusReport judges the latest report master accepted from a slave's mTLS identity.
func (s *service) statusReport(ctx context.Context, cluster, kind string, now time.Time) Check {
	c := Check{ID: "slave:" + cluster + ":" + kind, Component: kind, ClusterID: cluster}
	var received, health string
	err := store.QueryRow(ctx, s.db, "SELECT received_at, health FROM v2_cluster_reports WHERE kind = ? AND cluster_id = ?", []any{kind, cluster}, &received, &health)
	switch {
	case errors.Is(err, store.ErrNoRows):
		c.Status, c.Detail = "missing", "never_reported"
		return c
	case err != nil:
		c.Status, c.Detail = "error", "read_failed"
		return c
	}
	c.ObservedAt = parseTime(received)
	switch {
	case c.ObservedAt == nil || now.Sub(*c.ObservedAt) > s.stale:
		c.Status, c.Detail = "stale", "report_stale"
	case health != "ok":
		c.Status, c.Detail = "error", "slave_collection_failed"
	default:
		c.Status, c.Detail = "ok", "report_fresh"
	}
	return c
}

// snapshotChecks judges a cluster's node and queue reports from its current-status snapshot, where
// slaves publish them when current-status is enabled (the per-kind D1 reports are then not sent).
// One read covers both checks.
func (s *service) snapshotChecks(ctx context.Context, r currentstatus.Reader, cluster string, now time.Time) []Check {
	snap, found, err := r.Latest(ctx, cluster)
	out := make([]Check, 0, 2)
	for _, kind := range []string{"cluster-status", "resource-status"} {
		c := Check{ID: "slave:" + cluster + ":" + kind, Component: kind, ClusterID: cluster}
		health := snap.ClusterHealth
		if kind == "resource-status" {
			health = snap.ResourceHealth
		}
		switch {
		case err != nil:
			c.Status, c.Detail = "error", "read_failed"
		case !found || health == "":
			c.Status, c.Detail = "missing", "never_reported"
		default:
			at := snap.ObservedAt
			c.ObservedAt = &at
			switch {
			case now.Sub(at) > s.stale:
				c.Status, c.Detail = "stale", "report_stale"
			case health != "ok":
				c.Status, c.Detail = "error", "slave_collection_failed"
			default:
				c.Status, c.Detail = "ok", "report_fresh"
			}
		}
		out = append(out, c)
	}
	return out
}

// gateways reports each registered gateway's last registration (gateways register every minute).
func (s *service) gateways(ctx context.Context, now time.Time) []Check {
	rows, err := s.db.Query(ctx, "SELECT gateway_id, cluster_id, registered_ms FROM v2_gateway_registrations ORDER BY gateway_id")
	if err != nil {
		return []Check{{ID: "gateway", Component: "gateway", Status: "error", Detail: "read_failed"}}
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		var id, cluster string
		var ms int64
		if err := rows.Scan(&id, &cluster, &ms); err != nil {
			return append(out, Check{ID: "gateway", Component: "gateway", Status: "error", Detail: "read_failed"})
		}
		at := time.UnixMilli(ms).UTC()
		c := Check{ID: "gateway:" + id, Component: "gateway", ClusterID: cluster, ObservedAt: &at, Status: "ok", Detail: "registration_fresh"}
		if now.Sub(at) > s.stale {
			c.Status, c.Detail = "stale", "registration_stale"
		}
		out = append(out, c)
	}
	return out
}

// mail reports each mail hub provider and whether job notification can reach its provider.
func (s *service) mail(ctx context.Context) []Check {
	rows, err := s.db.Query(ctx, "SELECT name, kind, enabled, is_default, secret_sealed FROM v2_mail_providers ORDER BY name")
	if err != nil {
		return []Check{{ID: "master:mail", Component: "mail", Status: "error", Detail: "read_failed"}}
	}
	defer rows.Close()
	var out []Check
	usable := map[string]bool{}
	def := ""
	for rows.Next() {
		var name, kind, sealed string
		var enabled, isDefault int64
		if err := rows.Scan(&name, &kind, &enabled, &isDefault, &sealed); err != nil {
			return []Check{{ID: "master:mail", Component: "mail", Status: "error", Detail: "read_failed"}}
		}
		c := Check{ID: "master:mail:" + name, Component: "mail", Status: "ok", Detail: kind + "_configured_not_probed"}
		switch {
		case enabled != 1:
			c.Status, c.Detail = "disabled", "provider_disabled"
		case kind == "smtp" && sealed == "":
			c.Status, c.Detail = "error", "password_missing"
		default:
			usable[name] = true
		}
		if isDefault == 1 {
			def = name
		}
		out = append(out, c)
	}
	if rows.Err() != nil {
		return []Check{{ID: "master:mail", Component: "mail", Status: "error", Detail: "read_failed"}}
	}
	n := Check{ID: "master:mail", Component: "mail", Status: "disabled", Detail: "notification_disabled"}
	var raw string
	err = store.QueryRow(ctx, s.db, "SELECT config_json FROM v2_notification_config WHERE id = 1", nil, &raw)
	var cfg struct {
		Enabled  bool   `json:"enabled"`
		Provider string `json:"provider"`
	}
	switch {
	case errors.Is(err, store.ErrNoRows):
	case err != nil:
		n.Status, n.Detail = "error", "read_failed"
	case json.Unmarshal([]byte(raw), &cfg) != nil:
		n.Status, n.Detail = "error", "config_invalid"
	case !cfg.Enabled:
	case !usable[cmp.Or(cfg.Provider, def)]:
		n.Status, n.Detail = "error", "notification_provider_unavailable"
	default:
		n.Status, n.Detail = "ok", "notification_provider_ready"
	}
	return append([]Check{n}, out...)
}

// deliveries surfaces mail failures and stuck sends from the last 24 hours.
func (s *service) deliveries(ctx context.Context, now time.Time) Check {
	c := Check{ID: "master:mail-delivery", Component: "mail-delivery", Status: "ok", Detail: "no_recent_failures"}
	since := now.Add(-24 * time.Hour).Format(time.RFC3339Nano)
	stuck := now.Add(-5 * time.Minute).Format(time.RFC3339Nano)
	var failed, pending int64
	err := store.QueryRow(ctx, s.db, `SELECT
		(SELECT COUNT(*) FROM v2_job_notifications WHERE status = 'failed' AND updated_at > ?),
		(SELECT COUNT(*) FROM v2_job_notifications WHERE status = 'sending' AND updated_at < ?) +
		(SELECT COUNT(*) FROM v2_transfer_events WHERE status = 'sending' AND updated_at < ?)`, []any{since, stuck, stuck}, &failed, &pending)
	switch {
	case err != nil:
		c.Status, c.Detail = "error", "read_failed"
	case failed > 0:
		c.Status, c.Detail = "error", "recent_failures"
	case pending > 0:
		c.Status, c.Detail = "stale", "sends_awaiting_retry"
	}
	return c
}
