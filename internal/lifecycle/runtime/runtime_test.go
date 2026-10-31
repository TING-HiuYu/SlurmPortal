package joblifecycle

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobdoneemail"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/resources"
)

// These fixtures require both sides of every HTTP connection to authenticate
// with locally generated certificates; no real master, Slurm or GPU is contacted.
type testMTLS struct {
	pool           *x509.CertPool
	server, client tls.Certificate
}

func newTestMTLS(t *testing.T) testMTLS {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lifecycle fixture CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := func(serial int64, client bool) tls.Certificate {
		key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "lifecycle fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
		if client {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		} else {
			template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			template.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			template.DNSNames = []string{"localhost"}
		}
		der, e := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{der, caDER}, PrivateKey: key}
	}
	return testMTLS{pool: pool, server: leaf(2, false), client: leaf(3, true)}
}

func (m testMTLS) serve(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	s := httptest.NewUnstartedServer(handler)
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{m.server}, ClientCAs: m.pool, ClientAuth: tls.RequireAndVerifyClientCert}
	s.StartTLS()
	t.Cleanup(s.Close)
	return s
}
func (m testMTLS) httpClient(t *testing.T) *http.Client {
	t.Helper()
	c := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: m.pool, Certificates: []tls.Certificate{m.client}}}, Timeout: 2 * time.Second}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

type lifecycleFixture struct {
	mu        sync.Mutex
	jobs      map[uint32]map[string]any
	summaries []resources.Summary
	mails     []jobnotify.Event
	queries   atomic.Int64
	mtls      testMTLS
	observer  *httptest.Server
	master    *httptest.Server
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	f := &lifecycleFixture{jobs: map[uint32]map[string]any{}, mtls: newTestMTLS(t)}
	f.observer = f.mtls.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.queries.Add(1)
		if strings.Contains(req.URL.Path, "/resources/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []any{map[string]any{"node": "fixture-node", "gres": []any{map[string]any{"name": "gpu", "index": "0"}}}}, "errors": []any{}})
			return
		}
		id, _ := strconv.ParseUint(req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], 10, 32)
		f.mu.Lock()
		defer f.mu.Unlock()
		row := f.jobs[uint32(id)]
		if row == nil {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(jobsTestBody(row))
	}))
	f.master = f.mtls.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch req.URL.Path {
		case resources.SummaryPath:
			var summary resources.Summary
			if json.NewDecoder(req.Body).Decode(&summary) != nil {
				w.WriteHeader(400)
				return
			}
			f.summaries = append(f.summaries, summary)
			_, _ = io.WriteString(w, `{"outcome":"stored"}`)
		case jobnotify.CompletionPath:
			var report jobnotify.Event
			if json.NewDecoder(req.Body).Decode(&report) != nil {
				w.WriteHeader(400)
				return
			}
			f.mails = append(f.mails, report)
			_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
		default:
			http.NotFound(w, req)
		}
	}))
	return f
}

func (f *lifecycleFixture) config() Config {
	c := Config{ClusterID: "fixture", Listen: "127.0.0.1:0", TokenFile: "/not-used-by-new", SlurmURL: f.observer.URL, TLS: jobnotify.TLSFiles{ClientFingerprints: []string{strings.Repeat("0", 64)}}, MaxJobs: 32, ResolveTTL: "5s", RequestTimeout: "500ms", GRESIndicesAreNVML: true, sampleInterval: time.Second}
	c.Plugins.JobEmail = true
	c.Plugins.Resources.Enabled = true
	return c
}
func (f *lifecycleFixture) runtime(t *testing.T, edit func(*Config)) (*Runtime, eventbus.LocalTokens) {
	t.Helper()
	cfg := f.config()
	if edit != nil {
		edit(&cfg)
	}
	tokens, err := eventbus.NewLocalTokens()
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(context.Background(), cfg, tokens, f.mtls.httpClient(t), f.mtls.httpClient(t), f.master.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("runtime shutdown: %v", err)
		}
	})
	return r, tokens
}

func waitLifecycle(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func fakeClustat(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lifecycle-clustat-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "backend.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || !strings.HasPrefix(line, "QUERY ") {
					return
				}
				queries.Add(1)
				_ = json.NewEncoder(conn).Encode(map[string]any{"nodes": []any{map[string]any{"hostname": "fixture-node", "timestamp_ms": time.Now().UnixMilli(), "stale": false, "gres": []any{map[string]any{"index": 0, "uuid": "GPU-fixture-A", "util": 80, "mem_used_mb": 4096, "mem_total_mb": 8192}}}}})
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); wg.Wait(); _ = os.RemoveAll(dir) })
	return path, &queries
}

