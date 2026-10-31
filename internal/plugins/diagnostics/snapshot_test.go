package diagnostics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/objstore/objstoretest"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/currentstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func init() { plugin.Register(currentstatus.Plugin{}) }

// With current-status enabled, the node and queue checks come from each cluster's snapshot.
func TestReportsFromCurrentStatusSnapshot(t *testing.T) {
	ctx := context.Background()
	s3 := objstoretest.New(t)
	db := testutil.DB(t)
	admin := testutil.User(t, db, "boss", 1, "admin")
	var section yaml.Node
	if err := yaml.Unmarshal([]byte("endpoint: "+s3.URL+"\nbucket: status\n"), &section); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PLUGIN_CURRENT_STATUS_R2_ACCESS_KEY_ID": "ABCDEFGHIJKLMNOP", "PLUGIN_CURRENT_STATUS_R2_SECRET_ACCESS_KEY": strings.Repeat("s", 40)}
	a, err := app.New(ctx, &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth:     config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		Plugins:  config.Plugins{Enabled: []string{ID, currentstatus.ID}, Config: map[string]yaml.Node{currentstatus.ID: *section.Content[0]}},
		Clusters: plugin.Clusters{{ID: "cluster1", Fingerprint: strings.Repeat("a", 64)}, {ID: "cluster2", Fingerprint: strings.Repeat("b", 64)}, {ID: "old", Fingerprint: strings.Repeat("c", 64)}},
		JWTKeys:  authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		Getenv:   func(k string) string { return env[k] },
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	put := func(cluster string, at time.Time, nodes, queue string) {
		doc := map[string]any{"observed_at": at.Format(time.RFC3339Nano)}
		if nodes != "" {
			doc["cluster_status"] = map[string]any{"health": nodes}
		}
		if queue != "" {
			doc["resource_status"] = map[string]any{"health": queue}
		}
		b, _ := json.Marshal(doc)
		s3.Set("/status/"+currentstatus.Key(cluster), b, http.Header{})
	}
	put("cluster1", now.Add(-20*time.Second), "ok", "error")
	put("old", now.Add(-time.Hour), "ok", "ok")
	// cluster2 never published.

	tok, _, _, _ := a.JWT.Issue(admin, authn.SourcePassword)
	req := httptest.NewRequest(http.MethodGet, "/api/v2/diagnostics", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	var body struct {
		Data struct{ Checks []Check } `json:"data"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	got := map[string]string{}
	for _, c := range body.Data.Checks {
		got[c.ID] = c.Status + "/" + c.Detail
	}
	want := map[string]string{
		"slave:cluster1:cluster-status": "ok/report_fresh", "slave:cluster1:resource-status": "error/slave_collection_failed",
		"slave:cluster2:cluster-status": "missing/never_reported", "slave:cluster2:resource-status": "missing/never_reported",
		"slave:old:cluster-status": "stale/report_stale", "slave:old:resource-status": "stale/report_stale",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s = %q, want %q", id, got[id], w)
		}
	}
}
