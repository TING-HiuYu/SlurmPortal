// Package legacyimport copies v1 data that v2 keeps in its own tables (web access settings, the
// notification transport and template, GPU job summaries) when a v2 master first starts on a v1
// database. Each step runs only while its legacy table exists and its v2 table is still empty, so
// it is idempotent and a no-op on fresh installs. Users, groups, roles, CAs, certificates and OAuth
// connectors need no import: v2 reads those legacy tables directly.
package legacyimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Report lists what one run imported.
type Report struct {
	WebAccess    bool
	Notification bool
	Summaries    int64
}

func (r Report) Empty() bool { return !r.WebAccess && !r.Notification && r.Summaries == 0 }

func exists(ctx context.Context, db store.Querier, table string) (bool, error) {
	var n int64
	err := store.QueryRow(ctx, db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", []any{table}, &n)
	return n > 0, err
}

// empty reads at most one row: D1 bills rows read, and COUNT(*) would scan the whole table on
// every Worker start.
func empty(ctx context.Context, db store.Querier, table string) (bool, error) {
	var n int64
	err := store.QueryRow(ctx, db, "SELECT EXISTS(SELECT 1 FROM "+table+")", nil, &n)
	return n == 0, err
}

// pending reports whether legacy has data to move into the still-empty v2 table. The v2 table is
// checked first: once imported it is never empty, so later starts skip the sqlite_master scan.
func pending(ctx context.Context, db store.Querier, legacy, v2 string) (bool, error) {
	if ok, err := empty(ctx, db, v2); err != nil || !ok {
		return false, err
	}
	return exists(ctx, db, legacy)
}

func Run(ctx context.Context, db store.Store, now time.Time) (Report, error) {
	var r Report
	var err error
	if r.WebAccess, err = webAccess(ctx, db, now); err != nil {
		return r, fmt.Errorf("web access settings: %w", err)
	}
	if r.Notification, err = notification(ctx, db, now); err != nil {
		return r, fmt.Errorf("notification settings: %w", err)
	}
	if r.Summaries, err = summaries(ctx, db); err != nil {
		return r, fmt.Errorf("resource summaries: %w", err)
	}
	return r, nil
}

func legacyJSON(ctx context.Context, db store.Querier, table string) (string, error) {
	var raw string
	err := store.QueryRow(ctx, db, "SELECT config_json FROM "+table+" ORDER BY id LIMIT 1", nil, &raw)
	if errors.Is(err, store.ErrNoRows) {
		return "", nil
	}
	return raw, err
}

type v1Cluster struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Address      string   `json:"address"`
	SSHHost      string   `json:"ssh_host"`
	SSHPort      int      `json:"ssh_port"`
	SSHLocalPort int      `json:"ssh_local_port"`
	SSHCAID      int64    `json:"ssh_ca_id"`
	GatewayID    string   `json:"gateway_id"`
	GatewayURL   string   `json:"gateway_url"`
	Roles        []string `json:"allowed_role_keywords"`
}

// webAccess renames v1's snake_case settings to v2's; validation happens when v2 loads them.
func webAccess(ctx context.Context, db store.Store, now time.Time) (bool, error) {
	if ok, err := pending(ctx, db, "web_access_configuration", "v2_web_access_config"); err != nil || !ok {
		return false, err
	}
	raw, err := legacyJSON(ctx, db, "web_access_configuration")
	if err != nil || raw == "" {
		return false, err
	}
	var v1 struct {
		Enabled              bool        `json:"enabled"`
		TerminalEnabled      bool        `json:"terminal_enabled"`
		FilesEnabled         bool        `json:"files_enabled"`
		TransferEmailEnabled bool        `json:"transfer_email_enabled"`
		Clusters             []v1Cluster `json:"clusters"`
	}
	if err := json.Unmarshal([]byte(raw), &v1); err != nil {
		return false, err
	}
	clusters := make([]map[string]any, 0, len(v1.Clusters))
	for _, c := range v1.Clusters {
		if c.SSHLocalPort == 0 {
			c.SSHLocalPort = c.SSHPort
		}
		clusters = append(clusters, map[string]any{"id": c.ID, "name": c.Name, "enabled": c.Enabled, "address": c.Address, "sshHost": c.SSHHost,
			"sshPort": c.SSHPort, "sshLocalPort": c.SSHLocalPort, "sshCaId": c.SSHCAID, "gatewayId": c.GatewayID, "gatewayUrl": c.GatewayURL, "allowedRoles": c.Roles})
	}
	out, _ := json.Marshal(map[string]any{"enabled": v1.Enabled, "terminalEnabled": v1.TerminalEnabled, "filesEnabled": v1.FilesEnabled,
		"transferEmailEnabled": v1.TransferEmailEnabled, "clusters": clusters})
	_, err = db.Exec(ctx, "INSERT INTO v2_web_access_config (id, config_json, updated_at) VALUES (1, ?, ?)", string(out), now.UTC().Format(time.RFC3339Nano))
	return err == nil, err
}

