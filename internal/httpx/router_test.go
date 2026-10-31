package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
)

type stubAuthn struct {
	p   *authn.Principal
	err error
}

func (s stubAuthn) Authenticate(*http.Request) (*authn.Principal, error) { return s.p, s.err }

type stubAuthz map[string]bool

func (s stubAuthz) Allow(_ context.Context, p *authn.Principal, obj, act string) (bool, error) {
	for _, r := range p.Roles {
		if s[r+"|"+obj+"|"+act] {
			return true, nil
		}
	}
	return false, nil
}

func ok(w http.ResponseWriter, _ *http.Request) error {
	WriteJSON(w, http.StatusOK, "ok")
	return nil
}

func serve(rt *Router, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

func TestHandleRejectsUndeclaredPermission(t *testing.T) {
	rt := NewRouter(stubAuthn{}, stubAuthz{})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for route without permission")
		}
	}()
	rt.Handle(Route{Method: http.MethodGet, Path: "/x", Handler: ok})
}

func TestHandleRejectsPolicyWithoutAuthorizer(t *testing.T) {
	rt := NewRouter(stubAuthn{}, nil)
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for policy route without authorizer")
		}
	}()
	rt.Handle(Route{Method: http.MethodGet, Path: "/x", Permission: authz.Policy("user", "read"), Handler: ok})
}

func TestPermissionMatrix(t *testing.T) {
	user := &authn.Principal{UserID: 2, Username: "u", Roles: []string{"user"}}
	admin := &authn.Principal{UserID: 1, Username: "a", Roles: []string{"admin"}}
	policy := stubAuthz{"admin|user|write": true}
	cases := []struct {
		name string
		an   stubAuthn
		perm authz.Permission
		want int
	}{
		{"public anonymous", stubAuthn{}, authz.Public(), 200},
		{"auth anonymous", stubAuthn{}, authz.Authenticated(), 401},
		{"auth bad token", stubAuthn{err: authn.ErrInvalidToken}, authz.Authenticated(), 401},
		{"auth inactive", stubAuthn{err: authn.ErrInactive}, authz.Authenticated(), 401},
		{"auth backend down", stubAuthn{err: errors.New("db down")}, authz.Authenticated(), 503},
		{"auth ok", stubAuthn{p: user}, authz.Authenticated(), 200},
		{"policy anonymous", stubAuthn{}, authz.Policy("user", "write"), 401},
		{"policy denied", stubAuthn{p: user}, authz.Policy("user", "write"), 403},
		{"policy allowed", stubAuthn{p: admin}, authz.Policy("user", "write"), 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRouter(tc.an, policy)
			rt.Use(WithScope())
			rt.Handle(Route{Method: http.MethodPost, Path: "/r", Permission: tc.perm, Handler: ok})
			got := serve(rt, httptest.NewRequest(http.MethodPost, "/r", nil)).Code
			if got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPrincipalReachesHandlerAndScope(t *testing.T) {
	p := &authn.Principal{UserID: 7, Username: "x"}
	var scope *Scope
	rt := NewRouter(stubAuthn{p: p}, nil)
	rt.Use(WithScope(), func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			scope = ScopeFrom(r.Context())
		})
	})
	rt.Handle(Route{Method: http.MethodGet, Path: "/me", Permission: authz.Authenticated(), Handler: func(w http.ResponseWriter, r *http.Request) error {
		if authn.FromContext(r.Context()).UserID != 7 {
			t.Fatal("principal missing in handler")
		}
		SetTarget(r.Context(), "user:7")
		return ok(w, r)
	}})
	serve(rt, httptest.NewRequest(http.MethodGet, "/me", nil))
	if scope == nil || scope.Principal != p || scope.Route != "GET /me" || scope.Target != "user:7" || scope.Status != 200 {
		t.Fatalf("scope = %+v", scope)
	}
}

type body struct {
	Name string `json:"name"`
}

