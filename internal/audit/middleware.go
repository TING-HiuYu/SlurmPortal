package audit

import (
	"log/slog"
	"net/http"

	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

// Middleware audits every state-changing request, including denied ones.
// Must run inside httpx.WithScope so status, route and principal are visible.
func Middleware(rec *Recorder) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		if !rec.Enabled() {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			s := httpx.ScopeFrom(r.Context())
			if s == nil {
				return
			}
			if s.Routine && s.Status > 0 && s.Status < 400 {
				return
			}
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				if !s.Audited {
					return
				}
			}
			ev := Event{
				Action:    httpx.RouteOf(r),
				Target:    s.Target,
				Status:    s.Status,
				RequestID: s.ID,
				Outcome:   outcome(s.Status),
			}
			if sc := telemetry.CurrentSpanContext(r.Context()); sc.Valid() {
				ev.TraceID = sc.TraceIDString()
			}
			if p := s.Principal; p != nil {
				ev.ActorID, ev.Actor = p.UserID, p.Username
			}
			if err := rec.Record(r.Context(), ev); err != nil {
				slog.ErrorContext(r.Context(), "audit write failed", "err", err, "requestId", s.ID)
			}
		})
	}
}

func outcome(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return OutcomeDenied
	case status >= 400 || status == 0:
		return OutcomeFailure
	default:
		return OutcomeSuccess
	}
}
