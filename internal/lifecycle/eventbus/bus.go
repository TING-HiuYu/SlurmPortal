package eventbus

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	queued uint32 = iota
	running
	succeeded
	failed
	timedOut
	cancelled
	queueExpired
	panicked
)

const eventOverhead int64 = 512
const deliveryOverhead int64 = 256

type eventEntry struct {
	event      Event
	refs       atomic.Int64
	deliveries map[string]*delivery
	charge     int64
}

type seenEntry struct {
	fingerprint [32]byte
	until       time.Time
}

type subscription struct {
	spec    Listener
	types   map[string]bool
	queue   chan *delivery
	ctx     context.Context
	cancel  context.CancelFunc
	active  bool // guarded by Bus.mu
	workers atomic.Int64
}

type delivery struct {
	bus      *Bus
	entry    *eventEntry
	listener *subscription
	state    atomic.Uint32
	queuedAt time.Time
	ctx      context.Context
	cancel   context.CancelFunc
	charge   int64
	released sync.Once
}

// Bus is a bounded in-process queue that delivers each published event to every subscribed listener.
type Bus struct {
	mu        sync.Mutex
	cfg       Config
	types     map[string]Type
	listeners map[string]*subscription
	events    map[string]*eventEntry
	seen      map[string]seenEntry
	stats     Stats
	bytes     int64
	ctx       context.Context
	cancel    context.CancelFunc
	closed    bool
	wg        sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
}

// New validates cfg and returns an empty bus.
func New(cfg Config) (*Bus, error) {
	cfg = cfg.WithDefaults()
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bus{cfg: cfg, types: map[string]Type{}, listeners: map[string]*subscription{}, events: map[string]*eventEntry{}, seen: map[string]seenEntry{}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	b.wg.Add(1)
	go b.sweeper()
	return b, nil
}

func typeKey(id string, version int) string { return fmt.Sprintf("%s/%d", id, version) }

// RegisterType declares an event type listeners may subscribe to.
func (b *Bus) RegisterType(t Type) error {
	if !identifier.MatchString(t.ID) || t.SchemaVersion < 1 || len(t.Sources) > 128 {
		return ErrInvalidEvent
	}
	for _, s := range t.Sources {
		if !identifier.MatchString(s) {
			return ErrInvalidEvent
		}
	}
	t.Sources = append([]string(nil), t.Sources...)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	k := typeKey(t.ID, t.SchemaVersion)
	if _, ok := b.types[k]; ok {
		return errors.New("event type already registered")
	}
	b.types[k] = t
	return nil
}

// RegisteredTypes lists the declared event types.
func (b *Bus) RegisteredTypes() []Type {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Type, 0, len(b.types))
	for _, t := range b.types {
		t.Sources = append([]string(nil), t.Sources...)
		t.Validate = nil
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].SchemaVersion < out[j].SchemaVersion
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Subscribe adds a listener for its declared event types.
func (b *Bus) Subscribe(l Listener) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if !identifier.MatchString(l.ID) || l.Handle == nil || len(l.Types) == 0 || len(l.Types) > 128 {
		return errors.New("invalid event listener")
	}
	if _, ok := b.listeners[l.ID]; ok {
		return ErrListenerExists
	}
	if len(b.listeners) >= b.cfg.MaxListeners {
		return ErrFull
	}
	if l.Concurrency == 0 {
		l.Concurrency = b.cfg.ListenerConcurrency
	}
	if l.QueueSize == 0 {
		l.QueueSize = b.cfg.ListenerQueueSize
	}
	if l.Concurrency < 1 || l.Concurrency > 64 || l.QueueSize < 1 || l.QueueSize > 100_000 || l.Timeout < 0 || l.Timeout > 10*time.Minute {
		return errors.New("invalid event listener limits")
	}
	types := make(map[string]bool, len(l.Types))
	for _, t := range l.Types {
		known := false
		for _, registered := range b.types {
			if registered.ID == t {
				known = true
				break
			}
		}
		if !known {
			return ErrUnknownType
		}
		types[t] = true
	}
	l.Types = append([]string(nil), l.Types...)
	ctx, cancel := context.WithCancel(b.ctx)
	s := &subscription{spec: l, types: types, queue: make(chan *delivery, l.QueueSize), ctx: ctx, cancel: cancel, active: true}
	s.workers.Store(int64(l.Concurrency))
	b.listeners[l.ID] = s
	for i := 0; i < l.Concurrency; i++ {
		b.wg.Add(1)
		go b.worker(s)
	}
	return nil
}

// Unsubscribe retires the listener and completes its outstanding deliveries.
// A handler ignoring cancellation retains its worker and its ID until it exits.
func (b *Bus) Unsubscribe(id string) error {
	b.mu.Lock()
	s, ok := b.listeners[id]
	if !ok || !s.active {
		b.mu.Unlock()
		return ErrListenerMissing
	}
	s.active = false
	s.cancel()
	var deliveries []*delivery
	for _, e := range b.events {
		for _, d := range e.deliveries {
			if d.listener == s {
				deliveries = append(deliveries, d)
			}
		}
	}
	b.mu.Unlock()
	for _, d := range deliveries {
		d.finish(cancelled)
	}
	return nil
}

