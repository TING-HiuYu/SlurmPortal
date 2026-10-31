package resourcestatus_test

import (
	"context"
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

	. "github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcestatus"
)

func init() { plugin.Register(Plugin{}) }

func TestIngestAndQuery(t *testing.T) {
	fp := strings.Repeat("a", 64)
	db := testutil.DB(t)
	testutil.User(t, db, "alice", 1, "user")
	a, err := app.New(context.Background(), &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth: config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour}, Plugins: config.Plugins{Enabled: []string{ID}},
		Sync: config.Sync{PeerFingerprints: []string{fp}}, Clusters: plugin.Clusters{{ID: "cluster1", Fingerprint: fp}},
		JWTKeys: authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) int {
		req := httptest.NewRequest("POST", SnapshotPath, strings.NewReader(body))
		req = req.WithContext(authn.WithBridgePeer(req.Context(), fp))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		return rec.Code
	}
	at := time.Now().UTC().Format(time.RFC3339)
	good := `{"version":1,"cluster_id":"cluster1","observed_at":"` + at + `","health":"ok","error":null,
		"resources":{"cpu":{"total":64,"available":10},"memory_mib":{"total":1000,"available":5},"gpu":{"total":8,"available":0}},
		"jobs":[{"id":"42","name":"train","user":"alice","state":"RUNNING","partition":"gpu","allocated":{"cpu":8,"memory_mib":null,"gpu":2},
		"requested":{"cpu":8,"memory_mib":null,"gpu":2},"elapsed_seconds":60,"time_limit_seconds":3600,"reason":"None","node_count":1}],
		"jobs_total":1,"jobs_truncated":false}`
	if code := post(good); code != 200 {
		t.Fatalf("good report %d", code)
	}
	for name, bad := range map[string]string{
		"available > total": strings.Replace(good, `"available":10`, `"available":99`, 1),
		"control chars":     strings.Replace(good, `"train"`, `"tr\u0007ain"`, 1),
		"other cluster":     strings.Replace(good, `"cluster_id":"cluster1"`, `"cluster_id":"cluster2"`, 1),
		"truncation lie":    strings.Replace(good, `"jobs_total":1`, `"jobs_total":5`, 1),
		"unknown field":     strings.Replace(good, `"version":1`, `"version":1,"raw":{}`, 1),
	} {
		if code := post(bad); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	tok, _, _, _ := a.JWT.Issue(1, authn.SourcePassword)
	req := httptest.NewRequest("GET", "/api/v2/resource-status?clusters=cluster1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"health":"ok"`) || !strings.Contains(rec.Body.String(), `"train"`) {
		t.Fatalf("query %d %s", rec.Code, rec.Body.String())
	}
}
