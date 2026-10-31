package telemetry

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type noopSpan struct{ sc SpanContext }

func (s noopSpan) Context() SpanContext { return s.sc }
func (noopSpan) SetAttributes(...Attr)  {}
func (noopSpan) RecordError(error)      {}
func (noopSpan) End()                   {}

// Noop still propagates trace IDs so logs and audit records correlate without an exporter.
type Noop struct{}

func (Noop) StartSpan(_ context.Context, _ string, sc SpanContext, _ [8]byte) Span {
	return noopSpan{sc: sc}
}
func (Noop) Count(context.Context, string, int64, ...Attr)     {}
func (Noop) Observe(context.Context, string, float64, ...Attr) {}
func (Noop) Shutdown(context.Context) error                    { return nil }

// Multi fans out to several providers sharing the same span IDs.
type Multi []Provider

type multiSpan struct {
	sc    SpanContext
	spans []Span
}

func (m multiSpan) Context() SpanContext { return m.sc }
func (m multiSpan) SetAttributes(a ...Attr) {
	for _, s := range m.spans {
		s.SetAttributes(a...)
	}
}
func (m multiSpan) RecordError(err error) {
	for _, s := range m.spans {
		s.RecordError(err)
	}
}
func (m multiSpan) End() {
	for _, s := range m.spans {
		s.End()
	}
}

func (m Multi) StartSpan(ctx context.Context, name string, sc SpanContext, parent [8]byte) Span {
	ms := multiSpan{sc: sc}
	for _, p := range m {
		ms.spans = append(ms.spans, p.StartSpan(ctx, name, sc, parent))
	}
	return ms
}

func (m Multi) Count(ctx context.Context, name string, d int64, a ...Attr) {
	for _, p := range m {
		p.Count(ctx, name, d, a...)
	}
}

func (m Multi) Observe(ctx context.Context, name string, v float64, a ...Attr) {
	for _, p := range m {
		p.Observe(ctx, name, v, a...)
	}
}

func (m Multi) Flush(ctx context.Context) error {
	var errs []error
	for _, p := range m {
		if f, ok := p.(Flusher); ok {
			errs = append(errs, f.Flush(ctx))
		}
	}
	return errors.Join(errs...)
}

func (m Multi) Shutdown(ctx context.Context) error {
	var errs []error
	for _, p := range m {
		errs = append(errs, p.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

// Start opens a child of the span in ctx, or of remote when ctx has none.
func Start(ctx context.Context, t Tracer, name string, remote SpanContext) (context.Context, Span) {
	parent := CurrentSpanContext(ctx)
	if !parent.Valid() {
		parent = remote
	}
	sc := NextSpanContext(parent)
	s := t.StartSpan(ctx, name, sc, parent.SpanID)
	return ContextWithSpan(ctx, s), s
}

// HTTPMiddleware traces server requests; route returns the matched pattern so paths with IDs don't explode cardinality.
func HTTPMiddleware(p Provider, route func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			remote, _ := ParseTraceparent(r.Header.Get("Traceparent"))
			ctx, span := Start(r.Context(), p, "http.server", remote)
			defer span.End()
			w.Header().Set("Traceparent", span.Context().Traceparent())
			start := time.Now()
			r = r.WithContext(ctx)
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r)
			name := route(r)
			span.SetAttributes(Attr{"http.request.method", r.Method}, Attr{"http.route", name}, Attr{"http.response.status_code", sw.status})
			if sw.status >= 500 {
				span.RecordError(errors.New(http.StatusText(sw.status)))
			}
			attrs := []Attr{{"http.request.method", r.Method}, {"http.route", name}, {"http.response.status_class", sw.status / 100 * 100}}
			p.Observe(ctx, "http.server.duration_ms", float64(time.Since(start).Microseconds())/1000, attrs...)
			p.Count(ctx, "http.server.requests", 1, attrs...)
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
