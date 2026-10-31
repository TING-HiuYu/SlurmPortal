package telemetry

import (
	"context"
	"testing"
)

func TestTraceparentRoundTrip(t *testing.T) {
	const v = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	sc, ok := ParseTraceparent(v)
	if !ok || !sc.Sampled || sc.Traceparent() != v {
		t.Fatalf("roundtrip: %v %q", ok, sc.Traceparent())
	}
	for _, bad := range []string{
		"", "garbage",
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-zzf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	} {
		if _, ok := ParseTraceparent(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStartContinuesTrace(t *testing.T) {
	remote, _ := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	ctx, root := Start(context.Background(), Noop{}, "root", remote)
	if root.Context().TraceID != remote.TraceID || root.Context().SpanID == remote.SpanID {
		t.Fatal("root span did not continue remote trace")
	}
	_, child := Start(ctx, Noop{}, "child", SpanContext{})
	if child.Context().TraceID != remote.TraceID || child.Context().SpanID == root.Context().SpanID {
		t.Fatal("child span did not inherit trace")
	}
	_, fresh := Start(context.Background(), Noop{}, "fresh", SpanContext{})
	if !fresh.Context().Valid() || fresh.Context().TraceID == remote.TraceID {
		t.Fatal("fresh trace not generated")
	}
}

type recordingProvider struct {
	Noop
	ids [][8]byte
}

func (r *recordingProvider) StartSpan(ctx context.Context, n string, sc SpanContext, p [8]byte) Span {
	r.ids = append(r.ids, sc.SpanID)
	return r.Noop.StartSpan(ctx, n, sc, p)
}

func TestMultiSharesSpanIDs(t *testing.T) {
	a, b := &recordingProvider{}, &recordingProvider{}
	Start(context.Background(), Multi{a, b}, "x", SpanContext{})
	if len(a.ids) != 1 || a.ids[0] != b.ids[0] {
		t.Fatal("exporters saw different span IDs")
	}
}
