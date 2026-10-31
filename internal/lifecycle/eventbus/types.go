// Package eventbus implements a bounded, process-local broadcast event queue.
// Events and delivery acknowledgements are never persisted.
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var (
	ErrClosed          = errors.New("event bus is closed")
	ErrFull            = errors.New("event queue capacity exceeded")
	ErrUnknownType     = errors.New("unknown event type or schema")
	ErrInvalidEvent    = errors.New("invalid event")
	ErrIDConflict      = errors.New("event ID was reused with different content")
	ErrListenerExists  = errors.New("listener ID already registered")
	ErrListenerMissing = errors.New("listener is not registered")
	ErrDeliveryTimeout = errors.New("event delivery timed out")
	ErrQueueExpired    = errors.New("event delivery expired in queue")
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@-]{0,255}$`)

// Event is one lifecycle event (for example a job that ended) with its JSON payload.
type Event struct {
	ID            string          `json:"event_id"`
	Type          string          `json:"event_type_id"`
	SchemaVersion int             `json:"schema_version"`
	Subject       string          `json:"subject,omitempty"`
	Source        string          `json:"source"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

// Type is registered by trusted compiled-in code. Its validator must be quick
// and must not mutate or retain the payload. Remote type registration is absent.
type Type struct {
	ID            string
	SchemaVersion int
	Sources       []string
	Validate      func(json.RawMessage) error
}

// Handler processes one event.
type Handler func(context.Context, Event) error

// Listener handlers acknowledge by returning. A handler may transfer ownership
// to its own bounded task queue before returning, but must then own that task's
// cancellation, memory, retry and shutdown lifecycle independently of the event.
type Listener struct {
	ID          string
	Types       []string
	Timeout     time.Duration
	Concurrency int
	QueueSize   int
	Handle      Handler
}

// Config bounds the bus's queue and retries.
type Config struct {
	MaxEvents           int
	MaxBytes            int64
	MaxPayloadBytes     int
	MaxListeners        int
	DeliveryTimeout     time.Duration
	QueueTTL            time.Duration
	DedupTTL            time.Duration
	MaxDedupEntries     int
	ListenerQueueSize   int
	ListenerConcurrency int
	SweepInterval       time.Duration
}

// WithDefaults fills unset limits.
func (c Config) WithDefaults() Config {
	if c.MaxEvents == 0 {
		c.MaxEvents = 1024
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 64 << 20
	}
	if c.MaxPayloadBytes == 0 {
		c.MaxPayloadBytes = 256 << 10
	}
	if c.MaxListeners == 0 {
		c.MaxListeners = 128
	}
	if c.DeliveryTimeout == 0 {
		c.DeliveryTimeout = 5 * time.Second
	}
	if c.QueueTTL == 0 {
		c.QueueTTL = 30 * time.Second
	}
	if c.DedupTTL == 0 {
		c.DedupTTL = 10 * time.Minute
	}
	if c.MaxDedupEntries == 0 {
		c.MaxDedupEntries = 4096
	}
	if c.ListenerQueueSize == 0 {
		c.ListenerQueueSize = 128
	}
	if c.ListenerConcurrency == 0 {
		c.ListenerConcurrency = 1
	}
	if c.SweepInterval == 0 {
		c.SweepInterval = 50 * time.Millisecond
	}
	return c
}

// ValidateConfig rejects limits out of range.
func ValidateConfig(c Config) error {
	c = c.WithDefaults()
	if c.MaxEvents < 1 || c.MaxEvents > 1_000_000 || c.MaxBytes < 1024 || c.MaxBytes > 4<<30 || c.MaxPayloadBytes < 1 || c.MaxPayloadBytes > 16<<20 || int64(c.MaxPayloadBytes) > c.MaxBytes || c.MaxListeners < 1 || c.MaxListeners > 1024 || c.MaxDedupEntries < 1 || c.MaxDedupEntries > 1_000_000 || c.ListenerQueueSize < 1 || c.ListenerQueueSize > 100_000 || c.ListenerConcurrency < 1 || c.ListenerConcurrency > 64 {
		return errors.New("invalid event bus capacity")
	}
	if c.DeliveryTimeout < time.Microsecond || c.DeliveryTimeout > 10*time.Minute || c.QueueTTL < time.Microsecond || c.QueueTTL > time.Hour || c.DedupTTL < time.Microsecond || c.DedupTTL > 24*time.Hour || c.SweepInterval < time.Microsecond || c.SweepInterval > time.Minute {
		return errors.New("invalid event bus timeout")
	}
	return nil
}

// Receipt confirms a published event.
type Receipt struct {
	EventID    string `json:"event_id"`
	Duplicate  bool   `json:"duplicate"`
	Deliveries int    `json:"deliveries"`
}

// Stats deliberately contains no event payloads, job identities or tokens.
type Stats struct {
	Published        uint64 `json:"published"`
	Duplicates       uint64 `json:"duplicates"`
	Rejected         uint64 `json:"rejected"`
	CompletedEvents  uint64 `json:"completed_events"`
	Delivered        uint64 `json:"delivered"`
	Failed           uint64 `json:"failed"`
	TimedOut         uint64 `json:"timed_out"`
	Cancelled        uint64 `json:"cancelled"`
	QueueExpired     uint64 `json:"queue_expired"`
	Panicked         uint64 `json:"panicked"`
	ActiveEvents     int    `json:"active_events"`
	ActiveDeliveries int    `json:"active_deliveries"`
	RetainedBytes    int64  `json:"retained_bytes"`
	Listeners        int    `json:"listeners"`
	DedupEntries     int    `json:"dedup_entries"`
}