func postLifecycle(t *testing.T, client *http.Client, origin, path, token, body string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func TestRuntimeMTLSEndToEndSamplingAndShortJobSummaries(t *testing.T) {
	f := newLifecycleFixture(t)
	socket, queries := fakeClustat(t)
	now := time.Now().Unix()
	f.jobs[7] = jobTestRow(7, lifecycleSLUID, "RUNNING", now-65, 0, 1)
	f.jobs[8] = jobTestRow(8, "s123456789ABCE", "RUNNING", now-2, 0, 1)
	r, tokens := f.runtime(t, func(c *Config) { c.ClustatSocket = socket })
	listener := f.mtls.serve(t, r.Handler())
	client := f.mtls.httpClient(t)
	for _, id := range []uint32{7, 8} {
		sluid := lifecycleSLUID
		if id == 8 {
			sluid = "s123456789ABCE"
		}
		body, _ := json.Marshal(jobnotify.Callback{JobID: id, SLUID: sluid})
		if status := postLifecycle(t, client, listener.URL, StartingPath, tokens.ReadWrite, string(body)); status != 202 {
			t.Fatalf("start returned %d", status)
		}
	}
	waitLifecycle(t, 4*time.Second, "shared clustat samples", func() bool { return queries.Load() >= 4 })
	f.mu.Lock()
	for _, id := range []uint32{7, 8} {
		f.jobs[id]["job_state"] = []string{"COMPLETED"}
		f.jobs[id]["end_time"] = wrappedTest(time.Now().Unix())
	}
	f.mu.Unlock()
	for _, id := range []uint32{7, 8} {
		sluid := lifecycleSLUID
		if id == 8 {
			sluid = "s123456789ABCE"
		}
		body, _ := json.Marshal(jobnotify.Callback{JobID: id, SLUID: sluid})
		if status := postLifecycle(t, client, listener.URL, EndingPath, tokens.ReadWrite, string(body)); status != 202 {
			t.Fatalf("end returned %d", status)
		}
		if status := postLifecycle(t, client, listener.URL, EndingPath, tokens.ReadWrite, string(body)); status != 202 {
			t.Fatalf("duplicate end returned %d", status)
		}
	}
	waitLifecycle(t, 4*time.Second, "resource summaries and qualified mail", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.summaries) == 2 && len(f.mails) == 1 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mails[0].Job.JobID != 7 || f.mails[0].Duration.ActiveSeconds <= 60 {
		t.Fatalf("short job reached mail: %+v", f.mails)
	}
	for _, summary := range f.summaries {
		gpu := summary.Usage.GPU
		if summary.Job.CPUs != 4 || summary.Job.GPUs != 1 || summary.Job.MemoryMiB != 2048 || summary.Duration.Scope != "last_attempt" || gpu.ObservedGPUSeconds <= 0 || gpu.MeanUtilizationPercent == nil || math.Abs(*gpu.MeanUtilizationPercent-80) > 0.001 || gpu.MeanMemoryUtilizationPercent == nil || math.Abs(*gpu.MeanMemoryUtilizationPercent-50) > 0.001 {
			t.Errorf("bad observed summary: %+v", summary)
		}
	}
	if r.Snapshot().Duplicates < 2 {
		t.Fatalf("duplicate hooks weren't deduplicated: %+v", r.Snapshot())
	}
}

func TestRuntimeHandlerUsesMTLSAndDistinctReadWriteScopes(t *testing.T) {
	f := newLifecycleFixture(t)
	r, tokens := f.runtime(t, nil)
	server := f.mtls.serve(t, r.Handler())
	client := f.mtls.httpClient(t)
	body := `{"job_id":7,"sluid":"` + lifecycleSLUID + `"}`
	for _, token := range []string{tokens.ReadOnly, "wrong", ""} {
		if status := postLifecycle(t, client, server.URL, StartingPath, token, body); status != 403 {
			t.Errorf("read-only/invalid publish status %d", status)
		}
	}
	for _, token := range []string{tokens.ReadOnly, tokens.ReadWrite} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+StatsPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if response.StatusCode != 200 || bytes.Contains(data, []byte(tokens.ReadOnly)) || bytes.Contains(data, []byte(tokens.ReadWrite)) {
			t.Fatalf("stats leaked credentials or denied valid read: %d %s", response.StatusCode, data)
		}
	}
	unauthenticated := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: f.mtls.pool}}, Timeout: time.Second}
	defer unauthenticated.CloseIdleConnections()
	if response, err := unauthenticated.Get(server.URL + StatsPath); err == nil {
		_ = response.Body.Close()
		t.Fatal("missing client certificate was accepted")
	}
	request := httptest.NewRequest(http.MethodPost, StartingPath, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+tokens.ReadWrite)
	recorder := httptest.NewRecorder()
	r.Handler().ServeHTTP(recorder, request)
	if recorder.Code != 401 {
		t.Fatalf("plain HTTP bypassed mTLS: %d", recorder.Code)
	}
	if status := postLifecycle(t, client, server.URL, StartingPath, tokens.ReadWrite, `{"job_id":7,"job_id":8,"sluid":"`+lifecycleSLUID+`"}`); status != 400 {
		t.Fatalf("ambiguous callback accepted: %d", status)
	}
}

