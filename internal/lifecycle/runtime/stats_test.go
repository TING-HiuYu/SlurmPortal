package joblifecycle

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/resources"
)

type runtimeDeliveryStats struct {
	Bus               eventbus.Stats         `json:"bus"`
	ActiveJobs        int                    `json:"active_jobs"`
	Mail              *jobnotify.Stats       `json:"mail"`
	ResourceSummaries *resources.SenderStats `json:"resource_summaries"`
}

func readRuntimeDeliveryStats(t *testing.T, client *http.Client, origin, token string) (runtimeDeliveryStats, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, origin+StatsPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated stats failed: status%d err%v", response.StatusCode, err)
	}
	var stats runtimeDeliveryStats
	if err := json.Unmarshal(body, &stats); err != nil {
		t.Fatal(err)
	}
	return stats, body
}

func TestRuntimeStatsRetainPendingDeliveryAfterBusReferencesAreReleased(t *testing.T) {
	f := newLifecycleFixture(t)
	const payloadMarker = "private-job-payload-not-a-statistic"
	f.jobs[7] = jobTestRow(7, lifecycleSLUID, "COMPLETED", 1000, 1061, 0)
	f.jobs[7]["name"] = payloadMarker
	var mailAttempts, summaryAttempts atomic.Int64
	mailBlocked, summaryBlocked := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	master := f.mtls.serve(t, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case jobnotify.CompletionPath, resources.SummaryPath:
			counter, entered, outcome := &mailAttempts, mailBlocked, "smtp_accepted"
			if request.URL.Path == resources.SummaryPath {
				counter, entered, outcome = &summaryAttempts, summaryBlocked, "stored"
			}
			attempt := counter.Add(1)
			if attempt == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if attempt == 2 {
				close(entered)
			}
			select {
			case <-release:
				_ = json.NewEncoder(w).Encode(map[string]string{"outcome": outcome})
			case <-request.Context().Done():
			}
		default:
			http.NotFound(w, request)
		}
	}))
	tokens, err := eventbus.NewLocalTokens()
	if err != nil {
		t.Fatal(err)
	}
	sender := f.mtls.httpClient(t)
	sender.Timeout = 5 * time.Second
	r, err := New(context.Background(), f.config(), tokens, f.mtls.httpClient(t), sender, master.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := r.Close(ctx); err != nil {
			t.Errorf("runtime close: %v", err)
		}
	})
	// Release blocked HTTP handlers before closing the runtime on any failure.
	t.Cleanup(unblock)
	server := f.mtls.serve(t, r.Handler())
	client := f.mtls.httpClient(t)
	body := `{"job_id":7,"sluid":"` + lifecycleSLUID + `"}`
	if status := postLifecycle(t, client, server.URL, EndingPath, tokens.ReadWrite, body); status != http.StatusAccepted {
		t.Fatalf("ending hook rejected: %d", status)
	}
	for _, entered := range []chan struct{}{mailBlocked, summaryBlocked} {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("master delivery did not retry then block")
		}
	}
	var pending runtimeDeliveryStats
	var encoded []byte
	waitLifecycle(t, time.Second, "bus completion with independent pending FIFOs", func() bool {
		pending, encoded = readRuntimeDeliveryStats(t, client, server.URL, tokens.ReadOnly)
		return pending.Mail != nil && pending.ResourceSummaries != nil && pending.Mail.Pending == 1 && pending.ResourceSummaries.Pending == 1 && pending.Bus.CompletedEvents >= 2 && pending.Bus.ActiveEvents == 0 && pending.Bus.ActiveDeliveries == 0
	})
	if pending.Mail.Bytes <= 0 || pending.ResourceSummaries.Bytes <= 0 || pending.ResourceSummaries.Retries < 1 || mailAttempts.Load() < 2 || summaryAttempts.Load() < 2 {
		t.Fatal("stats lost blocked/retrying delivery ownership")
	}
	if bytes.Contains(encoded, []byte(payloadMarker)) || bytes.Contains(encoded, []byte(lifecycleSLUID)) || bytes.Contains(encoded, []byte(tokens.ReadOnly)) || bytes.Contains(encoded, []byte(tokens.ReadWrite)) {
		t.Fatal("aggregate stats exposed payload, job identity or credentials")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &fields)
	for _, name := range []string{"mail", "resource_summaries"} {
		var values map[string]any
		if err := json.Unmarshal(fields[name], &values); err != nil {
			t.Fatal(err)
		}
		for key, value := range values {
			if _, numeric := value.(float64); !numeric {
				t.Fatalf("%s.%s is not an aggregate counter", name, key)
			}
		}
	}
	unblock()
	waitLifecycle(t, 4*time.Second, "acknowledged delivery queues drained", func() bool {
		stats, _ := readRuntimeDeliveryStats(t, client, server.URL, tokens.ReadOnly)
		return stats.Mail.Pending == 0 && stats.Mail.Bytes == 0 && stats.Mail.Sent == 1 && stats.ResourceSummaries.Pending == 0 && stats.ResourceSummaries.Bytes == 0 && stats.ResourceSummaries.Stored == 1
	})
}

func TestRuntimeStatsOmitUninitializedDeliveryServices(t *testing.T) {
	bus, err := eventbus.New(eventbus.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	tokens, err := eventbus.NewLocalTokens()
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{bus: bus, tokens: tokens}
	mtls := newTestMTLS(t)
	server := mtls.serve(t, r.Handler())
	_, data := readRuntimeDeliveryStats(t, mtls.httpClient(t), server.URL, tokens.ReadOnly)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	if _, exists := fields["mail"]; exists {
		t.Fatal("nil mail service was reported as an initialized empty queue")
	}
	if _, exists := fields["resource_summaries"]; exists {
		t.Fatal("nil resource sender was reported as an initialized empty queue")
	}
}

func TestRuntimeStatsReflectConfiguredDeliveryServices(t *testing.T) {
	f := newLifecycleFixture(t)
	for _, on := range []bool{true, false} {
		r, tokens := f.runtime(t, func(c *Config) { c.Plugins.JobEmail = on; c.Plugins.Resources.Enabled = on })
		server := f.mtls.serve(t, r.Handler())
		stats, data := readRuntimeDeliveryStats(t, f.mtls.httpClient(t), server.URL, tokens.ReadOnly)
		if (stats.Mail != nil) != on || (stats.ResourceSummaries != nil) != on {
			t.Fatalf("stats did not reflect configured delivery services (enabled=%v)", on)
		}
		if bytes.Contains(data, []byte(`"plugins"`)) {
			t.Fatal("stats still expose negotiated plugin policy")
		}
	}
}
