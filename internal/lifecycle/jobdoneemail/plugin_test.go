package jobdoneemail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
)

func pluginTestConfig() jobnotify.Config {
	return jobnotify.Config{ClusterID: "fixture", Listen: "127.0.0.1:0", SlurmURL: "https://observer.example.invalid", CallbackTLS: jobnotify.TLSFiles{ClientFingerprints: []string{strings.Repeat("0", 64)}}, MaxEvents: 4, MaxAttempts: 3, RetryMax: "1ms", SendTTL: "2s"}
}
func pluginTestReport(sluid string, seconds int64) eventbus.Event {
	report := jobnotify.Event{Version: 1, EventID: "fixture/" + sluid + "/ended", ClusterID: "fixture", Job: jobnotify.Job{JobID: 7, SLUID: sluid, UID: 10019, Name: "isolated email fixture"}, Result: jobnotify.Result{State: "COMPLETED"}, Duration: jobnotify.Duration{ActiveSeconds: seconds, Scope: "last_attempt", ExcludesSuspend: true}, ObservedAt: time.Now().UTC()}
	body, _ := json.Marshal(report)
	return eventbus.Event{ID: "fixture/" + sluid + "/report", Type: "slurm.job.report.ready", SchemaVersion: 1, Subject: "fixture/" + sluid, Source: "lifecycle", OccurredAt: report.ObservedAt, Payload: body}
}
func waitPlugin(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
func createPlugin(t *testing.T, server *httptest.Server, cfg jobnotify.Config) *Plugin {
	t.Helper()
	p, err := New(context.Background(), cfg, server.Client(), server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Errorf("plugin close: %v", err)
		}
	})
	return p
}

func TestEventAcknowledgementTransfersOwnershipBeforeSMTPAcceptance(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		once.Do(func() { close(entered) })
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
		case <-req.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	p := createPlugin(t, server, pluginTestConfig())
	bus, err := eventbus.New(eventbus.Config{DeliveryTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	if err := bus.RegisterType(eventbus.Type{ID: "slurm.job.report.ready", SchemaVersion: 1, Sources: []string{"lifecycle"}}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Subscribe(eventbus.Listener{ID: ID, Types: []string{"slurm.job.report.ready"}, Handle: p.Handle}); err != nil {
		t.Fatal(err)
	}
	event := pluginTestReport("s123456789ABCD", 61)
	if _, err := bus.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("SMTP handoff not attempted")
	}
	waitPlugin(t, "event release while SMTP is pending", func() bool { return bus.Snapshot().ActiveEvents == 0 })
	if stats := p.Stats(); stats.Accepted != 1 || stats.Pending != 1 || stats.Sent != 0 {
		t.Fatalf("event acknowledgement claimed SMTP acceptance: %+v", stats)
	}
	if bus.Snapshot().TimedOut != 0 {
		t.Fatalf("long SMTP response blocked event handler: %+v", bus.Snapshot())
	}
	if err := p.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Accepted != 1 {
		t.Fatal("duplicate report created another email task")
	}
	close(release)
	waitPlugin(t, "final SMTP acceptance", func() bool { return p.Stats().Sent == 1 && p.Stats().Pending == 0 })
	if err := p.Handle(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Accepted != 1 {
		t.Fatal("completed report was sent again")
	}
}

func TestEmailPluginFiltersOnlyMailNotLifecycleAndRejectsMalformedReports(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
	}))
	t.Cleanup(server.Close)
	p := createPlugin(t, server, pluginTestConfig())
	for _, seconds := range []int64{0, 1, 59, 60} {
		if err := p.Handle(context.Background(), pluginTestReport("s123456789ABCD", seconds)); err != nil {
			t.Fatal(err)
		}
	}
	bad := pluginTestReport("s123456789ABCD", 61)
	bad.Payload = []byte(`{"job":`)
	if err := p.Handle(context.Background(), bad); err == nil {
		t.Fatal("malformed report accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Handle(ctx, pluginTestReport("s123456789ABCD", 61)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled handoff accepted: %v", err)
	}
	if p.Stats().Accepted != 0 || calls.Load() != 0 {
		t.Fatal("filtered report reached SMTP")
	}
}

func TestEmailPluginPreservesFIFOAcrossRetriesAndExplicitSkip(t *testing.T) {
	var mu sync.Mutex
	var order []string
	firstAttempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var report jobnotify.Event
		if json.NewDecoder(req.Body).Decode(&report) != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		order = append(order, report.Job.SLUID)
		if report.Job.SLUID == "s123456789ABCD" {
			firstAttempts++
			if firstAttempts == 1 {
				w.WriteHeader(503)
				return
			}
			_, _ = io.WriteString(w, `{"outcome":"smtp_accepted"}`)
		} else {
			_, _ = io.WriteString(w, `{"outcome":"skipped","reason":"missing_mail"}`)
		}
	}))
	t.Cleanup(server.Close)
	p := createPlugin(t, server, pluginTestConfig())
	for _, sluid := range []string{"s123456789ABCD", "s123456789ABCE"} {
		if err := p.Handle(context.Background(), pluginTestReport(sluid, 61)); err != nil {
			t.Fatal(err)
		}
	}
	waitPlugin(t, "retried FIFO deliveries", func() bool { stats := p.Stats(); return stats.Sent == 1 && stats.Skipped == 1 && stats.Pending == 0 })
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "s123456789ABCD" || order[1] != "s123456789ABCD" || order[2] != "s123456789ABCE" {
		t.Fatalf("FIFO reordered: %+v", order)
	}
}

func TestEmailPluginRejectsQueueOverflowAndShutdownCancelsPendingSMTP(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		once.Do(func() { close(entered) })
		<-req.Context().Done()
	}))
	t.Cleanup(server.Close)
	cfg := pluginTestConfig()
	cfg.MaxEvents = 1
	p := createPlugin(t, server, cfg)
	if err := p.Handle(context.Background(), pluginTestReport("s123456789ABCD", 61)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("SMTP handoff missing")
	}
	if err := p.Handle(context.Background(), pluginTestReport("s123456789ABCE", 61)); err == nil {
		t.Fatal("full sender queue accepted another report")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Stats().Pending != 0 || p.Stats().Bytes != 0 {
		t.Fatalf("shutdown retained messages: %+v", p.Stats())
	}
	if err := p.Handle(context.Background(), pluginTestReport("s123456789ABCE", 61)); err == nil {
		t.Fatal("stopped plugin accepted work")
	}
}
