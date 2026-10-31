package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testBus(t *testing.T, cfg Config) *Bus {
	t.Helper()
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.RegisterType(Type{ID: "slurm.execution.starting", SchemaVersion: 1, Sources: []string{"controller"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := b.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return b
}

func testEvent(id string) Event {
	return Event{ID: id, Type: "slurm.execution.starting", SchemaVersion: 1, Subject: "execution/s0123456789ABC", Source: "controller", OccurredAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(`{"job_id":42}`)}
}

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for time.Now().Before(until) {
		if predicate() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestBroadcastHasIndependentPayloadAndWaitsForAllListeners(t *testing.T) {
	b := testBus(t, Config{})
	firstRead := make(chan struct{})
	release := make(chan struct{})
	var seen atomic.Int64
	if err := b.Subscribe(Listener{ID: "first", Types: []string{"slurm.execution.starting"}, Handle: func(_ context.Context, e Event) error { e.Payload[0] = '['; seen.Add(1); close(firstRead); return nil }}); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(Listener{ID: "second", Types: []string{"slurm.execution.starting"}, Handle: func(_ context.Context, e Event) error {
		<-firstRead
		if string(e.Payload) != `{"job_id":42}` {
			return errors.New("payload was mutated by peer")
		}
		<-release
		seen.Add(1)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	e := testEvent("broadcast/1")
	receipt, err := b.Publish(context.Background(), e)
	if err != nil || receipt.Deliveries != 2 {
		t.Fatalf("publish %+v %v", receipt, err)
	}
	e.Payload[1] = 'X'
	waitFor(t, func() bool { return b.Snapshot().Delivered == 1 })
	if s := b.Snapshot(); s.ActiveEvents != 1 || s.ActiveDeliveries != 1 {
		t.Fatalf("prematurely destroyed: %+v", s)
	}
	close(release)
	waitFor(t, func() bool { return b.Snapshot().RetainedBytes == 0 })
	s := b.Snapshot()
	if s.CompletedEvents != 1 || s.Delivered != 2 || s.Failed != 0 || seen.Load() != 2 {
		t.Fatalf("bad broadcast stats %+v", s)
	}
}

func TestSubscriberSetIsFixedAtPublish(t *testing.T) {
	b := testBus(t, Config{})
	entered := make(chan struct{})
	release := make(chan struct{})
	var late atomic.Int64
	if err := b.Subscribe(Listener{ID: "first", Types: []string{"slurm.execution.starting"}, Handle: func(context.Context, Event) error { close(entered); <-release; return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), testEvent("snapshot/1")); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := b.Subscribe(Listener{ID: "late", Types: []string{"slurm.execution.starting"}, Handle: func(context.Context, Event) error { late.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitFor(t, func() bool { return b.Snapshot().ActiveEvents == 0 })
	if late.Load() != 0 {
		t.Fatal("listener received an event published before subscription")
	}
}

func TestCompletionCASRacesExactlyOnce(t *testing.T) {
	b := testBus(t, Config{MaxEvents: 2048, MaxDedupEntries: 4096, ListenerQueueSize: 2048, ListenerConcurrency: 32, DeliveryTimeout: 50 * time.Microsecond})
	var returned atomic.Int64
	if err := b.Subscribe(Listener{ID: "race", Types: []string{"slurm.execution.starting"}, Handle: func(ctx context.Context, _ Event) error { <-ctx.Done(); returned.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	const count = 1000
	for i := 0; i < count; i++ {
		if _, err := b.Publish(context.Background(), testEvent(fmt.Sprintf("late/%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, func() bool { s := b.Snapshot(); return s.CompletedEvents == count && s.RetainedBytes == 0 })
	s := b.Snapshot()
	if s.TimedOut != count || s.Delivered != 0 || s.Cancelled != 0 || s.ActiveDeliveries != 0 || returned.Load() != count {
		t.Fatalf("late completion consumed twice or leaked: %+v returned=%d", s, returned.Load())
	}
}

func TestAtomicFinalizerUnderSimultaneousSuccessAndTimeout(t *testing.T) {
	b := testBus(t, Config{})
	for i := 0; i < 1000; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		s := &subscription{spec: Listener{ID: "race"}}
		e := &eventEntry{event: testEvent(fmt.Sprintf("cas/%d", i)), deliveries: map[string]*delivery{}, charge: 512}
		e.refs.Store(1)
		d := &delivery{bus: b, entry: e, listener: s, ctx: ctx, cancel: cancel, charge: 256}
		e.deliveries["race"] = d
		b.mu.Lock()
		b.events[e.event.ID] = e
		b.bytes += 768
		b.mu.Unlock()
		var wg sync.WaitGroup
		start := make(chan struct{})
		for _, outcome := range []uint32{succeeded, timedOut, cancelled, failed} {
			wg.Add(1)
			go func(result uint32) { defer wg.Done(); <-start; d.finish(result) }(outcome)
		}
		close(start)
		wg.Wait()
		if e.refs.Load() != 0 {
			t.Fatalf("reference underflow at round %d: %d", i, e.refs.Load())
		}
	}
	s := b.Snapshot()
	if s.CompletedEvents != 1000 || s.Delivered+s.TimedOut+s.Cancelled+s.Failed != 1000 || s.ActiveEvents != 0 || s.RetainedBytes != 0 {
		t.Fatalf("atomic completion failed %+v", s)
	}
}

func TestTimedOutHandlerRetainsMemoryAndWorkerSlot(t *testing.T) {
	b := testBus(t, Config{DeliveryTimeout: time.Millisecond, QueueTTL: time.Second})
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	if err := b.Subscribe(Listener{ID: "slow", Types: []string{"slurm.execution.starting"}, QueueSize: 2, Concurrency: 1, Handle: func(context.Context, Event) error { entered <- struct{}{}; <-release; return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), testEvent("stuck/1")); err != nil {
		t.Fatal(err)
	}
	<-entered
	waitFor(t, func() bool { return b.Snapshot().TimedOut == 1 })
	if s := b.Snapshot(); s.ActiveEvents != 0 || s.RetainedBytes <= 0 {
		t.Fatalf("timed out handler memory was not retained in budget %+v", s)
	}
	if _, err := b.Publish(context.Background(), testEvent("stuck/2")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
		t.Fatal("handler exceeded its fixed worker limit")
	case <-time.After(5 * time.Millisecond):
	}
	close(release)
	waitFor(t, func() bool { return b.Snapshot().RetainedBytes == 0 })
	if b.Snapshot().TimedOut+b.Snapshot().Delivered != 2 {
		t.Fatal("delivery was lost")
	}
}

func TestQueueOverflowIsAtomicAndQueueExpiryReleasesAllReferences(t *testing.T) {
	b := testBus(t, Config{DeliveryTimeout: time.Second, QueueTTL: 5 * time.Millisecond, SweepInterval: time.Millisecond})
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int64
	if err := b.Subscribe(Listener{ID: "blocked", Types: []string{"slurm.execution.starting"}, QueueSize: 1, Handle: func(context.Context, Event) error {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), testEvent("queue/1")); err != nil {
		t.Fatal(err)
	}
	<-entered
	if _, err := b.Publish(context.Background(), testEvent("queue/2")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), testEvent("queue/3")); !errors.Is(err, ErrFull) {
		t.Fatalf("expected bounded admission rejection, got %v", err)
	}
	waitFor(t, func() bool { return b.Snapshot().QueueExpired == 1 })
	close(release)
	waitFor(t, func() bool { return b.Snapshot().RetainedBytes == 0 })
	s := b.Snapshot()
	if s.Published != 2 || s.CompletedEvents != 2 || s.Rejected != 1 || calls.Load() != 1 {
		t.Fatalf("queue expiry/overflow %+v calls=%d", s, calls.Load())
	}
}

func TestIdempotenceTypeValidationPanicAndUnsubscribe(t *testing.T) {
	b := testBus(t, Config{})
	if err := b.Subscribe(Listener{ID: "panic", Types: []string{"slurm.execution.starting"}, Handle: func(context.Context, Event) error { panic("deliberate plugin failure") }}); err != nil {
		t.Fatal(err)
	}
	e := testEvent("idempotent/1")
	if _, err := b.Publish(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Snapshot().Panicked == 1 })
	r, err := b.Publish(context.Background(), e)
	if err != nil || !r.Duplicate {
		t.Fatalf("dedup %+v %v", r, err)
	}
	e.Payload = json.RawMessage(`{"job_id":43}`)
	if _, err = b.Publish(context.Background(), e); !errors.Is(err, ErrIDConflict) {
		t.Fatal("ID collision not rejected")
	}
	e = testEvent("wrong-source")
	e.Source = "arbitrary-plugin"
	if _, err = b.Publish(context.Background(), e); !errors.Is(err, ErrInvalidEvent) {
		t.Fatal("reserved source not enforced")
	}
	e = testEvent("unknown-type")
	e.SchemaVersion = 99
	if _, err = b.Publish(context.Background(), e); !errors.Is(err, ErrUnknownType) {
		t.Fatal("unknown schema not rejected")
	}
	if err = b.Unsubscribe("panic"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Snapshot().Listeners == 0 })
	if _, err = b.Publish(context.Background(), testEvent("no-listener")); err != nil {
		t.Fatal(err)
	}
	if s := b.Snapshot(); s.CompletedEvents != 2 || s.ActiveEvents != 0 {
		t.Fatalf("zero listener event leaked %+v", s)
	}
}

func TestCloseBoundedAndCancellationDoesNotDoubleComplete(t *testing.T) {
	b := testBus(t, Config{})
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := b.Subscribe(Listener{ID: "stuck", Types: []string{"slurm.execution.starting"}, Handle: func(context.Context, Event) error { close(entered); <-release; return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), testEvent("shutdown/1")); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := b.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("uncooperative handler did not honor close bound: %v", err)
	}
	if _, err := b.Publish(context.Background(), testEvent("shutdown/2")); !errors.Is(err, ErrClosed) {
		t.Fatal("published after shutdown")
	}
	close(release)
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := b.Snapshot(); s.Cancelled != 1 || s.CompletedEvents != 1 || s.RetainedBytes != 0 {
		t.Fatalf("shutdown leaked or double-completed %+v", s)
	}
}

func TestRemoteDeadlineUpdateAndValidation(t *testing.T) {
	b := testBus(t, Config{})
	if err := b.UpdateConfig(Config{DeliveryTimeout: 2 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(Listener{ID: "updated", Types: []string{"slurm.execution.starting"}, Handle: func(ctx context.Context, _ Event) error { <-ctx.Done(); return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Publish(context.Background(), testEvent("updated/1")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return b.Snapshot().TimedOut == 1 })
	if ValidateConfig(Config{MaxEvents: -1}) == nil {
		t.Fatal("negative capacity accepted")
	}
	if ValidateConfig(Config{DeliveryTimeout: -1}) == nil {
		t.Fatal("negative deadline accepted")
	}
}

func TestOversizedPayloadIsRejectedBeforeValidationAndCopy(t *testing.T) {
	b := testBus(t, Config{MaxPayloadBytes: 128})
	var validations atomic.Int64
	if err := b.RegisterType(Type{ID: "oversize", SchemaVersion: 1, Validate: func(json.RawMessage) error { validations.Add(1); return nil }}); err != nil {
		t.Fatal(err)
	}
	e := testEvent("oversize/1")
	e.Type = "oversize"
	e.Payload = json.RawMessage(`"` + strings.Repeat("x", 8<<20) + `"`)
	if _, err := b.Publish(context.Background(), e); !errors.Is(err, ErrFull) {
		t.Fatalf("oversized body reached parser: %v", err)
	}
	if validations.Load() != 0 || b.Snapshot().RetainedBytes != 0 || b.Snapshot().Published != 0 {
		t.Fatal("oversized body was validated or retained")
	}
}