// notification keeps the transport and job template. The SMTP password was sealed with v1's
// NOTIFICATION_MASTER_KEY, which v2 does not use, so it is not carried over: the import is saved
// disabled and an administrator re-enters the password and re-enables mail.
func notification(ctx context.Context, db store.Store, now time.Time) (bool, error) {
	if ok, err := pending(ctx, db, "notification_config", "v2_notification_config"); err != nil || !ok {
		return false, err
	}
	raw, err := legacyJSON(ctx, db, "notification_config")
	if err != nil || raw == "" {
		return false, err
	}
	var v1 struct {
		TimeZone string          `json:"timeZone"`
		SMTP     json.RawMessage `json:"smtp"`
		Template json.RawMessage `json:"template"`
	}
	if err := json.Unmarshal([]byte(raw), &v1); err != nil {
		return false, err
	}
	ts := now.UTC().Format(time.RFC3339Nano)
	out, _ := json.Marshal(map[string]any{"enabled": false, "timeZone": v1.TimeZone, "provider": "", "template": v1.Template})
	stmts := []store.Statement{{Query: "INSERT INTO v2_notification_config (id, config_json, smtp_password_sealed, updated_at) VALUES (1, ?, '', ?)", Args: []any{string(out), ts}}}
	// The SMTP account becomes the default mail hub provider, disabled until its password is re-entered.
	smtp := mail.DefaultSMTP()
	if json.Unmarshal(v1.SMTP, &smtp) == nil && smtp.Host != "" {
		cfg, _ := json.Marshal(smtp)
		stmts = append(stmts, store.Statement{Query: `INSERT OR IGNORE INTO v2_mail_providers (name, kind, enabled, is_default, config_json, secret_sealed, created_at, updated_at)
			VALUES ('default', 'smtp', 0, 1, ?, '', ?, ?)`, Args: []any{string(cfg), ts, ts}})
	}
	_, err = db.Batch(ctx, stmts)
	return err == nil, err
}

// summaries copies GPU job history; v1 rows carry no event id, so the sluid stands in for it.
func summaries(ctx context.Context, db store.Store) (int64, error) {
	if ok, err := pending(ctx, db, "resource_job_summaries", "v2_resource_summaries"); err != nil || !ok {
		return 0, err
	}
	res, err := db.Exec(ctx, `INSERT OR IGNORE INTO v2_resource_summaries (cluster_id, sluid, event_id, job_id, uid, state, ended_at, ended_day,
		active_seconds, cpus, gpus, memory_mib, allocated_gpu_seconds, observed_gpu_seconds, gpu_utilization_percent_seconds,
		gpu_memory_utilization_percent_seconds, complete, payload_hash)
		SELECT cluster_id, sluid, 'legacy:' || sluid, job_id, uid, state, ended_at, ended_day, active_seconds, cpus, gpus, memory_mib,
		allocated_gpu_seconds, observed_gpu_seconds, gpu_utilization_percent_seconds, gpu_memory_utilization_percent_seconds,
		CASE WHEN complete THEN 1 ELSE 0 END, payload_hash FROM resource_job_summaries`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected, nil
}
