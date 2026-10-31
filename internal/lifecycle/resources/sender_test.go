package resources

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func summaryFixture() Summary {
	s := Summary{Version: 1, EventID: "cluster/s0123456789ABC/resource-summary", ClusterID: "cluster", Job: Job{JobID: 42, SLUID: "s0123456789ABC", UID: 10001, Name: "training", CPUs: 30, GPUs: 1, MemoryMiB: 63488, Nodes: []string{"node-a"}}, EndedAt: epoch().Add(10 * time.Second)}
	s.Result.State = "COMPLETED"
	s.Duration.ActiveSeconds = 10
	s.Duration.Scope = "last_attempt"
	a, b := 50.0, 25.0
	s.Usage.GPU = GPUUsage{AllocatedGPUSeconds: 10, ObservedGPUSeconds: 10, UtilizationPercentSeconds: 500, MemoryUtilizationPercentSeconds: 250, MeanUtilizationPercent: &a, MeanMemoryUtilizationPercent: &b, Complete: true, MissingReasons: []string{}}
	return s
}

func senderConfig() SenderConfig {
	return SenderConfig{MaxAttempts: 3, TTL: time.Second, RetryMin: time.Millisecond, RetryMax: 2 * time.Millisecond, RequestTimeout: 100 * time.Millisecond}
}
func waitSender(t *testing.T, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("sender condition timed out")
}
func testSender(t *testing.T, server *httptest.Server, maxEvents int, maxBytes int64, cfg SenderConfig) *Sender {
	t.Helper()
	s, err := NewSenderWithConfig(context.Background(), server.Client(), server.URL, maxEvents, maxBytes, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Close(ctx); err != nil {
			t.Errorf("sender close: %v", err)
		}
	})
	return s
}

func TestSenderRetriesSameImmutableBodyAndAcceptsMasterAcknowledgement(t *testing.T) {
	var calls atomic.Int64
	received := make(chan string, 3)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		received <- string(raw)
		if r.URL.Path != SummaryPath || r.Method != http.MethodPost {
			t.Errorf("wrong endpoint %s %s", r.Method, r.URL.Path)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `{"outcome":"stored"}`)
	}))
	defer server.Close()
	s := testSender(t, server, 10, 1<<20, senderConfig())
	summary := summaryFixture()
	if err := s.Accept(context.Background(), summary); err != nil {
		t.Fatal(err)
	}
	summary.Job.Nodes[0] = "mutated-after-transfer"
	waitSender(t, func() bool { return s.SnapshotStats().Stored == 1 })
	first, second := <-received, <-received
	if first != second || strings.Contains(first, "mutated-after-transfer") {
		t.Fatal("retry body is mutable")
	}
	if stats := s.SnapshotStats(); stats.Pending != 0 || stats.Bytes != 0 || stats.Retries != 1 || calls.Load() != 2 {
		t.Fatalf("retry ownership incorrect %+v", stats)
	}
}

func TestSenderTerminalAndTransientOutcomes(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		terminal string
		attempts int
	}{
		{"stored", 200, `{"outcome":"stored"}`, "stored", 1},
		{"duplicate", 200, `{"outcome":"duplicate"}`, "duplicate", 1},
		{"disabled", 200, `{"outcome":"skipped"}`, "skipped", 1},
		{"bad-request", 400, `{}`, "failed", 1},
		{"conflict", 409, `{}`, "failed", 1},
		{"throttled", 429, `{}`, "failed", 3},
		{"server-error", 503, `{}`, "failed", 3},
		{"unconfirmed", 202, `{"outcome":"received"}`, "failed", 3},
		{"unknown", 200, `{"outcome":"received"}`, "failed", 3},
		{"duplicate-json", 200, `{"outcome":"stored"}{}`, "failed", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			s := testSender(t, server, 10, 1<<20, senderConfig())
			if err := s.Accept(context.Background(), summaryFixture()); err != nil {
				t.Fatal(err)
			}
			waitSender(t, func() bool { v := s.SnapshotStats(); return v.Stored+v.RemoteDuplicates+v.Skipped+v.Failed == 1 })
			v := s.SnapshotStats()
			if calls.Load() != int64(tc.attempts) || v.Pending != 0 || v.Bytes != 0 {
				t.Fatalf("outcome not terminal as expected %+v calls=%d", v, calls.Load())
			}
			switch tc.terminal {
			case "stored":
				if v.Stored != 1 {
					t.Fatal(v)
				}
			case "duplicate":
				if v.RemoteDuplicates != 1 {
					t.Fatal(v)
				}
			case "skipped":
				if v.Skipped != 1 {
					t.Fatal(v)
				}
			case "failed":
				if v.Failed != 1 {
					t.Fatal(v)
				}
			}
		})
	}
}

