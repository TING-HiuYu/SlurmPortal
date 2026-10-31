package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func init() { plugin.Register(Plugin{}) }

func TestReport(t *testing.T) {
	ctx := context.Background()
	db := testutil.DB(t)
	admin := testutil.User(t, db, "boss", 1, "admin")
	user := testutil.User(t, db, "alice", 1, "user")
	now := time.Now().UTC()
	fresh, old := now.Add(-10*time.Second).Format(time.RFC3339Nano), now.Add(-time.Hour).Format(time.RFC3339Nano)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT INTO v2_cluster_reports (kind, cluster_id, attempted_ms, received_at, health, metadata_json) VALUES ('cluster-status', 'cluster1', 1, ?, 'ok', '{}')", []any{fresh}},
		{"INSERT INTO v2_cluster_reports (kind, cluster_id, attempted_ms, received_at, health, metadata_json) VALUES ('resource-status', 'cluster1', 1, ?, 'error', '{}')", []any{fresh}},
		{"INSERT INTO v2_cluster_reports (kind, cluster_id, attempted_ms, received_at, health, metadata_json) VALUES ('cluster-status', 'cluster2', 1, ?, 'ok', '{}')", []any{old}},
		{"INSERT INTO v2_gateway_registrations VALUES ('cluster1-gw', 'cluster1', 'i', 'b', 'f', '[]', '[]', ?)", []any{now.UnixMilli()}},
		{"INSERT INTO v2_notification_config VALUES (1, '{\"enabled\":true}', '', ?)", []any{fresh}},
		{"INSERT INTO v2_job_notifications (cluster_id, event_id, sluid, job_id, uid, fingerprint, status, created_at, updated_at) VALUES ('cluster1', 'e', 's', 1, 1, 'f', 'failed', ?, ?)", []any{fresh, fresh}},
	} {
		if _, err := db.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.New(ctx, &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth:     config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		Plugins:  config.Plugins{Enabled: []string{ID}},
		Sync:     config.Sync{PeerFingerprints: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}},
		Clusters: plugin.Clusters{{ID: "cluster1", Fingerprint: strings.Repeat("a", 64)}, {ID: "cluster2", Fingerprint: strings.Repeat("b", 64)}},
		JWTKeys:  authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	get := func(uid int64) (int, map[string]Check) {
		tok, _, _, _ := a.JWT.Issue(uid, authn.SourcePassword)
		req := httptest.NewRequest(http.MethodGet, "/api/v2/diagnostics", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		var body struct {
			Data struct{ Checks []Check } `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		out := map[string]Check{}
		for _, c := range body.Data.Checks {
			out[c.ID] = c
		}
		return rec.Code, out
	}
	if code, _ := get(user); code != http.StatusForbidden {
		t.Fatalf("plain user read diagnostics: %d", code)
	}
	code, checks := get(admin)
	want := map[string]string{
		"master:database":                "ok",
		"slave:cluster1:cluster-status":  "ok",
		"slave:cluster1:resource-status": "error",
		"slave:cluster2:cluster-status":  "stale",
		"slave:cluster2:resource-status": "missing",
		"gateway:cluster1-gw":            "ok",
		"master:mail":                    "error",
		"master:mail-delivery":           "error",
	}
	if code != 200 || len(checks) != len(want) {
		t.Fatalf("%d %+v", code, checks)
	}
	for id, status := range want {
		if checks[id].Status != status {
			t.Errorf("%s: %+v, want %s", id, checks[id], status)
		}
	}
}
