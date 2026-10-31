package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

// Scope is mutable per-request state visible to outer middleware after the handler returns.
type Scope struct {
	ID        string
	Route     string
	Principal *authn.Principal
	Status    int
	Target    string
	// Audited forces auditing of a safe-method request (e.g. an OAuth callback that logs a user in).
	Audited bool
	// Routine is set from the matched route; successful routine requests are not audited.
	Routine bool
}

type scopeKey struct{}

// ScopeFrom returns the per-request scope set by WithScope.
func ScopeFrom(ctx context.Context) *Scope {
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}

// RequestID returns the request's ID, or "" outside a request.
func RequestID(ctx context.Context) string {
	if s := ScopeFrom(ctx); s != nil {
		return s.ID
	}
	return ""
}

// RouteOf returns the matched route pattern, or "unmatched".
func RouteOf(r *http.Request) string {
	if s := ScopeFrom(r.Context()); s != nil && s.Route != "" {
		return s.Route
	}
	return "unmatched"
}

// SetTarget records the business object affected by the request for auditing.
func SetTarget(ctx context.Context, target string) {
	if s := ScopeFrom(ctx); s != nil {
		s.Target = target
	}
}

// MarkAudited records this request in the audit log even though its method is safe.
func MarkAudited(ctx context.Context) {
	if s := ScopeFrom(ctx); s != nil {
		s.Audited = true
	}
}

func setPrincipal(ctx context.Context, p *authn.Principal) {
	if s := ScopeFrom(ctx); s != nil {
		s.Principal = p
	}
}

type statusWriter struct {
	http.ResponseWriter
	scope *Scope
	wrote bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.scope.Status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.scope.Status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// WithScope must be the outermost middleware.
func WithScope() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-Id")
			if !requestIDPattern.MatchString(id) {
				id = newID()
			}
			s := &Scope{ID: id}
			w.Header().Set("X-Request-Id", id)
			next.ServeHTTP(&statusWriter{ResponseWriter: w, scope: s}, r.WithContext(context.WithValue(r.Context(), scopeKey{}, s)))
		})
	}
}

// Recover turns a handler panic into a logged 500 response.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					if v == http.ErrAbortHandler {
						panic(v)
					}
					slog.ErrorContext(r.Context(), "panic", "value", v, "stack", string(debug.Stack()), "requestId", RequestID(r.Context()))
					WriteError(w, r, apperr.ErrInternal)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets nosniff, no framing, no referrer, no-store and a deny-all CSP on every
// API response.
func SecurityHeaders() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cache-Control", "no-store")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			next.ServeHTTP(w, r)
		})
	}
}

// CORS allows only exact configured origins; an empty list disables cross-origin access.
func CORS(origins []string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			allowed := origin != "" && slices.Contains(origins, origin)
			if allowed {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id, Traceparent")
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
				h.Set("Access-Control-Max-Age", "600")
				h.Add("Vary", "Origin")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if allowed {
					w.WriteHeader(http.StatusNoContent)
				} else {
					WriteError(w, r, apperr.ErrForbidden)
				}
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP extracts the caller address; only trust a header set by the edge you control.
type ClientIP func(r *http.Request) string

// RemoteAddrIP returns the client IP from the TCP connection.
func RemoteAddrIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// TrustedHeaderIP reads the client IP from a header set by a trusted proxy (e.g. CF-Connecting-IP),
// falling back to the connection address.
func TrustedHeaderIP(header string) ClientIP {
	return func(r *http.Request) string {
		if v := strings.TrimSpace(r.Header.Get(header)); net.ParseIP(v) != nil {
			return v
		}
		return RemoteAddrIP(r)
	}
}

type bucket struct {
	tokens float64
	last   time.Time
}

// RateLimit is a per-client token bucket; buckets idle for 10 minutes are dropped.
func RateLimit(perSecond float64, burst int, ip ClientIP) Middleware {
	var mu sync.Mutex
	buckets := map[string]*bucket{}
	lastSweep := time.Now()
	allow := func(key string, now time.Time) bool {
		mu.Lock()
		defer mu.Unlock()
		if now.Sub(lastSweep) > time.Minute {
			for k, b := range buckets {
				if now.Sub(b.last) > 10*time.Minute {
					delete(buckets, k)
				}
			}
			lastSweep = now
		}
		b, ok := buckets[key]
		if !ok {
			b = &bucket{tokens: float64(burst), last: now}
			buckets[key] = b
		}
		b.tokens = min(float64(burst), b.tokens+now.Sub(b.last).Seconds()*perSecond)
		b.last = now
		if b.tokens < 1 {
			return false
		}
		b.tokens--
		return true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allow(ip(r), time.Now()) {
				w.Header().Set("Retry-After", "1")
				WriteError(w, r, apperr.ErrRateLimited)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