func TestRuntimeLocalPluginConfigControlsEnablement(t *testing.T) {
	f := newLifecycleFixture(t)
	r, _ := f.runtime(t, func(c *Config) { c.Plugins.JobEmail = false; c.Plugins.Resources.Enabled = false })
	if r.enabled(resources.ID) || r.enabled(jobdoneemail.ID) || r.mail != nil || r.metrics != nil {
		t.Fatal("disabled local plugins were started")
	}
	if r.enabled("future-plugin") {
		t.Fatal("unknown plugin reported enabled")
	}
	enabled, _ := f.runtime(t, nil)
	if !enabled.enabled(resources.ID) || !enabled.enabled(jobdoneemail.ID) || enabled.mail == nil || enabled.metrics == nil {
		t.Fatal("enabled local plugins were not started")
	}
	for _, seconds := range []int{4, 301, -1} {
		cfg := f.config()
		cfg.sampleInterval = 0
		cfg.Plugins.Resources.SampleIntervalSeconds = seconds
		if cfg.defaults() == nil {
			t.Fatalf("sample interval %d accepted", seconds)
		}
	}
	cfg := f.config()
	cfg.sampleInterval = 0
	if cfg.defaults() != nil || cfg.Plugins.Resources.SampleIntervalSeconds != 15 || cfg.sampleInterval != 15*time.Second {
		t.Fatalf("default sample interval not applied: %+v", cfg.Plugins.Resources)
	}
}

func TestRuntimeLocalSamplingIntervalUpdatesCollectorGapBudget(t *testing.T) {
	f := newLifecycleFixture(t)
	r, _ := f.runtime(t, func(c *Config) { c.sampleInterval = 0; c.Plugins.Resources.SampleIntervalSeconds = 5 })
	// A four-second sample gap is valid for the configured five-second cadence,
	// but would be rejected by the previous one-second collector gap budget.
	start := time.Now().Add(time.Minute)
	key := "fixture/interval-regression"
	if err := r.collector.Start(resources.Allocation{Key: key, StartedAt: start, GPUCount: 1, Devices: []resources.Device{{Node: "fixture-node", UUID: "GPU-fixture-A"}}}); err != nil {
		t.Fatal(err)
	}
	uuid := "GPU-fixture-A"
	r.collector.Observe(start.Add(4*time.Second), []resources.NodeSample{{Hostname: "fixture-node", Devices: []resources.DeviceSample{{UUID: &uuid, Util: 80, MemoryUsedMiB: 4096, MemoryTotalMiB: 8192}}}})
	usage := r.collector.Finish(key, start.Add(4*time.Second), 4)
	if usage.ObservedGPUSeconds != 4 || usage.MeanUtilizationPercent == nil || *usage.MeanUtilizationPercent != 80 {
		t.Fatalf("local cadence did not reach collector: %+v", usage)
	}
}

