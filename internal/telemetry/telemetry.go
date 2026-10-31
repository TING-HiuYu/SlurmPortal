// Package telemetry is the vendor-neutral tracing/metrics surface; exporters are plugins.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strings"
	"sync/atomic"
)

// SpanContext identifies a span within a trace (W3C trace context).
type SpanContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

func (s SpanContext) Valid() bool { return s.TraceID != [16]byte{} && s.SpanID != [8]byte{} }

func (s SpanContext) TraceIDString() string { return hex.EncodeToString(s.TraceID[:]) }

// Traceparent formats a W3C trace context header value.
func (s SpanContext) Traceparent() string {
	flags := "00"
	if s.Sampled {
		flags = "01"
	}
	return "00-" + hex.EncodeToString(s.TraceID[:]) + "-" + hex.EncodeToString(s.SpanID[:]) + "-" + flags
}

// ParseTraceparent reads a W3C traceparent header.
func ParseTraceparent(v string) (SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return SpanContext{}, false
	}
	var sc SpanContext
	if _, err := hex.Decode(sc.TraceID[:], []byte(parts[1])); err != nil {
		return SpanContext{}, false
	}
	if _, err := hex.Decode(sc.SpanID[:], []byte(parts[2])); err != nil {
		return SpanContext{}, false
	}
	flags, err := hex.DecodeString(parts[3])
	if err != nil || !sc.Valid() {
		return SpanContext{}, false
	}
	sc.Sampled = flags[0]&1 == 1
	return sc, true
}

func newSpanID() (id [8]byte) {
	_, _ = rand.Read(id[:])
	return
}

func newTraceID() (id [16]byte) {
	_, _ = rand.Read(id[:])
	return
}

// Attr is a span or metric attribute.
type Attr struct {
	Key   string
	Value any
}

// Span is an in-progress operation.
type Span interface {
	Context() SpanContext
	SetAttributes(attrs ...Attr)
	RecordError(err error)
	End()
}

// Tracer starts spans.
type Tracer interface {
	// StartSpan must use sc as-is so every exporter reports identical IDs.
	StartSpan(ctx context.Context, name string, sc SpanContext, parentSpanID [8]byte) Span
}

// Meter records counters and observations.
type Meter interface {
	Count(ctx context.Context, name string, delta int64, attrs ...Attr)
	Observe(ctx context.Context, name string, value float64, attrs ...Attr)
}

// Provider is the telemetry backend plugins and middleware write to; Noop discards everything.
type Provider interface {
	Tracer
	Meter
	Shutdown(ctx context.Context) error
}

type spanKey struct{}

// ContextWithSpan stores s in ctx.
func ContextWithSpan(ctx context.Context, s Span) context.Context {
	return context.WithValue(ctx, spanKey{}, s)
}

// SpanFrom returns the span in ctx, if any.
func SpanFrom(ctx context.Context) Span {
	s, _ := ctx.Value(spanKey{}).(Span)
	return s
}

// CurrentSpanContext returns the span context in ctx, or a zero value.
func CurrentSpanContext(ctx context.Context) SpanContext {
	if s := SpanFrom(ctx); s != nil {
		return s.Context()
	}
	return SpanContext{}
}

// NextSpanContext continues parent's trace with a fresh span ID, or starts a new trace sampled at the root ratio.
func NextSpanContext(parent SpanContext) SpanContext {
	if parent.Valid() {
		return SpanContext{TraceID: parent.TraceID, SpanID: newSpanID(), Sampled: parent.Sampled}
	}
	return SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: sampleRoot()}
}

var rootRatio atomic.Uint64

func init() { SetSampleRatio(1) }

// SetSampleRatio sets the probability that a new root trace is sampled; children inherit the decision.
func SetSampleRatio(r float64) { rootRatio.Store(math.Float64bits(min(max(r, 0), 1))) }

func sampleRoot() bool {
	r := math.Float64frombits(rootRatio.Load())
	if r >= 1 {
		return true
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11)/float64(1<<53) < r
}
