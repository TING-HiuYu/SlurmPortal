package telemetry

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// FinishedSpan is a completed span waiting to be exported.
type FinishedSpan struct {
	Name    string
	Context SpanContext
	Parent  [8]byte
	Start   time.Time
	End     time.Time
	Attrs   []Attr
	Err     string
}

type MetricKind int

const (
	MetricSum MetricKind = iota
	MetricHistogram
)

// HistogramBounds are millisecond bucket edges shared by all duration histograms.
var HistogramBounds = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

// MetricPoint is one aggregated counter or observation.
type MetricPoint struct {
	Name    string
	Kind    MetricKind
	Attrs   []Attr
	Sum     float64
	Count   uint64
	Buckets []uint64
	Start   time.Time
	Time    time.Time
}

// Exporter sends buffered spans and metrics to a backend (OTLP, Cloudflare logs).
type Exporter interface {
	ExportSpans(ctx context.Context, spans []FinishedSpan) error
	ExportMetrics(ctx context.Context, points []MetricPoint) error
}

// Buffered is an exporter-agnostic Provider: it buffers bounded batches of sampled spans and exports on Flush.
type Buffered struct {
	Exporter Exporter
	MaxSpans int

	mu      sync.Mutex
	spans   []FinishedSpan
	dropped uint64
	metrics map[string]*MetricPoint
	start   time.Time
}

// NewBuffered returns a provider that keeps at most maxSpans spans until Flush.
func NewBuffered(e Exporter, maxSpans int) *Buffered {
	if maxSpans <= 0 {
		maxSpans = 2048
	}
	return &Buffered{Exporter: e, MaxSpans: maxSpans, metrics: map[string]*MetricPoint{}, start: time.Now()}
}

type recSpan struct {
	b  *Buffered
	fs FinishedSpan
	mu sync.Mutex
	on bool
}

func (s *recSpan) Context() SpanContext { return s.fs.Context }
func (s *recSpan) SetAttributes(a ...Attr) {
	s.mu.Lock()
	s.fs.Attrs = append(s.fs.Attrs, a...)
	s.mu.Unlock()
}
func (s *recSpan) RecordError(err error) {
	s.mu.Lock()
	s.fs.Err = err.Error()
	s.mu.Unlock()
}
func (s *recSpan) End() {
	s.mu.Lock()
	if !s.on {
		s.mu.Unlock()
		return
	}
	s.on = false
	s.fs.End = time.Now()
	fs := s.fs
	s.mu.Unlock()
	s.b.mu.Lock()
	if len(s.b.spans) < s.b.MaxSpans {
		s.b.spans = append(s.b.spans, fs)
	} else {
		s.b.dropped++
	}
	s.b.mu.Unlock()
}

func (b *Buffered) StartSpan(_ context.Context, name string, sc SpanContext, parent [8]byte) Span {
	return &recSpan{b: b, on: sc.Sampled, fs: FinishedSpan{Name: name, Context: sc, Parent: parent, Start: time.Now()}}
}

func metricKey(name string, attrs []Attr) (string, []Attr) {
	a := append([]Attr(nil), attrs...)
	sort.Slice(a, func(i, j int) bool { return a[i].Key < a[j].Key })
	var sb strings.Builder
	sb.WriteString(name)
	for _, x := range a {
		fmt.Fprintf(&sb, "|%s=%v", x.Key, x.Value)
	}
	return sb.String(), a
}

func (b *Buffered) point(name string, kind MetricKind, attrs []Attr) *MetricPoint {
	k, a := metricKey(name, attrs)
	p, ok := b.metrics[k]
	if !ok {
		p = &MetricPoint{Name: name, Kind: kind, Attrs: a, Start: b.start}
		if kind == MetricHistogram {
			p.Buckets = make([]uint64, len(HistogramBounds)+1)
		}
		b.metrics[k] = p
	}
	return p
}

func (b *Buffered) Count(_ context.Context, name string, d int64, attrs ...Attr) {
	b.mu.Lock()
	p := b.point(name, MetricSum, attrs)
	p.Sum += float64(d)
	p.Count++
	b.mu.Unlock()
}

func (b *Buffered) Observe(_ context.Context, name string, v float64, attrs ...Attr) {
	b.mu.Lock()
	p := b.point(name, MetricHistogram, attrs)
	p.Sum += v
	p.Count++
	i := sort.SearchFloat64s(HistogramBounds, v)
	p.Buckets[i]++
	b.mu.Unlock()
}

// Flush exports buffered spans and a cumulative snapshot of all metrics.
func (b *Buffered) Flush(ctx context.Context) error {
	b.mu.Lock()
	spans := b.spans
	b.spans = nil
	if b.dropped > 0 {
		p := b.point("telemetry.spans.dropped", MetricSum, nil)
		p.Sum += float64(b.dropped)
		p.Count++
		b.dropped = 0
	}
	now := time.Now()
	points := make([]MetricPoint, 0, len(b.metrics))
	for _, p := range b.metrics {
		c := *p
		c.Buckets = append([]uint64(nil), p.Buckets...)
		c.Time = now
		points = append(points, c)
	}
	b.mu.Unlock()
	var errs []error
	if len(spans) > 0 {
		if err := b.Exporter.ExportSpans(ctx, spans); err != nil {
			errs = append(errs, err)
		}
	}
	if len(points) > 0 {
		if err := b.Exporter.ExportMetrics(ctx, points); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetry export: %v", errs)
	}
	return nil
}

func (b *Buffered) Shutdown(ctx context.Context) error { return b.Flush(ctx) }
