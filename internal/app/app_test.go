package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TING-HiuYu/SlurmPortal/internal/audit"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func newTestApp(t *testing.T) (*App, store.Store) {
	t.Helper()
	s := testutil.DB(t)
	testutil.Role(t, s, "disabled-role", 5)
	if _, err := s.Exec(context.Background(), "UPDATE roles SET status = 2 WHERE keyword = 'disabled-role'"); err != nil {
		t.Fatal(err)
	}
	testutil.User(t, s, "admin", 1, "admin")
	testutil.User(t, s, "bob", 1, "user", "disabled-role")
	testutil.User(t, s, "gone", 2, "admin")
	testutil.User(t, s, "deleted", 1, "admin")
	if _, err := s.Exec(context.Background(), "UPDATE users SET deleted_at = '2026-01-01' WHERE username = 'deleted'"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Role:    "master",
		HTTP:    config.HTTP{RatePerSecond: 1000, RateBurst: 1000},
		Auth:    config.Auth{Issuer: "i", Audience: "a", TokenTTL: 3600e9},
		Audit:   config.Audit{Enabled: true},
		JWTKeys: authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
	}
	a, err := New(context.Background(), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}

func call(t *testing.T, a *App, method, path string, userID int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if userID > 0 {
		tok, _, _, err := a.JWT.Issue(userID, authn.SourcePassword)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	a.Handler.ServeHTTP(rec, req)
	return rec
}

func TestMeUsesLiveDirectory(t *testing.T) {
	a, _ := newTestApp(t)
	rec := call(t, a, http.MethodGet, "/api/v2/me", 2)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			Roles []string `json:"roles"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Data.User.Username != "bob" || len(body.Data.Roles) != 1 || body.Data.Roles[0] != "user" {
		t.Fatalf("disabled role leaked or wrong user: %+v", body.Data)
	}
	for _, id := range []int64{3, 4, 99} {
		if code := call(t, a, http.MethodGet, "/api/v2/me", id).Code; code != 401 {
			t.Errorf("user %d: status %d, want 401", id, code)
		}
	}
	if rec.Header().Get("Traceparent") == "" || rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("trace/request id headers missing")
	}
}

func TestMutationsAreAuditedIncludingDenied(t *testing.T) {
	ctx := context.Background()
	a, s := newTestApp(t)
	if err := a.Policies.Replace([]authz.Rule{{Role: "admin", Object: "probe", Action: "update"}}); err != nil {
		t.Fatal(err)
	}
	a.Router.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/probe", Permission: authz.Policy("probe", "update"),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			httpx.SetTarget(r.Context(), "probe:1")
			httpx.WriteJSON(w, http.StatusOK, nil)
			return nil
		}})

	if code := call(t, a, http.MethodPost, "/api/v2/probe", 1).Code; code != 200 {
		t.Fatalf("admin status %d", code)
	}
	if code := call(t, a, http.MethodPost, "/api/v2/probe", 2).Code; code != 403 {
		t.Fatalf("user status %d", code)
	}
	call(t, a, http.MethodGet, "/api/v2/me", 1)

	var recs []audit.Record
	_ = audit.SQLStore{DB: s}.Scan(ctx, func(r audit.Record) error { recs = append(recs, r); return nil })
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (GET must not be audited)", len(recs))
	}
	if recs[0].Outcome != audit.OutcomeSuccess || recs[0].Actor != "admin" || recs[0].Target != "probe:1" || recs[0].Action != "POST /api/v2/probe" || recs[0].TraceID == "" {
		t.Fatalf("first record = %+v", recs[0])
	}
	if recs[1].Outcome != audit.OutcomeDenied || recs[1].Status != 403 || recs[1].Actor != "bob" {
		t.Fatalf("second record = %+v", recs[1])
	}
	if _, err := audit.Verify(ctx, audit.SQLStore{DB: s}); err != nil {
		t.Fatal(err)
	}

	if code := call(t, a, http.MethodGet, "/api/v2/audit", 2).Code; code != 403 {
		t.Fatalf("non-admin audit read: %d", code)
	}
	rec := call(t, a, http.MethodGet, "/api/v2/audit?limit=1", 1)
	var page struct {
		Data []struct {
			Seq  int64  `json:"seq"`
			Hash string `json:"hash"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != 200 || len(page.Data) != 1 || page.Data[0].Seq != 1 || len(page.Data[0].Hash) != 64 {
		t.Fatalf("audit list: %d %s", rec.Code, rec.Body.String())
	}
	verify := func() (bool, int64) {
		var v struct {
			Data struct {
				Intact   bool  `json:"intact"`
				BrokenAt int64 `json:"brokenAt"`
			} `json:"data"`
		}
		_ = json.Unmarshal(call(t, a, http.MethodGet, "/api/v2/audit/verify", 1).Body.Bytes(), &v)
		return v.Data.Intact, v.Data.BrokenAt
	}
	if ok, _ := verify(); !ok {
		t.Fatal("intact chain reported broken")
	}
	if _, err := s.Exec(ctx, "UPDATE v2_audit_log SET actor = 'x' WHERE seq = 1"); err == nil {
		t.Fatal("audit row mutable")
	}
	for _, q := range []string{"DROP TRIGGER v2_audit_log_no_update", "UPDATE v2_audit_log SET actor = 'mallory' WHERE seq = 2"} {
		if _, err := s.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if ok, at := verify(); ok || at != 2 {
		t.Fatalf("tampering not detected: intact=%v at=%d", ok, at)
	}
}

func TestMeAccessListsHeldPolicies(t *testing.T) {
	a, _ := newTestApp(t)
	grants := func(id int64) map[string]bool {
		rec := call(t, a, http.MethodGet, "/api/v2/me/access", id)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data struct {
				Plugins     []string `json:"plugins"`
				Permissions []struct{ Object, Action string }
			} `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Data.Plugins == nil {
			t.Fatal("plugins missing")
		}
		out := map[string]bool{}
		for _, p := range body.Data.Permissions {
			out[p.Object+":"+p.Action] = true
		}
		return out
	}
	admin, bob := grants(1), grants(2)
	if !admin["user:create"] || len(admin) != len(a.Router.Policies()) {
		t.Fatalf("admin grants %v", admin)
	}
	if bob["user:create"] {
		t.Fatalf("plain user holds admin policies: %v", bob)
	}
	if code := call(t, a, http.MethodGet, "/api/v2/me/access", 0).Code; code != 401 {
		t.Fatalf("anonymous: %d", code)
	}
}

// Writes by people republish the sync snapshot; machine reports under /sync/ cannot change it.
func TestTrackWritesSkipsMachineReports(t *testing.T) {
	a := &App{}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := httpx.WithScope()(a.trackWrites(ok))
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodPost, "/api/v2/users", true},
		{http.MethodGet, "/api/v2/users", false},
		{http.MethodPut, "/sync/v2/current-status", false},
		{http.MethodPost, "/sync/v2/web-access/register", false},
	} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		if got := a.TakeWrites(); got != c.want {
			t.Errorf("%s %s: writes=%v, want %v", c.method, c.path, got, c.want)
		}
	}
}
