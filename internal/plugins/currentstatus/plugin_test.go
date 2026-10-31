package currentstatus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/objstore/objstoretest"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/clusterstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcestatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
	"gopkg.in/yaml.v3"
)

func init() { plugin.Register(Plugin{}) }

var (
	fpCluster1 = strings.Repeat("a", 64)
	fpCluster2 = strings.Repeat("b", 64)
)

type harness struct {
	t     *testing.T
	app   *app.App
	s3    *objstoretest.Server
	token string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s3 := objstoretest.New(t)
	db := testutil.DB(t)
	testutil.User(t, db, "alice", 1, "user")
	var section yaml.Node
	if err := yaml.Unmarshal([]byte("endpoint: "+s3.URL+"\nbucket: status\n"), &section); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"PLUGIN_CURRENT_STATUS_R2_ACCESS_KEY_ID": "ABCDEFGHIJKLMNOP", "PLUGIN_CURRENT_STATUS_R2_SECRET_ACCESS_KEY": strings.Repeat("s", 40)}
	a, err := app.New(context.Background(), &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth:     config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		Plugins:  config.Plugins{Enabled: []string{ID}, Config: map[string]yaml.Node{ID: *section.Content[0]}},
		Sync:     config.Sync{PeerFingerprints: []string{fpCluster1, fpCluster2}},
		Clusters: plugin.Clusters{{ID: "cluster1", Name: "Cluster 1", Fingerprint: fpCluster1}, {ID: "cluster2", Fingerprint: fpCluster2}},
		JWTKeys:  authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		Getenv:   func(k string) string { return env[k] },
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, _, _ := a.JWT.Issue(1, authn.SourcePassword)
	return &harness{t, a, s3, tok}
}

