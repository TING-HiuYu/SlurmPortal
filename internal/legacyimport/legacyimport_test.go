package legacyimport_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/legacyimport"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func TestImportOnceFromV1(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	if r, err := legacyimport.Run(ctx, db, time.Now()); err != nil || !r.Empty() {
		t.Fatalf("fresh install imported %+v %v", r, err)
	}
	for _, q := range []string{
		`CREATE TABLE web_access_configuration (id INTEGER PRIMARY KEY, config_json TEXT NOT NULL)`,
		`INSERT INTO web_access_configuration VALUES (1, '{"enabled":true,"terminal_enabled":true,"files_enabled":false,"transfer_email_enabled":false,
			"clusters":[{"id":"cluster1","name":"Cluster 1","enabled":true,"address":"cluster1.example.org","ssh_host":"127.0.0.1","ssh_port":9933,"ssh_local_port":22,
			"ssh_ca_id":0,"gateway_id":"cluster1-gw","gateway_url":"https://cluster1.example.org:19933","allowed_role_keywords":["user"]}]}')`,
		`CREATE TABLE notification_config (id INTEGER PRIMARY KEY, config_json TEXT NOT NULL, smtp_password_ciphertext TEXT)`,
		`INSERT INTO notification_config VALUES (1, '{"enabled":true,"timeZone":"Asia/Shanghai","smtp":{"host":"smtp.example.org","port":465,"security":"tls"},
			"template":{"subject":"done","html":"<p>x</p>","constants":{}},"transferTemplate":{}}', 'v1-sealed')`,
		`CREATE TABLE resource_job_summaries (cluster_id TEXT, sluid TEXT, job_id INTEGER, uid INTEGER, job_name TEXT, nodes TEXT, state TEXT, ended_at TEXT,
			ended_day TEXT, active_seconds INTEGER, cpus INTEGER, gpus INTEGER, memory_mib REAL, allocated_gpu_seconds REAL, observed_gpu_seconds REAL,
			gpu_utilization_percent_seconds REAL, gpu_memory_utilization_percent_seconds REAL, complete INTEGER, missing_reasons_json TEXT, payload_hash TEXT)`,
		`INSERT INTO resource_job_summaries VALUES ('cluster1','s2XXM5WJHN9200',1,10019,'j','n','COMPLETED','2026-10-26T17:00:58.000Z','2026-10-27',13,2,1,64,13,13,500,100,1,'[]','h1'),
			('cluster1','s2XXM5WJHN9201',2,10019,'j','n','FAILED','2026-10-26T18:00:00.000Z','2026-10-27',5,1,0,8,0,0,0,0,0,'[]','h2')`,
	} {
		if _, err := db.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	r, err := legacyimport.Run(ctx, db, time.Now())
	if err != nil || !r.WebAccess || !r.Notification || r.Summaries != 2 {
		t.Fatalf("import %+v %v", r, err)
	}
	var raw, sealed string
	if err := store.QueryRow(ctx, db, "SELECT config_json FROM v2_web_access_config", nil, &raw); err != nil {
		t.Fatal(err)
	}
	var wa struct {
		Enabled  bool
		Clusters []struct {
			GatewayURL   string   `json:"gatewayUrl"`
			SSHLocalPort int      `json:"sshLocalPort"`
			AllowedRoles []string `json:"allowedRoles"`
		}
	}
	if json.Unmarshal([]byte(raw), &wa) != nil || !wa.Enabled || wa.Clusters[0].GatewayURL != "https://cluster1.example.org:19933" || wa.Clusters[0].SSHLocalPort != 22 || wa.Clusters[0].AllowedRoles[0] != "user" {
		t.Fatalf("web access %s", raw)
	}
	if err := store.QueryRow(ctx, db, "SELECT config_json, smtp_password_sealed FROM v2_notification_config", nil, &raw, &sealed); err != nil {
		t.Fatal(err)
	}
	var n struct{ Enabled bool }
	if json.Unmarshal([]byte(raw), &n) != nil || n.Enabled || strings.Contains(raw, "smtp") || sealed != "" {
		t.Fatalf("notification %s %q", raw, sealed)
	}
	var kind, provider string
	var enabled, def int
	if err := store.QueryRow(ctx, db, "SELECT kind, enabled, is_default, config_json, secret_sealed FROM v2_mail_providers WHERE name = 'default'", nil,
		&kind, &enabled, &def, &provider, &sealed); err != nil || kind != "smtp" || enabled != 0 || def != 1 || sealed != "" || !strings.Contains(provider, `"host":"smtp.example.org"`) {
		t.Fatalf("provider %v %s %d %d %s", err, kind, enabled, def, provider)
	}
	var event string
	if err := store.QueryRow(ctx, db, "SELECT event_id FROM v2_resource_summaries WHERE job_id = 1", nil, &event); err != nil || event != "legacy:s2XXM5WJHN9200" {
		t.Fatalf("summary %q %v", event, err)
	}
	if r, err := legacyimport.Run(ctx, db, time.Now()); err != nil || !r.Empty() {
		t.Fatalf("second run imported again: %+v %v", r, err)
	}
}
