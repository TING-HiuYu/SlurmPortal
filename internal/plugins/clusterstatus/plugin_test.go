package clusterstatus_test

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
	"github.com/TING-HiuYu/SlurmPortal/internal/slave"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"

	. "github.com/TING-HiuYu/SlurmPortal/internal/plugins/clusterstatus"
)

func init() { plugin.Register(Plugin{}) }

var (
	fpCluster1 = strings.Repeat("a", 64)
	fpOther    = strings.Repeat("b", 64)
)

type fakeRunner struct{ out string }

func (f fakeRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte(f.out), nil
}

func setupMaster(t *testing.T) *app.App {
	db := testutil.DB(t)
	testutil.User(t, db, "alice", 1, "user")
	cfg := &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth:     config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		Plugins:  config.Plugins{Enabled: []string{ID}},
		Sync:     config.Sync{PeerFingerprints: []string{fpCluster1, fpOther}},
		Clusters: plugin.Clusters{{ID: "cluster1", Name: "Cluster 1", Fingerprint: fpCluster1}, {ID: "cluster2", Fingerprint: strings.Repeat("c", 64)}},
		JWTKeys:  authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}
	a, err := app.New(context.Background(), cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// peerTransport delivers reports in-process as the Worker bridge would, tagged with a peer fingerprint.
type peerTransport struct {
	h  http.Handler
	fp string
}

func (p peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, r.WithContext(authn.WithBridgePeer(r.Context(), p.fp)))
	return rec.Result(), nil
}

func statusOf(t *testing.T, a *app.App) map[string]any {
	tok, _, _, _ := a.JWT.Issue(1, authn.SourcePassword)
	req := httptest.NewRequest("GET", "/api/v2/cluster-status", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("query %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Clusters []map[string]any `json:"clusters"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	out := map[string]any{}
	for _, c := range body.Data.Clusters {
		out[c["id"].(string)] = c
	}
	return out
}

func TestSlaveReportsReachMaster(t *testing.T) {
	a := setupMaster(t)
	col, err := NewCollector("cluster1", Config{Enabled: true, Address: "10.0.0.5"}, fakeRunner{"NodeName=n1 State=IDLE\nNodeName=n2 State=MIXED+DRAIN\n"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rp := slave.Reporter{Name: ID, URL: "https://main" + SnapshotPath, Client: &http.Client{Transport: peerTransport{a.Handler, fpCluster1}},
		Collect: func(ctx context.Context, at time.Time) (any, error) { return col.Collect(ctx, at) }, Now: time.Now}
	if err := rp.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	cluster1 := statusOf(t, a)["cluster1"].(map[string]any)
	if cluster1["status"] != "partial_maintenance" || cluster1["health"] != "ok" || cluster1["nodesTotal"].(float64) != 2 || cluster1["address"] != "10.0.0.5" || cluster1["name"] != "Cluster 1" {
		t.Fatalf("cluster1 = %v", cluster1)
	}
	if cluster2 := statusOf(t, a)["cluster2"].(map[string]any); cluster2["error"] != "never_observed" {
		t.Fatalf("cluster2 = %v", cluster2)
	}

	// A peer whose certificate is not bound to the claimed cluster cannot report for it.
	rp.Client = &http.Client{Transport: peerTransport{a.Handler, fpOther}}
	if err := rp.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("unbound peer: %v", err)
	}
	// Users cannot post snapshots.
	tok, _, _, _ := a.JWT.Issue(1, authn.SourcePassword)
	req := httptest.NewRequest("POST", SnapshotPath, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("user ingest %d", rec.Code)
	}
}

func TestCheckRejectsInconsistentReports(t *testing.T) {
	now := time.Now()
	good := func() Report {
		c := EmptyCounts()
		c["idle"] = 1
		return Report{Version: 1, ClusterID: "cluster1", ObservedAt: now, Health: "ok", AddressHealth: "not_configured", CurrentIPs: []string{},
			Nodes: []Node{{Name: "n1", Status: "idle", SlurmState: "IDLE"}}, NodeCounts: c, NodesTotal: 1}
	}
	if r := good(); r.Check("cluster1", now, MaxNodes) != nil {
		t.Fatal("good report rejected")
	}
	for name, mutate := range map[string]func(*Report){
		"other cluster":  func(r *Report) { r.ClusterID = "cluster2" },
		"future":         func(r *Report) { r.ObservedAt = now.Add(time.Minute) },
		"status forged":  func(r *Report) { r.Nodes[0].Status = "full" },
		"count mismatch": func(r *Report) { r.NodesTotal = 2 },
		"loopback ip": func(r *Report) {
			a := "x.example"
			r.Address, r.AddressHealth, r.CurrentIPs = &a, "ok", []string{"127.0.0.1"}
		},
		"extra status": func(r *Report) { r.NodeCounts["weird"] = 0 },
		"ok but error": func(r *Report) { e := "collection_failed"; r.Error = &e },
	} {
		r := good()
		mutate(&r)
		if r.Check("cluster1", now, MaxNodes) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
