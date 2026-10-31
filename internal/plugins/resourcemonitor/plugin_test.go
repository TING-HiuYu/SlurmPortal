package resourcemonitor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"

	. "github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcemonitor"
)

func init() { plugin.Register(Plugin{}) }

var (
	fp      = strings.Repeat("a", 64)
	unbound = strings.Repeat("b", 64)
)

func summary(sluid string, uid int, state string, ended time.Time) string {
	return fmt.Sprintf(`{"version":1,"event_id":"cluster1/%[1]s/resources","cluster_id":"cluster1",
		"job":{"job_id":42,"sluid":"%[1]s","uid":%[2]d,"name":"train","cpus":8,"gpus":2,"memory_mib":4096,"nodes":["n1"]},
		"result":{"state":"%[3]s"},"duration":{"active_seconds":3600,"scope":"last_attempt"},"ended_at":"%[4]s",
		"usage":{"gpu":{"allocated_gpu_seconds":7200,"observed_gpu_seconds":7200,"utilization_percent_seconds":360000,
		"memory_utilization_percent_seconds":144000,"mean_utilization_percent":50,"mean_memory_utilization_percent":20,
		"complete":true,"missing_reasons":[]}}}`, sluid, uid, state, ended.UTC().Format(time.RFC3339))
}

func TestResourceMonitor(t *testing.T) {
	db := testutil.DB(t)
	alice := testutil.User(t, db, "alice", 1, "user")
	bob := testutil.User(t, db, "bob", 1, "user")
	admin := testutil.User(t, db, "admin", 1, "admin")
	ctx := context.Background()
	for id, uid := range map[int64]int{alice: 10001, bob: 10002, admin: 0} {
		if _, err := db.Exec(ctx, "UPDATE users SET uid_number = ? WHERE id = ?", uid, id); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.New(ctx, &config.Config{
		Role: "master", HTTP: config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth: config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour}, Plugins: config.Plugins{Enabled: []string{ID}},
		Sync: config.Sync{PeerFingerprints: []string{fp, unbound}}, Clusters: plugin.Clusters{{ID: "cluster1", Fingerprint: fp}},
		JWTKeys: authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}, db)
	if err != nil {
		t.Fatal(err)
	}
	post := func(peer, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", SummaryPath, strings.NewReader(body))
		if peer != "" {
			req = req.WithContext(authn.WithBridgePeer(req.Context(), peer))
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		return rec
	}
	token := func(id int64) string {
		tok, _, _, err := a.JWT.Issue(id, authn.SourcePassword)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	get := func(path string, id int64) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+token(id))
		rec := httptest.NewRecorder()
		a.Handler.ServeHTTP(rec, req)
		return rec
	}

	now := time.Now()
	good := summary("s0123456789ABC", 10001, "COMPLETE", now.Add(-time.Minute))
	if rec := post(fp, "", good); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"outcome":"stored"}` {
		t.Fatalf("store %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(fp, "", good); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"outcome":"duplicate"}` {
		t.Fatalf("duplicate %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(fp, "", strings.Replace(good, `"name":"train"`, `"name":"other"`, 1)); rec.Code != 409 {
		t.Fatalf("identity conflict %d", rec.Code)
	}
	if rec := post(fp, "", summary("s0123456789ABD", 10002, "FAILED", now.Add(-time.Minute))); rec.Code != 200 {
		t.Fatalf("bob %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(fp, "", summary("s0123456789ABE", 10002, "COMPLETED", now.Add(-10*24*time.Hour))); rec.Code != 200 {
		t.Fatalf("old %d", rec.Code)
	}

	if rec := post(unbound, "", good); rec.Code != 403 {
		t.Errorf("unbound peer %d", rec.Code)
	}
	if rec := post(strings.Repeat("c", 64), "", good); rec.Code != 401 && rec.Code != 403 {
		t.Errorf("unknown peer %d", rec.Code)
	}
	if rec := post("", token(alice), good); rec.Code != 401 {
		t.Errorf("user token %d", rec.Code)
	}
	for name, bad := range map[string]string{
		"other cluster":   strings.Replace(good, `"cluster_id":"cluster1"`, `"cluster_id":"cluster2"`, 1),
		"unknown field":   strings.Replace(good, `"version":1`, `"version":1,"raw":{}`, 1),
		"bad state":       strings.Replace(good, `"COMPLETE"`, `"RUNNING"`, 1),
		"bad sluid":       strings.Replace(good, `"sluid":"s0123456789ABC"`, `"sluid":"s0123456789ABI"`, 1),
		"job id zero":     strings.Replace(good, `"job_id":42`, `"job_id":0`, 1),
		"negative cpus":   strings.Replace(good, `"cpus":8`, `"cpus":-1`, 1),
		"missing cpus":    strings.Replace(good, `"cpus":8,`, ``, 1),
		"future end":      summary("s0123456789ABF", 10001, "COMPLETED", now.Add(time.Hour)),
		"offset end":      strings.Replace(good, `Z"`, `+00:00"`, 1),
		"allocated lie":   strings.Replace(good, `"allocated_gpu_seconds":7200`, `"allocated_gpu_seconds":7100`, 1),
		"percent > 100":   strings.Replace(good, `"utilization_percent_seconds":360000`, `"utilization_percent_seconds":730000`, 1),
		"mean mismatch":   strings.Replace(good, `"mean_utilization_percent":50`, `"mean_utilization_percent":51`, 1),
		"reason newline":  strings.Replace(good, `"missing_reasons":[]`, `"missing_reasons":["a\nb"]`, 1),
		"reasons null":    strings.Replace(good, `"missing_reasons":[]`, `"missing_reasons":null`, 1),
		"nodes object":    strings.Replace(good, `"nodes":["n1"]`, `"nodes":{}`, 1),
		"event id cr":     strings.Replace(good, `"event_id":"cluster1/`, `"event_id":"\rgz/`, 1),
		"wrong scope":     strings.Replace(good, `"last_attempt"`, `"all"`, 1),
		"version 2":       strings.Replace(good, `"version":1`, `"version":2`, 1),
		"fractional uid":  strings.Replace(good, `"uid":10001`, `"uid":10001.5`, 1),
		"huge active sec": strings.Replace(good, `"active_seconds":3600`, `"active_seconds":315576001`, 1),
	} {
		if rec := post(fp, "", bad); rec.Code != 400 {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	if rec := get("/api/v2/resource-monitor/stats", alice); rec.Code != 403 {
		t.Fatalf("user global stats %d", rec.Code)
	}
	if rec := get("/api/v2/resource-monitor/stats?states=FAILED&x=1", admin); rec.Code != 400 {
		t.Fatalf("unknown param %d", rec.Code)
	}
	if rec := get("/api/v2/resource-monitor/stats?states=RUNNING", admin); rec.Code != 400 {
		t.Fatalf("bad state filter %d", rec.Code)
	}
	var global struct {
		Data struct {
			Days []struct {
				Date   string
				States []struct {
					State    string
					JobCount int64
				}
				GPUUtilizationPercent *float64 `json:"gpuUtilizationPercent"`
			}
			SelectedStates []string `json:"selectedStates"`
			Users          []struct {
				UID      int64
				Username string
				JobCount int64
				GPUHours float64 `json:"gpuHours"`
			}
		}
	}
	rec := get("/api/v2/resource-monitor/stats", admin)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &global) != nil {
		t.Fatalf("global stats %d %s", rec.Code, rec.Body.String())
	}
	d := global.Data
	last := d.Days[len(d.Days)-1]
	if len(d.Days) != 7 || len(d.Users) != 2 || d.Users[0].Username != "alice" || d.Users[1].Username != "bob" ||
		d.Users[0].GPUHours != 2 || len(last.States) != 2 || last.States[0].State != "COMPLETED" || last.States[1].State != "FAILED" ||
		last.GPUUtilizationPercent == nil || *last.GPUUtilizationPercent != 50 {
		t.Fatalf("global %s", rec.Body.String())
	}
	rec = get("/api/v2/resource-monitor/stats?states=FAILED", admin)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &global) != nil || len(global.Data.Users) != 1 || global.Data.Users[0].Username != "bob" {
		t.Fatalf("filtered %s", rec.Body.String())
	}

	var mine struct {
		Data struct {
			Days []struct {
				States []struct {
					State    string
					JobCount int64
				}
			}
			Users any
		}
	}
	rec = get("/api/v2/resource-monitor/my-stats", alice)
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &mine) != nil || mine.Data.Users != nil {
		t.Fatalf("my stats %d %s", rec.Code, rec.Body.String())
	}
	var states []string
	for _, day := range mine.Data.Days {
		for _, s := range day.States {
			states = append(states, s.State)
		}
	}
	if len(states) != 1 || states[0] != "COMPLETED" {
		t.Fatalf("alice saw %v", states)
	}
	if rec := get("/api/v2/resource-monitor/my-stats?uid=10002", alice); rec.Code != 400 {
		t.Fatalf("uid param %d", rec.Code)
	}
	if rec := get("/api/v2/resource-monitor/my-stats", admin); rec.Code != 403 {
		t.Fatalf("admin without uid %d", rec.Code)
	}
}