// UpdateConfig changes admission limits and deadlines for new deliveries. Queue
// sizes and worker counts are defaults for listeners registered subsequently.
func (b *Bus) UpdateConfig(cfg Config) error {
	cfg = cfg.WithDefaults()
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	b.cfg = cfg
	return nil
}

func validatePayload(t Type, payload json.RawMessage) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrInvalidEvent
		}
	}()
	if t.Validate != nil {
		return t.Validate(payload)
	}
	return nil
}

// Publish queues event for every matching listener, failing when the queue is full.
func (b *Bus) Publish(ctx context.Context, event Event) (Receipt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if !identifier.MatchString(event.ID) || !identifier.MatchString(event.Type) || !identifier.MatchString(event.Source) || event.SchemaVersion < 1 || len(event.Subject) > 512 || event.OccurredAt.IsZero() {
		return Receipt{}, ErrInvalidEvent
	}
	b.mu.Lock()
	t, ok := b.types[typeKey(event.Type, event.SchemaVersion)]
	limit := b.cfg.MaxPayloadBytes
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return Receipt{}, ErrClosed
	}
	if !ok {
		return Receipt{}, ErrUnknownType
	}
	if len(event.Payload) > limit {
		return Receipt{}, ErrFull
	}
	// Reject oversized input before scanning, validating or copying it. This is
	// also required for in-process publishers, which bypass HTTP body limits.
	if !json.Valid(event.Payload) {
		return Receipt{}, ErrInvalidEvent
	}
	event.OccurredAt = event.OccurredAt.UTC()
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	if len(t.Sources) > 0 {
		allowed := false
		for _, source := range t.Sources {
			if event.Source == source {
				allowed = true
				break
			}
		}
		if !allowed {
			return Receipt{}, ErrInvalidEvent
		}
	}
	if err := validatePayload(t, event.Payload); err != nil {
		return Receipt{}, fmt.Errorf("%w: payload rejected", ErrInvalidEvent)
	}
	wire, _ := json.Marshal(event)
	fingerprint := sha256.Sum256(wire)
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return Receipt{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if len(event.Payload) > b.cfg.MaxPayloadBytes {
		b.stats.Rejected++
		return Receipt{}, ErrFull
	}
	b.expireSeenLocked(now)
	if previous, ok := b.seen[event.ID]; ok {
		if previous.fingerprint != fingerprint {
			b.stats.Rejected++
			return Receipt{}, ErrIDConflict
		}
		b.stats.Duplicates++
		n := 0
		if e := b.events[event.ID]; e != nil {
			n = int(e.refs.Load())
		}
		return Receipt{EventID: event.ID, Duplicate: true, Deliveries: n}, nil
	}
	var subscribers []*subscription
	for _, s := range b.listeners {
		if s.active && s.types[event.Type] {
			subscribers = append(subscribers, s)
		}
	}
	charge := int64(len(wire)) + eventOverhead + int64(len(subscribers))*(int64(len(event.Payload))+deliveryOverhead)
	if len(b.events) >= b.cfg.MaxEvents || charge > b.cfg.MaxBytes-b.bytes || !b.makeSeenRoomLocked() {
		b.stats.Rejected++
		return Receipt{}, ErrFull
	}
	for _, s := range subscribers {
		if len(s.queue) >= cap(s.queue) {
			b.stats.Rejected++
			return Receipt{}, ErrFull
		}
	}
	e := &eventEntry{event: event, deliveries: map[string]*delivery{}, charge: int64(len(wire)) + eventOverhead}
	e.refs.Store(int64(len(subscribers)))
	b.seen[event.ID] = seenEntry{fingerprint: fingerprint, until: now.Add(b.cfg.DedupTTL)}
	b.stats.Published++
	if len(subscribers) == 0 {
		b.stats.CompletedEvents++
		return Receipt{EventID: event.ID}, nil
	}
	b.events[event.ID] = e
	b.bytes += charge
	for _, s := range subscribers {
		dctx, cancel := context.WithCancel(s.ctx)
		d := &delivery{bus: b, entry: e, listener: s, queuedAt: now, ctx: dctx, cancel: cancel, charge: int64(len(event.Payload)) + deliveryOverhead}
		e.deliveries[s.spec.ID] = d
		s.queue <- d
	}
	return Receipt{EventID: event.ID, Deliveries: len(subscribers)}, nil
}

func (b *Bus) expireSeenLocked(now time.Time) {
	for id, entry := range b.seen {
		if now.After(entry.until) && b.events[id] == nil {
			delete(b.seen, id)
		}
	}
}

