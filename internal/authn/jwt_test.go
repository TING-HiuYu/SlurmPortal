package authn

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type dir map[int64]*Principal

func (d dir) LoadPrincipal(_ context.Context, id int64) (*Principal, error) {
	p, ok := d[id]
	if !ok {
		return nil, ErrInactive
	}
	return p, nil
}

type revoked map[string]bool

func (r revoked) IsRevoked(_ context.Context, id string) (bool, error) { return r[id], nil }

var testKey = []byte("0123456789abcdef0123456789abcdef")

func newTestJWT(t *testing.T, rev Revocations) *JWT {
	t.Helper()
	j, err := NewJWT("iss", "aud", time.Hour, Keyring{ActiveKID: "k1", Keys: map[string][]byte{"k1": testKey}},
		dir{1: {UserID: 1, Username: "alice", Roles: []string{"admin"}}}, rev)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func authReq(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestJWTRoundTrip(t *testing.T) {
	j := newTestJWT(t, nil)
	tok, id, _, err := j.Issue(1, SourcePassword)
	if err != nil {
		t.Fatal(err)
	}
	p, err := j.Authenticate(authReq(tok))
	if err != nil || p.UserID != 1 || p.TokenID != id || p.Source != SourcePassword || !p.HasRole("admin") {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestJWTNoCredentials(t *testing.T) {
	p, err := newTestJWT(t, nil).Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if p != nil || err != nil {
		t.Fatalf("p=%v err=%v", p, err)
	}
}

func sign(t *testing.T, method jwt.SigningMethod, key any, kid string, c jwt.RegisteredClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, c)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJWTRejects(t *testing.T) {
	now := time.Now()
	good := jwt.RegisteredClaims{Issuer: "iss", Subject: "1", Audience: jwt.ClaimStrings{"aud"}, ID: "x",
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}
	with := func(f func(*jwt.RegisteredClaims)) jwt.RegisteredClaims { c := good; f(&c); return c }

	j := newTestJWT(t, revoked{"revoked-id": true})
	valid := sign(t, jwt.SigningMethodHS256, testKey, "k1", good)
	parts := strings.Split(valid, ".")
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"k1"}`))

	cases := map[string]string{
		"alg none":       noneHeader + "." + parts[1] + ".",
		"tampered":       parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"2"}`)) + "." + parts[2],
		"wrong key":      sign(t, jwt.SigningMethodHS256, []byte("ffffffffffffffffffffffffffffffff"), "k1", good),
		"unknown kid":    sign(t, jwt.SigningMethodHS256, testKey, "k9", good),
		"hs512":          sign(t, jwt.SigningMethodHS512, testKey, "k1", good),
		"wrong audience": sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other"} })),
		"wrong issuer":   sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.Issuer = "evil" })),
		"expired":        sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute)) })),
		"no expiry":      sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil })),
		"no jti":         sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.ID = "" })),
		"bad subject":    sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.Subject = "0" })),
		"revoked":        sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.ID = "revoked-id" })),
		"disabled user":  sign(t, jwt.SigningMethodHS256, testKey, "k1", with(func(c *jwt.RegisteredClaims) { c.Subject = "99" })),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if p, err := j.Authenticate(authReq(tok)); err == nil || p != nil {
				t.Fatalf("accepted: %+v", p)
			}
		})
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Basic abc")
	if _, err := j.Authenticate(r); err != ErrInvalidToken {
		t.Fatalf("basic auth err = %v", err)
	}
}

func TestKeyringValidation(t *testing.T) {
	d := dir{}
	if _, err := NewJWT("i", "a", time.Hour, Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("short")}}, d, nil); err == nil {
		t.Fatal("short key accepted")
	}
	if _, err := NewJWT("i", "a", time.Hour, Keyring{ActiveKID: "missing", Keys: map[string][]byte{"k": testKey}}, d, nil); err == nil {
		t.Fatal("missing active key accepted")
	}
}

func TestKeyRotationAcceptsPreviousKey(t *testing.T) {
	old := newTestJWT(t, nil)
	tok, _, _, _ := old.Issue(1, SourcePassword)
	rotated := newTestJWT(t, nil)
	rotated.Keys = Keyring{ActiveKID: "k2", Keys: map[string][]byte{"k1": testKey, "k2": []byte("abcdefghijklmnopqrstuvwxyz012345")}}
	if _, err := rotated.Authenticate(authReq(tok)); err != nil {
		t.Fatalf("old token rejected after rotation: %v", err)
	}
}
