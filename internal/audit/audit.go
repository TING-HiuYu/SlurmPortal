// Package audit records operations in an append-only SHA-256 hash chain.
// Enablement is fixed at construction; there is deliberately no runtime switch.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Record is one stored audit entry. Hash covers every other field plus PrevHash, which chains the
// records so that editing or removing one breaks verification.
type Record struct {
	Seq       int64             `json:"seq"`
	Time      time.Time         `json:"time"`
	ActorID   int64             `json:"actorId"`
	Actor     string            `json:"actor"`
	Action    string            `json:"action"`
	Target    string            `json:"target"`
	Outcome   string            `json:"outcome"`
	Status    int               `json:"status"`
	RequestID string            `json:"requestId"`
	TraceID   string            `json:"traceId"`
	Detail    map[string]string `json:"detail,omitempty"`
	PrevHash  string            `json:"prevHash"`
	Hash      string            `json:"-"`
}

const (
	OutcomeSuccess = "success"
	OutcomeDenied  = "denied"
	OutcomeFailure = "failure"
)

// Event is the caller-supplied part of a record; chain fields are filled by the recorder.
type Event struct {
	ActorID   int64
	Actor     string
	Action    string
	Target    string
	Outcome   string
	Status    int
	RequestID string
	TraceID   string
	Detail    map[string]string
}

// Head is the sequence number and hash of the newest record, which the next record links to.
type Head struct {
	Seq  int64
	Hash string
}

// Store persists the audit chain (SQLStore in production, MemoryStore in tests).
type Store interface {
	// Append must read the head and insert the built record atomically.
	Append(ctx context.Context, build func(Head) (Record, error)) (Record, error)
	Scan(ctx context.Context, fn func(Record) error) error
}

// GenesisHash anchors the chain so an empty prefix cannot be forged.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ComputeHash returns the SHA-256 of the record's JSON form with Hash left empty.
func ComputeHash(r Record) (string, error) {
	r.Hash = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

var sensitiveKeys = []string{"password", "passwd", "secret", "token", "private", "credential", "cookie", "authorization", "key"}

func redact(detail map[string]string) map[string]string {
	if len(detail) == 0 {
		return nil
	}
	out := make(map[string]string, len(detail))
	for k, v := range detail {
		lk := strings.ToLower(k)
		for _, s := range sensitiveKeys {
			if strings.Contains(lk, s) {
				v = "[redacted]"
				break
			}
		}
		out[k] = v
	}
	return out
}

// Recorder appends events to the chain; a disabled Recorder accepts and drops them.
type Recorder struct {
	enabled bool
	store   Store
	now     func() time.Time
}

// NewRecorder returns a Recorder; an enabled one needs a store.
func NewRecorder(enabled bool, store Store) (*Recorder, error) {
	if enabled && store == nil {
		return nil, errors.New("audit enabled without a store")
	}
	return &Recorder{enabled: enabled, store: store, now: time.Now}, nil
}

// Enabled reports whether events are stored. It is safe on a nil Recorder.
func (r *Recorder) Enabled() bool { return r != nil && r.enabled }

// Record appends ev to the chain, stamping time, sequence and hashes.
func (r *Recorder) Record(ctx context.Context, ev Event) error {
	if !r.Enabled() {
		return nil
	}
	if ev.Action == "" || ev.Outcome == "" {
		return errors.New("audit event needs action and outcome")
	}
	_, err := r.store.Append(ctx, func(h Head) (Record, error) {
		rec := Record{
			Seq:       h.Seq + 1,
			Time:      r.now().UTC().Truncate(time.Microsecond),
			ActorID:   ev.ActorID,
			Actor:     ev.Actor,
			Action:    ev.Action,
			Target:    ev.Target,
			Outcome:   ev.Outcome,
			Status:    ev.Status,
			RequestID: ev.RequestID,
			TraceID:   ev.TraceID,
			Detail:    redact(ev.Detail),
			PrevHash:  h.Hash,
		}
		if rec.PrevHash == "" {
			rec.PrevHash = GenesisHash
		}
		var err error
		rec.Hash, err = ComputeHash(rec)
		return rec, err
	})
	return err
}

// BrokenChainError is returned by Verify for the first record that does not link or hash correctly.
type BrokenChainError struct {
	Seq    int64
	Reason string
}

func (e *BrokenChainError) Error() string {
	return fmt.Sprintf("audit chain broken at seq %d: %s", e.Seq, e.Reason)
}

// Verify walks the chain and reports the first inconsistency. A bounded log has dropped its oldest
// records, so a chain starting after seq 1 is anchored at its first record's previous hash.
func Verify(ctx context.Context, s Store) (int64, error) {
	prev := Head{Hash: GenesisHash}
	first := true
	err := s.Scan(ctx, func(rec Record) error {
		if first && rec.Seq > 1 {
			prev = Head{Seq: rec.Seq - 1, Hash: rec.PrevHash}
		}
		first = false
		if rec.Seq != prev.Seq+1 {
			return &BrokenChainError{Seq: rec.Seq, Reason: "sequence gap"}
		}
		if rec.PrevHash != prev.Hash {
			return &BrokenChainError{Seq: rec.Seq, Reason: "previous hash mismatch"}
		}
		h, err := ComputeHash(rec)
		if err != nil {
			return err
		}
		if h != rec.Hash {
			return &BrokenChainError{Seq: rec.Seq, Reason: "content hash mismatch"}
		}
		prev = Head{Seq: rec.Seq, Hash: rec.Hash}
		return nil
	})
	return prev.Seq, err
}

// MemoryStore is an in-process Store for tests.
type MemoryStore struct {
	mu   sync.Mutex
	recs []Record
}

func (m *MemoryStore) Append(_ context.Context, build func(Head) (Record, error)) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var h Head
	if n := len(m.recs); n > 0 {
		h = Head{Seq: m.recs[n-1].Seq, Hash: m.recs[n-1].Hash}
	}
	rec, err := build(h)
	if err != nil {
		return Record{}, err
	}
	m.recs = append(m.recs, rec)
	return rec, nil
}

func (m *MemoryStore) Scan(_ context.Context, fn func(Record) error) error {
	m.mu.Lock()
	recs := append([]Record(nil), m.recs...)
	m.mu.Unlock()
	for _, r := range recs {
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}