func TestSenderBoundsIncludeInflightHeadAndRejectConflictingIDs(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, `{"outcome":"stored"}`)
	}))
	defer server.Close()
	cfg := senderConfig()
	cfg.RequestTimeout = time.Second
	s := testSender(t, server, 1, 1<<20, cfg)
	summary := summaryFixture()
	if err := s.Accept(context.Background(), summary); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := s.Accept(context.Background(), summary); err != nil {
		t.Fatal(err)
	}
	changed := summary
	changed.Job.Name = "different"
	if err := s.Accept(context.Background(), changed); !errors.Is(err, ErrSummaryConflict) {
		t.Fatalf("ID conflict accepted %v", err)
	}
	next := summary
	next.EventID += "/next"
	if err := s.Accept(context.Background(), next); !errors.Is(err, ErrSenderFull) {
		t.Fatalf("inflight head escaped capacity %v", err)
	}
	if v := s.SnapshotStats(); v.Pending != 1 || v.Bytes <= 0 || v.Duplicates != 1 || v.Rejected != 2 {
		t.Fatalf("queue budget %+v", v)
	}
	close(release)
	waitSender(t, func() bool { return s.SnapshotStats().Stored == 1 })
	if err := s.Accept(context.Background(), summary); err != nil {
		t.Fatal(err)
	}
	if s.SnapshotStats().Duplicates != 2 {
		t.Fatal("completed summary was not deduplicated")
	}
}

func TestSenderShutdownCancelsNetworkAndFreesQueue(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := senderConfig()
	cfg.RequestTimeout = time.Second
	s := testSender(t, server, 10, 1<<20, cfg)
	if err := s.Accept(context.Background(), summaryFixture()); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Accept(context.Background(), summaryFixture()); !errors.Is(err, ErrSenderClosed) {
		t.Fatalf("closed sender accepted work %v", err)
	}
	if v := s.SnapshotStats(); v.Pending != 0 || v.Bytes != 0 || v.Cancelled != 1 {
		t.Fatalf("shutdown leaked %+v", v)
	}
}

func TestSenderTTLValidationAndPayloadBudget(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	cfg := senderConfig()
	cfg.MaxAttempts = 100
	cfg.TTL = 8 * time.Millisecond
	cfg.RetryMin = 3 * time.Millisecond
	cfg.RetryMax = 3 * time.Millisecond
	s := testSender(t, server, 10, 1<<20, cfg)
	if err := s.Accept(context.Background(), summaryFixture()); err != nil {
		t.Fatal(err)
	}
	waitSender(t, func() bool { return s.SnapshotStats().Failed == 1 })
	if calls.Load() >= 100 {
		t.Fatal("TTL ignored")
	}
	bad := summaryFixture()
	bad.Usage.GPU.MemoryUtilizationPercentSeconds = math.NaN()
	if s.Accept(context.Background(), bad) == nil {
		t.Fatal("nonfinite summary accepted")
	}
	bad = summaryFixture()
	bad.Job.Name = strings.Repeat("x", 5000)
	if s.Accept(context.Background(), bad) == nil {
		t.Fatal("oversized job metadata accepted")
	}
	bad = summaryFixture()
	bad.Job.Nodes = make([]string, 4096)
	for i := range bad.Job.Nodes {
		bad.Job.Nodes[i] = strings.Repeat("x", 100)
	}
	if s.Accept(context.Background(), bad) == nil {
		t.Fatal("payload byte limit ignored")
	}
	if _, err := NewSender(context.Background(), server.Client(), "http://localhost", 1, 4096); err == nil {
		t.Fatal("HTTP origin accepted")
	}
	if _, err := NewSender(context.Background(), server.Client(), server.URL+"/wrong", 1, 4096); err == nil {
		t.Fatal("origin path accepted")
	}
	wire, _ := json.Marshal(s.SnapshotStats())
	if strings.Contains(string(wire), "job_id") || strings.Contains(string(wire), "cluster") {
		t.Fatal("aggregate counters expose payload")
	}
}
