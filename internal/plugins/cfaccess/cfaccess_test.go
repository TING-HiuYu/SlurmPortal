package cfaccess

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

func init() { plugin.Register(Plugin{}) }

const (
	team = "lab.cloudflareaccess.com"
	aud  = "0123456789abcdef0123456789abcdef"
)

type env struct {
	t     *testing.T
	app   *app.App
	db    store.Store
	key   *rsa.PrivateKey
	other *rsa.PrivateKey
	hits  *int
}

func setup(t *testing.T) env {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	hits := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": "k1", "kty": "RSA", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(srv.Close)
	old := certsURL
	certsURL = func(string) string { return srv.URL }
	t.Cleanup(func() { certsURL = old })

	db := testutil.DB(t)
	testutil.User(t, db, "admin", 1, "admin")
	alice := testutil.User(t, db, "alice", 1, "user")
	testutil.User(t, db, "carol", 2, "user")
	ctx := context.Background()
	_, _ = db.Exec(ctx, "UPDATE users SET mail = 'Alice@Example.com' WHERE id = ?", alice)
	_, _ = db.Exec(ctx, "UPDATE users SET mail = 'carol@example.com' WHERE username = 'carol'")

	var cfg config.Config
	if err := config.Parse([]byte(`
plugins:
  enabled: [cloudflare-access]
  config:
    cloudflare-access:
      team-domain: `+team+`
      applications:
        - origin: https://admin.example
          path-prefix: /api/
          aud: `+aud+`
`), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Role = "master"
	cfg.HTTP = config.HTTP{RatePerSecond: 1000, RateBurst: 1000}
	cfg.Auth = config.Auth{Issuer: "i", Audience: "a", TokenTTL: 12 * time.Hour}
	cfg.JWTKeys = authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}}
	a, err := app.New(ctx, &cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	return env{t, a, db, key, other, hits}
}

func (e env) assertion(key *rsa.PrivateKey, mutate func(jwt.MapClaims)) string {
	now := time.Now()
	c := jwt.MapClaims{"iss": "https://" + team, "aud": []string{aud}, "email": "alice@example.com", "type": "app",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "sub": "abc"}
	if mutate != nil {
		mutate(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(key)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e env) do(method, path, assertion, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "https://admin.example"+path, nil)
	if assertion != "" {
		req.Header.Set(AssertionHeader, assertion)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(rec, req)
	return rec
}

func (e env) session(assertion string) (string, time.Time, int) {
	rec := e.do("POST", "/api/v2/auth/access/session", assertion, "")
	var body struct {
		Data struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Data.Token, body.Data.ExpiresAt, rec.Code
}

func TestSessionIsBoundToAccessAssertion(t *testing.T) {
	e := setup(t)
	short := e.assertion(e.key, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(10 * time.Minute).Unix() })
	tok, exp, code := e.session(short)
	if code != 200 || tok == "" {
		t.Fatalf("session %d", code)
	}
	if time.Until(exp) > 11*time.Minute {
		t.Fatalf("session outlives the Access token: %s", exp)
	}
	if rec := e.do("GET", "/api/v2/me", short, tok); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"alice"`) {
		t.Fatalf("me with assertion: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/v2/me", "", tok); rec.Code != 401 {
		t.Fatalf("token replayed without Access: %d", rec.Code)
	}
	bob := e.assertion(e.key, func(c jwt.MapClaims) { c["email"] = "admin@example.com" })
	if rec := e.do("GET", "/api/v2/me", bob, tok); rec.Code != 401 {
		t.Fatalf("token used under another Access identity: %d", rec.Code)
	}
	if rec := e.do("GET", "/api/v2/auth/access", short, ""); !strings.Contains(rec.Body.String(), `"available":true`) {
		t.Fatalf("capability: %s", rec.Body.String())
	}
	if *e.hits != 1 {
		t.Fatalf("JWKS fetched %d times, want cached", *e.hits)
	}
}

func TestRejectsBadAssertions(t *testing.T) {
	e := setup(t)
	cases := map[string]string{
		"forged signature": e.assertion(e.other, nil),
		"wrong audience":   e.assertion(e.key, func(c jwt.MapClaims) { c["aud"] = []string{"ffffffffffffffffffffffffffffffff"} }),
		"wrong issuer":     e.assertion(e.key, func(c jwt.MapClaims) { c["iss"] = "https://evil.cloudflareaccess.com" }),
		"expired":          e.assertion(e.key, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }),
		"service token":    e.assertion(e.key, func(c jwt.MapClaims) { c["type"] = "org" }),
		"no email":         e.assertion(e.key, func(c jwt.MapClaims) { delete(c, "email") }),
		"unknown user":     e.assertion(e.key, func(c jwt.MapClaims) { c["email"] = "nobody@example.com" }),
		"disabled user":    e.assertion(e.key, func(c jwt.MapClaims) { c["email"] = "carol@example.com" }),
		"missing":          "",
	}
	for name, a := range cases {
		if tok, _, code := e.session(a); code < 400 || tok != "" {
			t.Errorf("%s: status %d", name, code)
		}
	}
	// HS256 with the RSA public key as secret (algorithm confusion) must fail.
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "https://" + team, "aud": aud, "email": "alice@example.com",
		"type": "app", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	hs.Header["kid"] = "k1"
	s, _ := hs.SignedString(e.key.N.Bytes())
	if _, _, code := e.session(s); code != 401 {
		t.Errorf("alg confusion: %d", code)
	}
	// An assertion for this app is not accepted on a path outside the application.
	req := httptest.NewRequest("POST", "https://other.example/api/v2/auth/access/session", nil)
	req.Header.Set(AssertionHeader, e.assertion(e.key, nil))
	rec := httptest.NewRecorder()
	e.app.Handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("foreign origin: %d", rec.Code)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []Config{
		{},
		{TeamDomain: "https://x.cloudflareaccess.com", Applications: []Application{{Origin: "https://a", AUD: aud}}},
		{TeamDomain: team, Applications: []Application{{Origin: "http://a", AUD: aud}}},
		{TeamDomain: team, Applications: []Application{{Origin: "https://a", AUD: "bad aud!"}}},
		{TeamDomain: "evil.example.com", Applications: []Application{{Origin: "https://a", AUD: aud}}},
		{TeamDomain: team, Applications: []Application{{Origin: "https://a", PathPrefix: "/api", AUD: aud}}},
		{TeamDomain: team, Applications: []Application{{Origin: "https://a", PathPrefix: "/../", AUD: aud}}},
		{TeamDomain: team, Applications: []Application{{Origin: "https://a", AUD: aud}, {Origin: "https://A/", AUD: aud}}},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("accepted %+v", c)
		}
	}
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{"email@example.com": "em*il@example.com", "ab@x.org": "a*@x.org", "abcd@x.org": "a*d@x.org", "bad": ""} {
		if got := MaskEmail(in); got != want {
			t.Fatalf("MaskEmail(%q) = %q, want %q", in, got, want)
		}
	}
}
