package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
)

func resourceReadyEvent(t *testing.T, key string) eventbus.Event {
	t.Helper()
	payload, err := json.Marshal(Allocation{Key: key, StartedAt: epoch(), GPUCount: 1, Devices: []Device{{Node: "node-a", UUID: "GPU-A"}}})
	if err != nil {
		t.Fatal(err)
	}
	return eventbus.Event{ID: key + "/resources-ready", Type: ReadyType, SchemaVersion: 1, Subject: key, Source: "lifecycle", OccurredAt: epoch(), Payload: payload}
}

func resourceEndingEvent(key string) eventbus.Event {
	return eventbus.Event{ID: key + "/ending", Type: EndingType, SchemaVersion: 1, Subject: key, Source: "slurm-controller", OccurredAt: epoch().Add(time.Second), Payload: json.RawMessage(`{"job_id":42,"sluid":"s0123456789ABC"}`)}
}

func TestResourceListenerReadyThenEndingPausesAndRejectsLateReady(t *testing.T) {
	c := NewCollector(10, time.Second)
	l := NewListener(c, 10)
	ready := resourceReadyEvent(t, "job")
	if err := l.Handle(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if err := l.Handle(context.Background(), ready); err != nil {
		t.Fatal("idempotent Ready failed", err)
	}
	if c.Count() != 1 {
		t.Fatal("Ready did not create exactly one watch")
	}
	c.Observe(epoch().Add(time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 50, 1, 4))})
	if err := l.Handle(context.Background(), resourceEndingEvent("job")); err != nil {
		t.Fatal(err)
	}
	if err := l.Handle(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	c.Observe(epoch().Add(2*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 100, 1, 4))})
	u := c.Finish("job", epoch().Add(time.Second), 1)
	near(t, u.ObservedGPUSeconds, 1)
	near(t, *u.MeanUtilizationPercent, 50)
	if !u.Complete {
		t.Fatalf("ending lost final valid samples %+v", u)
	}
}

func TestResourceListenerEndingBeforeReadyNeverStartsWatch(t *testing.T) {
	c := NewCollector(10, time.Second)
	l := NewListener(c, 10)
	if err := l.Handle(context.Background(), resourceEndingEvent("job")); err != nil {
		t.Fatal(err)
	}
	if err := l.Handle(context.Background(), resourceReadyEvent(t, "job")); err != nil {
		t.Fatal(err)
	}
	if c.Count() != 0 {
		t.Fatal("late Ready resurrected ended execution")
	}
}

func TestResourceListenerConcurrentReadyAndEndingNeverResurrectsExecution(t *testing.T) {
	c := NewCollector(1000, time.Second)
	l := NewListener(c, 4096)
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("job-%d", i)
		ready, ending := resourceReadyEvent(t, key), resourceEndingEvent(key)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := l.Handle(context.Background(), ready); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := l.Handle(context.Background(), ending); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	c.Observe(epoch().Add(2*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 100, 1, 4))})
	for i := 0; i < 1000; i++ {
		u := c.Finish(fmt.Sprintf("job-%d", i), epoch().Add(time.Second), 1)
		if u.ObservedGPUSeconds != 0 {
			t.Fatal("ending/Ready race reopened sampling")
		}
	}
}

func TestResourceListenerBoundsAndExpiresTombstones(t *testing.T) {
	c := NewCollector(10, time.Second)
	l := NewListener(c, 2)
	now := epoch()
	l.now = func() time.Time { return now }
	for _, key := range []string{"old", "retained", "new"} {
		if err := l.Handle(context.Background(), resourceEndingEvent(key)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if len(l.tombstones) != 2 {
		t.Fatal("tombstone memory unbounded")
	}
	if _, ok := l.tombstones["old"]; ok {
		t.Fatal("oldest tombstone not evicted")
	}
	if err := l.Handle(context.Background(), resourceReadyEvent(t, "retained")); err != nil {
		t.Fatal(err)
	}
	if c.Count() != 0 {
		t.Fatal("retained tombstone was ignored")
	}
	now = now.Add(25 * time.Hour)
	if err := l.Handle(context.Background(), resourceReadyEvent(t, "retained")); err != nil {
		t.Fatal(err)
	}
	if c.Count() != 1 || len(l.tombstones) != 0 {
		t.Fatal("expired tombstones were not reclaimed")
	}
}

func TestResourceListenerValidatesSchemaSubjectAndCancellation(t *testing.T) {
	c := NewCollector(10, time.Second)
	l := NewListener(c, 10)
	base := resourceReadyEvent(t, "job")
	for _, change := range []func(*eventbus.Event){func(e *eventbus.Event) { e.SchemaVersion = 2 }, func(e *eventbus.Event) { e.Subject = "different" }, func(e *eventbus.Event) { e.Type = "unsupported" }, func(e *eventbus.Event) { e.Payload = json.RawMessage(`{"key":"job","unexpected":true}`) }} {
		bad := base
		change(&bad)
		if l.Handle(context.Background(), bad) == nil {
			t.Fatalf("invalid event accepted %+v", bad)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if l.Handle(ctx, base) == nil {
		t.Fatal("cancelled handler changed state")
	}
	if c.Count() != 0 {
		t.Fatal("invalid event created watch")
	}
	if NewListener(nil, 1).Handle(context.Background(), base) == nil {
		t.Fatal("nil collector accepted")
	}
	if err := l.Handle(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if json.Unmarshal(base.Payload, &payload) != nil {
		t.Fatal("allocation wire invalid")
	}
	for _, field := range []string{"key", "started_at", "devices", "gpu_count"} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("allocation missing canonical field %s", field)
		}
	}
}

func TestResourceListenerSubscribesThroughEventBusAndAcknowledgesByReturn(t *testing.T) {
	c := NewCollector(10, time.Second)
	l := NewListener(c, 10)
	bus, err := eventbus.New(eventbus.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	for _, kind := range []string{ReadyType, EndingType} {
		if err := bus.RegisterType(eventbus.Type{ID: kind, SchemaVersion: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bus.Subscribe(eventbus.Listener{ID: ID, Types: []string{ReadyType, EndingType}, Handle: l.Handle}); err != nil {
		t.Fatal(err)
	}
	if _, err := bus.Publish(context.Background(), resourceReadyEvent(t, "job")); err != nil {
		t.Fatal(err)
	}
	waitSender(t, func() bool { return c.Count() == 1 && bus.Snapshot().Delivered == 1 })
	if _, err := bus.Publish(context.Background(), resourceEndingEvent("job")); err != nil {
		t.Fatal(err)
	}
	waitSender(t, func() bool { return bus.Snapshot().CompletedEvents == 2 })
	if s := bus.Snapshot(); s.ActiveEvents != 0 || s.Delivered != 2 || s.RetainedBytes != 0 {
		t.Fatalf("resource listener did not release event references %+v", s)
	}
	c.Observe(epoch().Add(2*time.Second), []NodeSample{node("node-a", sample("GPU-A", 0, 100, 1, 4))})
	if c.Finish("job", epoch().Add(time.Second), 1).ObservedGPUSeconds != 0 {
		t.Fatal("ending listener did not pause watch")
	}
}