func TestRuntimeFrozenReportRetriesWithoutRepeatingSummaryOwnership(t *testing.T) {
	f := newLifecycleFixture(t)
	f.jobs[7] = jobTestRow(7, lifecycleSLUID, "FAILED", 1000, 1061, 0)
	r, _ := f.runtime(t, nil)
	if err := r.bus.UpdateConfig(eventbus.Config{MaxEvents: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.bus.RegisterType(eventbus.Type{ID: "fixture.hold", SchemaVersion: 1, Sources: []string{"fixture"}}); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := r.bus.Subscribe(eventbus.Listener{ID: "fixture.hold", Types: []string{"fixture.hold"}, Handle: func(ctx context.Context, e eventbus.Event) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.bus.Publish(context.Background(), eventbus.Event{ID: "fixture/hold", Type: "fixture.hold", SchemaVersion: 1, Source: "fixture", OccurredAt: time.Now(), Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("blocking fixture listener did not start")
	}
	callback := jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}
	key := r.key(callback)
	r.mu.Lock()
	r.jobs[key] = &execution{callback: callback, created: time.Now(), ending: true, working: true}
	r.mu.Unlock()
	if err := r.resolveEnd(context.Background(), key, callback); !errors.Is(err, eventbus.ErrFull) {
		t.Fatalf("report should wait for bus capacity: %v", err)
	}
	waitLifecycle(t, time.Second, "resource sender ownership", func() bool { return r.metrics.SnapshotStats().Stored == 1 })
	r.mu.Lock()
	frozen := append([]byte(nil), r.jobs[key].report...)
	queued := r.jobs[key].queuedSummary
	r.mu.Unlock()
	if len(frozen) == 0 || !queued {
		t.Fatal("failed publication did not retain frozen report and summary ownership")
	}
	f.mu.Lock()
	f.jobs[7]["name"] = "later controller data must not alter report"
	f.jobs[7]["job_state"] = []string{"COMPLETING"}
	f.mu.Unlock()
	close(release)
	waitLifecycle(t, time.Second, "bus capacity recovery", func() bool { return r.Snapshot().ActiveEvents == 0 })
	if err := r.publishReport(context.Background(), key, frozen); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, time.Second, "frozen email delivery", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.mails) == 1 })
	if got := r.metrics.SnapshotStats(); got.Accepted != 1 || got.Duplicates != 0 || got.Stored != 1 {
		t.Fatalf("publication retry repeated summary handoff: %+v", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.summaries) != 1 || f.summaries[0].Job.Name != "isolated lifecycle fixture" || f.mails[0].Job.Name != "isolated lifecycle fixture" {
		t.Fatalf("retry changed or duplicated frozen content: summary=%+v mail=%+v", f.summaries, f.mails)
	}
}

func TestRuntimeReportByteAdmissionFailurePreservesCollectedUsageForRetry(t *testing.T) {
	f := newLifecycleFixture(t)
	f.jobs[7] = jobTestRow(7, lifecycleSLUID, "COMPLETED", 1000, 1002, 1)
	r, _ := f.runtime(t, func(c *Config) { c.MaxRetainedReportBytes = 4096 })
	callback := jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}
	key := r.key(callback)
	start := time.Unix(1000, 0)
	if err := r.collector.Start(resources.Allocation{Key: key, StartedAt: start, GPUCount: 1, Devices: []resources.Device{{Node: "fixture-node", UUID: "GPU-fixture-A"}}}); err != nil {
		t.Fatal(err)
	}
	uuid := "GPU-fixture-A"
	r.collector.Observe(start.Add(2*time.Second), []resources.NodeSample{{Hostname: "fixture-node", Devices: []resources.DeviceSample{{UUID: &uuid, Util: 80, MemoryUsedMiB: 4096, MemoryTotalMiB: 8192}}}})
	held := []byte(`{"held":"` + strings.Repeat("x", 4085) + `"}`)
	r.mu.Lock()
	r.jobs[key] = &execution{callback: callback, created: time.Now(), ending: true, working: true, watching: true}
	r.jobs["fixture/held-report"] = &execution{working: true, report: held}
	r.reportBytes = int64(len(held))
	r.mu.Unlock()
	if err := r.resolveEnd(context.Background(), key, callback); !errors.Is(err, eventbus.ErrFull) {
		t.Fatalf("full report budget was not enforced: %v", err)
	}
	preview := r.collector.Preview(key, start.Add(2*time.Second), 2)
	if preview.ObservedGPUSeconds != 2 || preview.MeanUtilizationPercent == nil || *preview.MeanUtilizationPercent != 80 || r.metrics.SnapshotStats().Accepted != 0 {
		t.Fatalf("failed admission consumed GPU samples or handed off summary: %+v", preview)
	}
	r.mu.Lock()
	delete(r.jobs, "fixture/held-report")
	r.reportBytes = 0
	r.mu.Unlock()
	if err := r.resolveEnd(context.Background(), key, callback); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, time.Second, "retained measurements summary", func() bool { return r.metrics.SnapshotStats().Stored == 1 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.summaries) != 1 || f.summaries[0].Usage.GPU.ObservedGPUSeconds != 2 || f.summaries[0].Usage.GPU.MeanUtilizationPercent == nil || *f.summaries[0].Usage.GPU.MeanUtilizationPercent != 80 {
		t.Fatalf("retry lost collected measurements: %+v", f.summaries)
	}
	r.mu.Lock()
	remaining := r.reportBytes
	r.mu.Unlock()
	if remaining != 0 || len(r.collector.Nodes()) != 0 {
		t.Fatalf("successful handoff retained report or monitor state: bytes=%d", remaining)
	}
}

func TestRuntimeEndingBeforeStartingDoesNotReopenExecution(t *testing.T) {
	f := newLifecycleFixture(t)
	f.jobs[7] = jobTestRow(7, lifecycleSLUID, "CANCELLED", 1000, 1002, 0)
	r, _ := f.runtime(t, nil)
	callback := jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}
	if _, err := r.Publish(context.Background(), EndingType, callback, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, time.Second, "short cancellation summary", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.summaries) == 1 })
	if _, err := r.Publish(context.Background(), StartingType, callback, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, time.Second, "late start event cleanup", func() bool { return r.Snapshot().ActiveEvents == 0 })
	r.mu.Lock()
	active := len(r.jobs)
	r.mu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if active != 0 || len(f.summaries) != 1 || len(f.mails) != 0 || !f.summaries[0].Usage.GPU.Complete {
		t.Fatalf("late start reopened finished execution: jobs=%d summaries=%+v mail=%d", active, f.summaries, len(f.mails))
	}
}

