package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

type env struct {
	db  store.Store
	svc *Service
	h   http.Handler
	key *rsa.PrivateKey
}

func legacyEncrypt(t *testing.T, k *rsa.PrivateKey, pw string) string {
	t.Helper()
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, &k.PublicKey, []byte(pw))
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ct)
}

// allowAll lets any signed-in principal through policy routes; permissions are tested elsewhere.
type allowAll struct{}

func (allowAll) Allow(context.Context, *authn.Principal, string, string) (bool, error) {
	return true, nil
}

func setup(t *testing.T) *env {
	t.Helper()
	db := testutil.DB(t)
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	for _, u := range []struct {
		name   string
		pw     string
		status int
	}{{"alice", "abc12345", 1}, {"bob", "bobpass99", 2}, {"corrupt", "", 1}} {
		ct := "not-base64!"
		if u.pw != "" {
			ct = legacyEncrypt(t, key, u.pw)
		}
		id := testutil.User(t, db, u.name, u.status)
		if _, err := db.Exec(context.Background(), "UPDATE users SET password = ? WHERE id = ?", ct, id); err != nil {
			t.Fatal(err)
		}
	}
	rev := Revocations{DB: db, Now: time.Now}
	jwt, err := authn.NewJWT("i", "a", time.Hour, authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}}, identity.Directory{DB: db}, rev)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(db, jwt, rev)
	rt := httpx.NewRouter(jwt, allowAll{})
	rt.Use(httpx.WithScope())
	svc.Register(rt, httpx.RemoteAddrIP)
	return &env{db: db, svc: svc, h: rt.Handler(), key: key}
}

func (e *env) convert(t *testing.T) {
	t.Helper()
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(e.key)})
	dec, err := ParseLegacyKey(pemKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConvertLegacy(context.Background(), e.db, dec, true); err != nil {
		t.Fatal(err)
	}
}

func (e *env) do(method, path, body, token, remote string, secure bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	if secure {
		req.TLS = &tls.ConnectionState{}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *env) login(t *testing.T, user, pw string) (int, string) {
	t.Helper()
	rec := e.do(http.MethodPost, "/api/v2/auth/login", `{"username":"`+user+`","password":"`+pw+`"}`, "", "127.0.0.1:5000", false)
	var body struct {
		Data tokenResponse `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.Data.Token
}

func TestConvertLegacy(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(e.key)})
	dec, _ := ParseLegacyKey(pemKey)

	rep, err := ConvertLegacy(ctx, e.db, dec, false)
	if err != nil || len(rep.Converted) != 2 || len(rep.Failed) != 1 || rep.Failed[3] == "" {
		t.Fatalf("dry-run rep=%+v err=%v", rep, err)
	}
	var n int
	_ = store.QueryRow(ctx, e.db, "SELECT COUNT(*) FROM v2_user_credentials", nil, &n)
	if n != 0 {
		t.Fatal("dry-run wrote credentials")
	}
	if _, err := ConvertLegacy(ctx, e.db, dec, true); err != nil {
		t.Fatal(err)
	}
	var app, ldap, legacy string
	_ = store.QueryRow(ctx, e.db, "SELECT c.app_hash, c.ldap_hash, u.password FROM v2_user_credentials c JOIN users u ON u.id = c.user_id WHERE c.user_id = 1", nil, &app, &ldap, &legacy)
	if ok, _ := password.Verify(app, "abc12345"); !ok || !password.VerifyLDAPCrypt(ldap, "abc12345") {
		t.Fatal("converted hashes do not verify")
	}
	if legacy == "" {
		t.Fatal("legacy ciphertext removed; rollback would break")
	}
	rep, _ = ConvertLegacy(ctx, e.db, dec, true)
	if len(rep.Converted) != 0 || rep.Existing != 2 {
		t.Fatalf("rerun rep=%+v", rep)
	}
}

func TestLogin(t *testing.T) {
	e := setup(t)
	e.convert(t)
	if code, tok := e.login(t, "alice", "abc12345"); code != 200 || tok == "" {
		t.Fatalf("valid login: %d", code)
	}
	for name, c := range map[string][2]string{
		"wrong password": {"alice", "nope"},
		"unknown user":   {"mallory", "abc12345"},
		"disabled user":  {"bob", "bobpass99"},
		"no credential":  {"corrupt", "x"},
	} {
		if code, _ := e.login(t, c[0], c[1]); code != 401 {
			t.Errorf("%s: status %d", name, code)
		}
	}
	body := `{"username":"alice","password":"abc12345"}`
	if code := e.do(http.MethodPost, "/api/v2/auth/login", body, "", "203.0.113.9:4000", false).Code; code != 403 {
		t.Fatalf("plain HTTP from remote peer: %d", code)
	}
	if code := e.do(http.MethodPost, "/api/v2/auth/login", body, "", "203.0.113.9:4000", true).Code; code != 200 {
		t.Fatalf("TLS from remote peer: %d", code)
	}
}

func TestLoginThrottle(t *testing.T) {
	e := setup(t)
	e.convert(t)
	for range 5 {
		e.login(t, "alice", "wrong")
	}
	if code, _ := e.login(t, "alice", "abc12345"); code != 429 {
		t.Fatalf("throttle not applied: %d", code)
	}
	if code, _ := e.login(t, "ALICE", "abc12345"); code != 429 {
		t.Fatal("throttle bypassed via username case")
	}
}

func TestRefreshAndLogoutRevoke(t *testing.T) {
	e := setup(t)
	e.convert(t)
	_, tok := e.login(t, "alice", "abc12345")
	rec := e.do(http.MethodPost, "/api/v2/auth/refresh", "", tok, "127.0.0.1:1", false)
	if rec.Code != 200 {
		t.Fatalf("refresh: %d", rec.Code)
	}
	var body struct {
		Data tokenResponse `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if e.do(http.MethodPost, "/api/v2/auth/refresh", "", tok, "127.0.0.1:1", false).Code != 401 {
		t.Fatal("old token still valid after refresh")
	}
	if e.do(http.MethodPost, "/api/v2/auth/logout", "", body.Data.Token, "127.0.0.1:1", false).Code != 204 {
		t.Fatal("logout failed")
	}
	if e.do(http.MethodPost, "/api/v2/auth/refresh", "", body.Data.Token, "127.0.0.1:1", false).Code != 401 {
		t.Fatal("token valid after logout")
	}
}

func TestChangePassword(t *testing.T) {
	e := setup(t)
	e.convert(t)
	_, tok := e.login(t, "alice", "abc12345")
	put := func(body string) int {
		return e.do(http.MethodPut, "/api/v2/me/password", body, tok, "127.0.0.1:1", false).Code
	}
	if c := put(`{"currentPassword":"bad","newPassword":"a-much-better-pass"}`); c != 401 {
		t.Fatalf("wrong current: %d", c)
	}
	if c := put(`{"currentPassword":"abc12345","newPassword":"short"}`); c != 400 {
		t.Fatalf("weak new: %d", c)
	}
	if c := put(`{"currentPassword":"abc12345","newPassword":"a-much-better-pass","userId":2}`); c != 400 {
		t.Fatalf("extra field accepted: %d", c)
	}
	if c := put(`{"currentPassword":"abc12345","newPassword":"a-much-better-pass"}`); c != 204 {
		t.Fatalf("change: %d", c)
	}
	if code, _ := e.login(t, "alice", "a-much-better-pass"); code != 200 {
		t.Fatal("new password rejected")
	}
	if code, _ := e.login(t, "alice", "abc12345"); code != 401 {
		t.Fatal("old password still accepted")
	}
}