func (h *harness) put(fp string, rep any) (int, string) {
	b, _ := json.Marshal(rep)
	req := httptest.NewRequest(http.MethodPut, IngestPath, strings.NewReader(string(b)))
	if fp != "" {
		req = req.WithContext(authn.WithBridgePeer(req.Context(), fp))
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.app.Handler.ServeHTTP(rec, req)
	var out struct {
		Data struct{ Outcome string } `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Data.Outcome
}

func report(cluster string, at time.Time) Report {
	at = at.UTC().Truncate(time.Millisecond)
	alloc := int64(2)
	res := resourcestatus.Report{Version: 1, ClusterID: cluster, ObservedAt: at, Health: "ok",
		Resources: &resourcestatus.Resources{CPU: resourcestatus.Capacity{Total: 64, Available: 10}, MemoryMiB: resourcestatus.Capacity{Total: 1000, Available: 5}, GPU: resourcestatus.Capacity{Total: 8, Available: 6, Allocated: &alloc}},
		Jobs:      []resourcestatus.Job{{ID: "42", Name: "train", User: "alice", State: "RUNNING", Partition: "gpu", Reason: "None"}}, JobsTotal: 1}
	counts := clusterstatus.EmptyCounts()
	counts["running"] = 1
	nodes := clusterstatus.Report{Version: 1, ClusterID: cluster, ObservedAt: at, Health: "ok", CurrentIPs: []string{}, AddressHealth: "not_configured",
		Nodes: []clusterstatus.Node{{Name: "cn01", SlurmState: "MIXED", Status: "running"}}, NodeCounts: counts, NodesTotal: 1}
	return Report{Version: 1, ClusterID: cluster, ObservedAt: at, ResourceStatus: &res, ClusterStatus: &nodes}
}

func TestIngestCAS(t *testing.T) {
	h := newHarness(t)
	t0 := time.Now().Add(-time.Minute)
	if code, out := h.put(fpCluster1, report("cluster1", t0)); code != 200 || out != "accepted" {
		t.Fatal(code, out)
	}
	obj, ok := h.s3.Object("/status/" + Key("cluster1"))
	if !ok || obj.Meta.Get("X-Amz-Meta-Observed-Ms") != strconv.FormatInt(t0.Truncate(time.Millisecond).UnixMilli(), 10) {
		t.Fatal("object metadata", obj.Meta)
	}
	var body map[string]any
	if json.Unmarshal(obj.Body, &body) != nil || body["stale_after_seconds"] != float64(180) || body["received_at"] == nil || body["cluster_id"] != "cluster1" {
		t.Fatal(string(obj.Body))
	}
	if code, out := h.put(fpCluster1, report("cluster1", t0)); code != 200 || out != "duplicate" {
		t.Fatal("duplicate", code, out)
	}
	if code, out := h.put(fpCluster1, report("cluster1", t0.Add(-time.Second))); code != 200 || out != "ignored" {
		t.Fatal("older", code, out)
	}
	changed := report("cluster1", t0)
	changed.ResourceStatus.Jobs[0].Name = "other"
	if code, out := h.put(fpCluster1, changed); code != 200 || out != "ignored" {
		t.Fatal("same time, different content", code, out)
	}
	if code, out := h.put(fpCluster1, report("cluster1", t0.Add(time.Second))); code != 200 || out != "accepted" {
		t.Fatal("newer", code, out)
	}
	// A competing writer stores a newer report between HEAD and PUT: the loser must not overwrite it.
	newer := t0.Add(time.Hour).UnixMilli()
	h.s3.BeforePut = func(s *objstoretest.Server, path string) {
		s.BeforePut = nil
		o, _ := s.Object(path)
		meta := o.Meta.Clone()
		meta.Set("X-Amz-Meta-Observed-Ms", strconv.FormatInt(newer, 10))
		s.Set(path, []byte(`{"winner":true}`), meta)
	}
	if code, out := h.put(fpCluster1, report("cluster1", t0.Add(2*time.Second))); code != 200 || out != "ignored" {
		t.Fatal("contention", code, out)
	}
	if o, _ := h.s3.Object("/status/" + Key("cluster1")); string(o.Body) != `{"winner":true}` {
		t.Fatal("competing writer overwritten")
	}
	// Each cluster writes only its own key.
	if code, _ := h.put(fpCluster2, report("cluster2", t0)); code != 200 {
		t.Fatal(code)
	}
	if _, ok := h.s3.Object("/status/" + Key("cluster2")); !ok {
		t.Fatal("cluster2 object missing")
	}
}

func TestIngestRejects(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	mutate := func(f func(*Report)) Report { r := report("cluster1", now); f(&r); return r }
	for name, rep := range map[string]Report{
		"other cluster":      report("cluster2", now),
		"section time":       mutate(func(r *Report) { r.ClusterStatus.ObservedAt = r.ObservedAt.Add(time.Second) }),
		"missing section":    mutate(func(r *Report) { r.ResourceStatus = nil }),
		"truncated jobs":     mutate(func(r *Report) { r.ResourceStatus.JobsTruncated, r.ResourceStatus.JobsTotal = true, 5 }),
		"bad version":        mutate(func(r *Report) { r.Version = 2 }),
		"future observation": report("cluster1", now.Add(time.Hour)),
	} {
		if code, _ := h.put(fpCluster1, rep); code != 400 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := h.put(strings.Repeat("c", 64), report("cluster1", now)); code != 401 && code != 403 {
		t.Fatal("unbound certificate", code)
	}
	if code, _ := h.put("", report("cluster1", now)); code != 401 {
		t.Fatal("anonymous", code)
	}
	h.s3.Set("/status/"+Key("cluster1"), []byte(`{}`), http.Header{"X-Amz-Meta-Version": {"9"}})
	if code, _ := h.put(fpCluster1, report("cluster1", now)); code != 503 {
		t.Fatal("foreign metadata", code)
	}
}

func TestLinks(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.put(fpCluster1, report("cluster1", time.Now())); code != 200 {
		t.Fatal(code)
	}
	get := func(path, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.app.Handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(LinksPath, ""); rec.Code != 401 {
		t.Fatal("anonymous links", rec.Code)
	}
	if rec := get(LinksPath+"?cluster=cluster1", h.token); rec.Code != 400 {
		t.Fatal("selector accepted", rec.Code)
	}
	rec := get(LinksPath, h.token)
	var out struct {
		Data struct {
			StaleAfterSeconds int
			Clusters          []link
		}
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Data.Clusters) != 2 || out.Data.StaleAfterSeconds != 180 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if c := out.Data.Clusters[0]; c.ID != "cluster1" || c.Name != "Cluster 1" || time.Until(c.ExpiresAt) > 15*time.Minute || !strings.Contains(c.URL, "/status/current-status/v1/cluster1/current.json?") {
		t.Fatal(c)
	}
	resp, err := http.Get(out.Data.Clusters[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"train"`) {
		t.Fatal(resp.StatusCode, string(b))
	}
}

type fakeRes struct {
	r   resourcestatus.Report
	err error
}

func (f fakeRes) Collect(context.Context, time.Time) (resourcestatus.Report, error) {
	return f.r, f.err
}

type fakeNodes struct {
	r   clusterstatus.Report
	err error
}

func (f fakeNodes) Collect(context.Context, time.Time) (clusterstatus.Report, error) {
	return f.r, f.err
}

func TestCollector(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Millisecond)
	good := report("cluster1", at)
	collect := func(r ResourceCollector, n ClusterCollector) Report {
		c, err := NewCollectorWithSources("cluster1", r, n)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := c.Collect(context.Background(), at)
		if err != nil {
			t.Fatal(err)
		}
		return rep
	}
	rep := collect(fakeRes{r: *good.ResourceStatus}, fakeNodes{r: *good.ClusterStatus})
	if rep.ResourceStatus.Health != "ok" || rep.ClusterStatus.Health != "ok" || rep.Check("cluster1", time.Now()) != nil {
		t.Fatal("good sample", rep)
	}
	rep = collect(fakeRes{err: errors.New("squeue failed")}, fakeNodes{r: *good.ClusterStatus})
	if rep.ResourceStatus.Health != "error" || rep.ClusterStatus.Health != "ok" || rep.Check("cluster1", time.Now()) != nil {
		t.Fatal("resource failure", rep.ResourceStatus)
	}
	addr := "login.example.org"
	withDNS := *good.ClusterStatus
	withDNS.Address, withDNS.CurrentIPs, withDNS.AddressHealth, withDNS.Health = &addr, []string{"203.0.113.7"}, "ok", "error"
	rep = collect(fakeRes{r: *good.ResourceStatus}, fakeNodes{r: withDNS, err: errors.New("sinfo failed")})
	if rep.ClusterStatus.Health != "error" || len(rep.ClusterStatus.CurrentIPs) != 1 || len(rep.ClusterStatus.Nodes) != 0 || rep.Check("cluster1", time.Now()) != nil {
		t.Fatal("node failure keeps fresh DNS", rep.ClusterStatus)
	}
	foreign := withDNS
	foreign.ClusterID = "cluster2"
	rep = collect(fakeRes{r: *good.ResourceStatus}, fakeNodes{r: foreign})
	if rep.ClusterStatus.Address != nil || len(rep.ClusterStatus.CurrentIPs) != 0 || rep.ClusterStatus.AddressHealth != "not_configured" {
		t.Fatal("foreign DNS crossed cluster boundary", rep.ClusterStatus)
	}
	big := *good.ResourceStatus
	big.Jobs = make([]resourcestatus.Job, 0, MaxItems)
	for i := range MaxItems {
		big.Jobs = append(big.Jobs, resourcestatus.Job{ID: strconv.Itoa(i), Name: strings.Repeat("n", 300), User: "alice", State: "PENDING"})
	}
	big.JobsTotal = len(big.Jobs)
	rep = collect(fakeRes{r: big}, fakeNodes{r: *good.ClusterStatus})
	if rep.ResourceStatus.Health != "error" || rep.ClusterStatus.Health != "error" {
		t.Fatal("oversized sample must fail both sections explicitly")
	}
}
