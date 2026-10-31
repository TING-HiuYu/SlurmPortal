package cftelemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

func TestJSONLines(t *testing.T) {
	var out bytes.Buffer
	b := telemetry.NewBuffered(&Exporter{Out: &out, Role: "master"}, 0)
	ctx, root := telemetry.Start(context.Background(), b, "http.server", telemetry.SpanContext{})
	root.SetAttributes(telemetry.Attr{Key: "http.route", Value: "GET /x"})
	_, child := telemetry.Start(ctx, b, "db.query", telemetry.SpanContext{})
	child.RecordError(context.DeadlineExceeded)
	child.End()
	root.End()
	b.Observe(ctx, "http.server.duration_ms", 12, telemetry.Attr{Key: "http.route", Value: "GET /x"})
	if err := b.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d: %s", len(lines), out.String())
	}
	var c, r, m map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &c)
	_ = json.Unmarshal([]byte(lines[1]), &r)
	_ = json.Unmarshal([]byte(lines[2]), &m)
	if c["trace_id"] != r["trace_id"] || c["parent_span_id"] != r["span_id"] || c["error"] == nil {
		t.Fatalf("span linkage wrong: %v / %v", c, r)
	}
	if m["type"] != "metric" || m["count"].(float64) != 1 {
		t.Fatalf("metric line = %v", m)
	}
}