func (b body) Validate() error {
	if b.Name == "" {
		return errors.New("name required")
	}
	return nil
}

func TestDecodeJSON(t *testing.T) {
	cases := []struct {
		name, ct, body string
		want           int
	}{
		{"ok", "application/json", `{"name":"a"}`, 200},
		{"charset ok", "application/json; charset=utf-8", `{"name":"a"}`, 200},
		{"wrong type", "text/plain", `{"name":"a"}`, 415},
		{"unknown field", "application/json", `{"name":"a","admin":true}`, 400},
		{"trailing data", "application/json", `{"name":"a"}{"name":"b"}`, 400},
		{"validation", "application/json", `{"name":""}`, 400},
		{"too large", "application/json", `{"name":"` + strings.Repeat("a", DefaultMaxBody) + `"}`, 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := NewRouter(nil, nil)
			rt.Handle(Route{Method: http.MethodPost, Path: "/d", Permission: authz.Public(), Handler: func(w http.ResponseWriter, r *http.Request) error {
				var b body
				if err := DecodeJSON(w, r, &b); err != nil {
					return err
				}
				return ok(w, r)
			}})
			req := httptest.NewRequest(http.MethodPost, "/d", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.ct)
			if got := serve(rt, req).Code; got != tc.want {
				t.Fatalf("status = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCORS(t *testing.T) {
	rt := NewRouter(nil, nil)
	rt.Use(CORS([]string{"https://console.example.org"}))
	rt.Handle(Route{Method: http.MethodGet, Path: "/c", Permission: authz.Public(), Handler: ok})

	pre := httptest.NewRequest(http.MethodOptions, "/c", nil)
	pre.Header.Set("Origin", "https://evil.example")
	pre.Header.Set("Access-Control-Request-Method", "GET")
	if rec := serve(rt, pre); rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign preflight: %d %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	pre.Header.Set("Origin", "https://console.example.org")
	if rec := serve(rt, pre); rec.Code != 204 || rec.Header().Get("Access-Control-Allow-Origin") != "https://console.example.org" {
		t.Fatalf("allowed preflight: %d", rec.Code)
	}
}

func TestRequestIDSanitized(t *testing.T) {
	rt := NewRouter(nil, nil)
	rt.Use(WithScope())
	rt.Handle(Route{Method: http.MethodGet, Path: "/i", Permission: authz.Public(), Handler: ok})
	req := httptest.NewRequest(http.MethodGet, "/i", nil)
	req.Header.Set("X-Request-Id", "bad\nvalue")
	if id := serve(rt, req).Header().Get("X-Request-Id"); len(id) != 32 {
		t.Fatalf("unsafe request id echoed: %q", id)
	}
	req.Header.Set("X-Request-Id", "client-req-0001")
	if id := serve(rt, req).Header().Get("X-Request-Id"); id != "client-req-0001" {
		t.Fatalf("valid request id dropped: %q", id)
	}
}

func TestRateLimit(t *testing.T) {
	rt := NewRouter(nil, nil)
	rt.Use(RateLimit(0.0001, 2, RemoteAddrIP))
	rt.Handle(Route{Method: http.MethodGet, Path: "/l", Permission: authz.Public(), Handler: ok})
	codes := []int{}
	for range 3 {
		codes = append(codes, serve(rt, httptest.NewRequest(http.MethodGet, "/l", nil)).Code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 {
		t.Fatalf("codes = %v", codes)
	}
}

func TestRecoverHidesPanic(t *testing.T) {
	rt := NewRouter(nil, nil)
	rt.Use(WithScope(), Recover())
	rt.Handle(Route{Method: http.MethodGet, Path: "/p", Permission: authz.Public(), Handler: func(http.ResponseWriter, *http.Request) error {
		panic("secret detail")
	}})
	rec := serve(rt, httptest.NewRequest(http.MethodGet, "/p", nil))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("panic leaked: %d %s", rec.Code, rec.Body.String())
	}
}
