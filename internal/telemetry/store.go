package telemetry

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Flusher is implemented by exporters that buffer; edges without background time (Workers) call it per request.
type Flusher interface {
	Flush(ctx context.Context) error
}

// Inject propagates the current span to an outbound request.
func Inject(ctx context.Context, req *http.Request) {
	if sc := CurrentSpanContext(ctx); sc.Valid() {
		req.Header.Set("Traceparent", sc.Traceparent())
	}
}

// TracedStore records a span per database call; statement text is kept, bound arguments never are.
type TracedStore struct {
	store.Store
	Provider Provider
}

func statement(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 200 {
		q = q[:200]
	}
	return q
}

func (t TracedStore) span(ctx context.Context, op, q string) (context.Context, Span, time.Time) {
	ctx, s := Start(ctx, t.Provider, "db."+op, SpanContext{})
	s.SetAttributes(Attr{"db.system", "sqlite"}, Attr{"db.statement", statement(q)})
	return ctx, s, time.Now()
}

func (t TracedStore) end(ctx context.Context, s Span, op string, start time.Time, err error) {
	if err != nil {
		s.RecordError(err)
	}
	s.End()
	t.Provider.Observe(ctx, "db.client.duration_ms", float64(time.Since(start).Microseconds())/1000, Attr{"db.operation", op})
}

func (t TracedStore) Query(ctx context.Context, q string, args ...any) (store.Rows, error) {
	ctx, s, start := t.span(ctx, "query", q)
	rows, err := t.Store.Query(ctx, q, args...)
	t.end(ctx, s, "query", start, err)
	return rows, err
}

func (t TracedStore) Exec(ctx context.Context, q string, args ...any) (store.Result, error) {
	ctx, s, start := t.span(ctx, "exec", q)
	res, err := t.Store.Exec(ctx, q, args...)
	t.end(ctx, s, "exec", start, err)
	return res, err
}

func (t TracedStore) Batch(ctx context.Context, stmts []store.Statement) ([]store.Result, error) {
	first := ""
	if len(stmts) > 0 {
		first = stmts[0].Query
	}
	ctx, s, start := t.span(ctx, "batch", first)
	s.SetAttributes(Attr{"db.batch.size", len(stmts)})
	res, err := t.Store.Batch(ctx, stmts)
	t.end(ctx, s, "batch", start, err)
	return res, err
}
