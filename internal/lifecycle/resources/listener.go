package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
)

const ReadyType = "slurm.execution.resources.ready"
const EndingType = "slurm.execution.ending"

// Listener owns the resource plugin's event-driven watch lifecycle. The host
// produces verified Allocation payloads and owns the shared sampling ticker;
// this listener performs no network work and can acknowledge quickly.
type Listener struct {
	mu            sync.Mutex
	collector     *Collector
	tombstones    map[string]time.Time
	maxTombstones int
	ttl           time.Duration
	now           func() time.Time
}

// NewListener returns a listener feeding collector; it remembers maxTombstones finished jobs so
// late events are ignored.
func NewListener(collector *Collector, maxTombstones int) *Listener {
	if maxTombstones < 1 {
		maxTombstones = 4096
	}
	if maxTombstones > 1_000_000 {
		maxTombstones = 1_000_000
	}
	return &Listener{collector: collector, tombstones: map[string]time.Time{}, maxTombstones: maxTombstones, ttl: 24 * time.Hour, now: time.Now}
}

// Handle acknowledges by returning. Ending creates a bounded tombstone before
// pausing a watch, preventing a concurrently delivered or late Ready event from
// restarting this execution while its tombstone remains retained.
func (l *Listener) Handle(ctx context.Context, event eventbus.Event) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if l.collector == nil || event.SchemaVersion != 1 || event.Subject == "" || len(event.Subject) > 512 {
		return errors.New("invalid resource lifecycle event")
	}
	var allocation Allocation
	switch event.Type {
	case ReadyType:
		decoder := json.NewDecoder(bytes.NewReader(event.Payload))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&allocation) != nil || decoder.Decode(new(any)) != io.EOF || allocation.Key != event.Subject {
			return errors.New("invalid resource allocation event")
		}
	case EndingType:
	default:
		return errors.New("unsupported resource lifecycle event")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := l.now()
	for key, expiry := range l.tombstones {
		if !now.Before(expiry) {
			delete(l.tombstones, key)
		}
	}
	if event.Type == ReadyType {
		if _, ended := l.tombstones[event.Subject]; ended {
			return nil
		}
		return l.collector.Start(allocation)
	}
	if _, exists := l.tombstones[event.Subject]; !exists {
		if len(l.tombstones) >= l.maxTombstones {
			oldest := ""
			var until time.Time
			for key, expiry := range l.tombstones {
				if oldest == "" || expiry.Before(until) {
					oldest = key
					until = expiry
				}
			}
			delete(l.tombstones, oldest)
		}
		l.tombstones[event.Subject] = now.Add(l.ttl)
	}
	at := event.OccurredAt
	if at.IsZero() {
		at = now
	}
	l.collector.Pause(event.Subject, true, at)
	return nil
}