func (b *Bus) makeSeenRoomLocked() bool {
	if len(b.seen) < b.cfg.MaxDedupEntries {
		return true
	}
	oldest := ""
	var until time.Time
	for id, e := range b.seen {
		if b.events[id] != nil {
			continue
		}
		if oldest == "" || e.until.Before(until) {
			oldest = id
			until = e.until
		}
	}
	if oldest == "" {
		return false
	}
	delete(b.seen, oldest)
	return true
}

func (d *delivery) finish(result uint32) bool {
	var prior uint32
	for {
		prior = d.state.Load()
		if prior >= succeeded {
			return false
		}
		if d.state.CompareAndSwap(prior, result) {
			break
		}
	}
	d.cancel()
	b := d.bus
	b.mu.Lock()
	switch result {
	case succeeded:
		b.stats.Delivered++
	case failed:
		b.stats.Failed++
	case timedOut:
		b.stats.TimedOut++
	case cancelled:
		b.stats.Cancelled++
	case queueExpired:
		b.stats.QueueExpired++
	case panicked:
		b.stats.Panicked++
	}
	delete(d.entry.deliveries, d.listener.spec.ID)
	if d.entry.refs.Add(-1) == 0 {
		delete(b.events, d.entry.event.ID)
		b.bytes -= d.entry.charge
		d.entry.event.Payload = nil
		b.stats.CompletedEvents++
	}
	b.mu.Unlock()
	if prior == queued {
		d.releaseCharge()
	}
	return true
}

func (d *delivery) releaseCharge() {
	d.released.Do(func() { d.bus.mu.Lock(); d.bus.bytes -= d.charge; d.bus.mu.Unlock() })
}

func (b *Bus) worker(s *subscription) {
	defer b.wg.Done()
	defer func() {
		if s.workers.Add(-1) == 0 {
			b.mu.Lock()
			if !s.active && b.listeners[s.spec.ID] == s {
				delete(b.listeners, s.spec.ID)
			}
			b.mu.Unlock()
		}
	}()
	for {
		select {
		case <-s.ctx.Done():
			return
		case d := <-s.queue:
			b.invoke(d)
		}
	}
}

func (b *Bus) invoke(d *delivery) {
	if !d.state.CompareAndSwap(queued, running) {
		return
	}
	defer d.releaseCharge()
	b.mu.Lock()
	if d.state.Load() != running || d.ctx.Err() != nil {
		b.mu.Unlock()
		d.finish(cancelled)
		return
	}
	event := d.entry.event
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	timeout := b.cfg.DeliveryTimeout
	if d.listener.spec.Timeout > 0 && d.listener.spec.Timeout < timeout {
		timeout = d.listener.spec.Timeout
	}
	b.mu.Unlock()
	ctx, cancel := context.WithTimeout(d.ctx, timeout)
	defer cancel()
	timer := time.AfterFunc(timeout, func() { d.finish(timedOut) })
	defer timer.Stop()
	result := succeeded
	func() {
		defer func() {
			if recover() != nil {
				result = panicked
			}
		}()
		if err := d.listener.spec.Handle(ctx, event); err != nil {
			result = failed
		}
	}()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result = timedOut
	} else if d.ctx.Err() != nil {
		result = cancelled
	}
	d.finish(result)
}

func (b *Bus) sweeper() {
	defer b.wg.Done()
	for {
		b.mu.Lock()
		interval := b.cfg.SweepInterval
		b.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-b.ctx.Done():
			timer.Stop()
			return
		case now := <-timer.C:
			b.mu.Lock()
			b.expireSeenLocked(now)
			var expired []*delivery
			for _, entry := range b.events {
				for _, d := range entry.deliveries {
					if d.state.Load() == queued && now.Sub(d.queuedAt) >= b.cfg.QueueTTL {
						expired = append(expired, d)
					}
				}
			}
			b.mu.Unlock()
			for _, d := range expired {
				d.finish(queueExpired)
			}
		}
	}
}

// Snapshot returns delivery counters.
func (b *Bus) Snapshot() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stats
	s.ActiveEvents = len(b.events)
	s.RetainedBytes = b.bytes
	s.DedupEntries = len(b.seen)
	for _, e := range b.events {
		s.ActiveDeliveries += int(e.refs.Load())
	}
	for _, l := range b.listeners {
		if l.active {
			s.Listeners++
		}
	}
	return s
}

// Close stops accepting events and waits for in-flight deliveries until ctx ends.
func (b *Bus) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.cancel()
		var deliveries []*delivery
		for _, s := range b.listeners {
			s.active = false
			s.cancel()
		}
		for _, e := range b.events {
			for _, d := range e.deliveries {
				deliveries = append(deliveries, d)
			}
		}
		b.mu.Unlock()
		for _, d := range deliveries {
			d.finish(cancelled)
		}
		go func() { b.wg.Wait(); close(b.done) }()
	})
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