func TestRuntimeResolutionExpiresAndShutdownReleasesMemory(t *testing.T) {
	f := newLifecycleFixture(t)
	r, _ := f.runtime(t, func(c *Config) { c.ResolveTTL = "150ms"; c.MaxJobs = 1 })
	callback := jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}
	if _, err := r.Publish(context.Background(), StartingType, callback, time.Now()); err != nil {
		t.Fatal(err)
	}
	waitLifecycle(t, time.Second, "missing job to enter runtime", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.jobs) == 1 })
	waitLifecycle(t, 3*time.Second, "unresolvable job expiration", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return len(r.jobs) == 0 })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(context.Background(), StartingType, callback, time.Now()); !errors.Is(err, eventbus.ErrClosed) {
		t.Fatalf("publish accepted after close: %v", err)
	}
	stats := r.Snapshot()
	if stats.ActiveEvents != 0 || stats.ActiveDeliveries != 0 || stats.RetainedBytes != 0 {
		t.Fatalf("shutdown retained event state: %+v", stats)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) != 0 || len(r.seen) != 0 || len(r.eventTimes) != 0 {
		t.Fatal("shutdown retained lifecycle state")
	}
}

func TestRuntimeRejectsAdditionalExecutionsAtCapacity(t *testing.T) {
	f := newLifecycleFixture(t)
	r, _ := f.runtime(t, func(c *Config) { c.MaxJobs = 1 })
	first, _ := json.Marshal(jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID})
	second, _ := json.Marshal(jobnotify.Callback{JobID: 8, SLUID: "s123456789ABCE"})
	if err := r.accept(context.Background(), eventbus.Event{Type: StartingType, Payload: first}); err != nil {
		t.Fatal(err)
	}
	if err := r.accept(context.Background(), eventbus.Event{Type: StartingType, Payload: second}); !errors.Is(err, eventbus.ErrFull) {
		t.Fatalf("execution capacity was not enforced: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.jobs) != 1 {
		t.Fatalf("capacity limit exceeded: %d", len(r.jobs))
	}
}

func TestRuntimeCloseCancelsInFlightObserverRequest(t *testing.T) {
	f := newLifecycleFixture(t)
	entered := make(chan struct{})
	var once sync.Once
	blocked := f.mtls.serve(t, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		once.Do(func() { close(entered) })
		<-req.Context().Done()
	}))
	r, _ := f.runtime(t, func(c *Config) { c.SlurmURL = blocked.URL; c.RequestTimeout = "5s" })
	if _, err := r.Publish(context.Background(), StartingType, jobnotify.Callback{JobID: 7, SLUID: lifecycleSLUID}, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("observer request did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("shutdown waited for observer timeout: %v", time.Since(started))
	}
}
