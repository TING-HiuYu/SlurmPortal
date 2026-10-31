// Package cftelemetry emits spans and metrics as structured JSON lines, which Cloudflare Workers Logs
// (and Logpush) index natively; on the Worker it can also write Analytics Engine data points.
package cftelemetry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

const ID = "telemetry-cloudflare"

// Config configures the Cloudflare telemetry exporter.
type Config struct {
	MaxSpans int `yaml:"max-spans"`
	// AnalyticsEngine writes request metrics to the Worker binding exposed by the edge (Worker only).
	AnalyticsEngine bool `yaml:"analytics-engine"`
}

// Plugin is the telemetry-cloudflare plugin.
type Plugin struct{}

func (Plugin) ID() string           { return ID }
func (Plugin) Roles() []plugin.Role { return []plugin.Role{plugin.RoleMaster, plugin.RoleStandalone} }

func (Plugin) Provider(_ context.Context, role plugin.Role, decode func(any) error) (telemetry.Provider, error) {
	var cfg Config
	if err := decode(&cfg); err != nil {
		return nil, err
	}
	return telemetry.NewBuffered(&Exporter{Out: os.Stdout, Role: string(role), Analytics: cfg.AnalyticsEngine}, cfg.MaxSpans), nil
}

// Exporter writes spans and metrics as JSON log lines (and Analytics Engine points when bound).
type Exporter struct {
	Out       io.Writer
	Role      string
	Analytics bool
	mu        sync.Mutex
}

func flat(a []telemetry.Attr) map[string]any {
	m := make(map[string]any, len(a))
	for _, x := range a {
		m[x.Key] = x.Value
	}
	return m
}

func (e *Exporter) line(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err = e.Out.Write(append(b, '\n'))
	return err
}

func (e *Exporter) ExportSpans(_ context.Context, spans []telemetry.FinishedSpan) error {
	for _, s := range spans {
		rec := map[string]any{
			"type": "span", "name": s.Name, "trace_id": hex.EncodeToString(s.Context.TraceID[:]), "span_id": hex.EncodeToString(s.Context.SpanID[:]),
			"start": s.Start.UTC().Format(time.RFC3339Nano), "duration_ms": float64(s.End.Sub(s.Start).Microseconds()) / 1000,
			"role": e.Role, "attributes": flat(s.Attrs),
		}
		if s.Parent != ([8]byte{}) {
			rec["parent_span_id"] = hex.EncodeToString(s.Parent[:])
		}
		if s.Err != "" {
			rec["error"] = s.Err
		}
		if err := e.line(rec); err != nil {
			return err
		}
		if e.Analytics && s.Parent == ([8]byte{}) {
			writeDataPoint(s)
		}
	}
	return nil
}

func (e *Exporter) ExportMetrics(_ context.Context, points []telemetry.MetricPoint) error {
	for _, p := range points {
		rec := map[string]any{"type": "metric", "name": p.Name, "sum": p.Sum, "count": p.Count, "attributes": flat(p.Attrs), "role": e.Role}
		if p.Kind == telemetry.MetricHistogram {
			rec["buckets"], rec["bounds"] = p.Buckets, telemetry.HistogramBounds
		}
		if err := e.line(rec); err != nil {
			return err
		}
	}
	return nil
}
