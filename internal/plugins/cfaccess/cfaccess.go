// Package cfaccess lets a request already authenticated by Cloudflare Access exchange its Access
// assertion for an API session, and re-verifies that assertion on every later request.
package cfaccess

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/auth"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
)

const (
	ID              = "cloudflare-access"
	AssertionHeader = "Cf-Access-Jwt-Assertion"
	jwksTTL         = 5 * time.Minute
	jwksMinRefresh  = 30 * time.Second
)

// certsURL is a variable so tests can serve keys locally.
var certsURL = func(team string) string { return "https://" + team + "/cdn-cgi/access/certs" }

// Application is one Access application: an origin and path prefix, and the AUD tag its tokens carry.
type Application struct {
	Origin     string `yaml:"origin"`
	PathPrefix string `yaml:"path-prefix"`
	AUD        string `yaml:"aud"`
}

// Config names the Access team and the applications whose tokens are accepted.
type Config struct {
	TeamDomain   string        `yaml:"team-domain"`
	Applications []Application `yaml:"applications"`
}

var (
	teamPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.cloudflareaccess\.com$`)
	audPattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	pathPattern = regexp.MustCompile(`^/(?:[A-Za-z0-9_.~-]+/)*$`)
)

// validate applies the v1 rules: *.cloudflareaccess.com team, https origins, "/"-terminated prefixes.
func (c *Config) validate() error {
	c.TeamDomain = strings.ToLower(strings.TrimSpace(c.TeamDomain))
	if !teamPattern.MatchString(c.TeamDomain) {
		return errors.New("cloudflare-access: team-domain must be <team>.cloudflareaccess.com")
	}
	if len(c.Applications) == 0 || len(c.Applications) > 32 {
		return errors.New("cloudflare-access: 1-32 applications are required")
	}
	seen := map[string]bool{}
	for i := range c.Applications {
		a := &c.Applications[i]
		u, err := url.Parse(a.Origin)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Host, "*") {
			return fmt.Errorf("cloudflare-access: application origin %q must be an https origin", a.Origin)
		}
		a.Origin = "https://" + strings.ToLower(u.Host)
		if a.PathPrefix == "" {
			a.PathPrefix = "/"
		}
		if !pathPattern.MatchString(a.PathPrefix) || len(a.PathPrefix) > 256 || slices.Contains(strings.Split(a.PathPrefix, "/"), "..") ||
			slices.Contains(strings.Split(a.PathPrefix, "/"), ".") || !audPattern.MatchString(a.AUD) {
			return fmt.Errorf("cloudflare-access: application %q needs a /path/ prefix and its AUD tag", a.Origin)
		}
		if seen[a.Origin+a.PathPrefix] {
			return fmt.Errorf("cloudflare-access: duplicate application %s%s", a.Origin, a.PathPrefix)
		}
		seen[a.Origin+a.PathPrefix] = true
	}
	return nil
}

// Plugin is the cloudflare-access login plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	var cfg Config
	if err := h.Decode(&cfg); err != nil {
		return err
	}
	if err := cfg.validate(); err != nil {
		return err
	}
	v := &verifier{cfg: cfg, http: &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: time.Now}
	s := &service{v: v, identity: h.Identity, tokens: h.Tokens}
	if h.Tokens.Validators == nil {
		h.Tokens.Validators = map[authn.Source]func(*http.Request, *authn.Principal) error{}
	}
	h.Tokens.Validators[authn.SourceAccess] = s.revalidate
	s.register(h.Router)
	return nil
}

// Assertion is the verified subset of an Access application token.
type Assertion struct {
	Email   string
	Expires time.Time
}

type accessClaims struct {
	Email string `json:"email"`
	Type  string `json:"type"`
	jwt.RegisteredClaims
}

type verifier struct {
	cfg  Config
	http *http.Client
	now  func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

var errNoAssertion = apperr.New(http.StatusUnauthorized, "access_required", "request did not pass through Cloudflare Access")

// application picks the Access application protecting this request's origin and path.
func (v *verifier) application(r *http.Request) (Application, bool) {
	if r.TLS == nil || strings.ContainsAny(r.URL.EscapedPath(), "%\\") {
		return Application{}, false
	}
	origin := "https://" + strings.ToLower(r.Host)
	var best Application
	found := false
	for _, a := range v.cfg.Applications {
		if a.Origin == origin && strings.HasPrefix(r.URL.Path, a.PathPrefix) && len(a.PathPrefix) >= len(best.PathPrefix) {
			best, found = a, true
		}
	}
	return best, found
}

func (v *verifier) Verify(r *http.Request) (Assertion, error) {
	raw := r.Header.Get(AssertionHeader)
	app, ok := v.application(r)
	if raw == "" || !ok || len(raw) > 16<<10 {
		return Assertion{}, errNoAssertion
	}
	var c accessClaims
	parse := func() error {
		c = accessClaims{}
		_, err := jwt.ParseWithClaims(raw, &c, func(t *jwt.Token) (any, error) {
			kid, _ := t.Header["kid"].(string)
			return v.key(r.Context(), kid)
		}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer("https://"+v.cfg.TeamDomain), jwt.WithAudience(app.AUD),
			jwt.WithExpirationRequired(), jwt.WithTimeFunc(v.now))
		return err
	}
	err := parse()
	// A key may have been replaced under the same kid: refetch once (rate limited) and retry.
	if errors.Is(err, jwt.ErrTokenSignatureInvalid) && v.expire() {
		err = parse()
	}
	if err != nil || c.Type != "app" || c.Email == "" || c.ExpiresAt == nil {
		return Assertion{}, errNoAssertion
	}
	return Assertion{Email: strings.ToLower(strings.TrimSpace(c.Email)), Expires: c.ExpiresAt.Time}, nil
}

func (v *verifier) expire() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.now().Sub(v.fetched) < jwksMinRefresh {
		return false
	}
	v.fetched = time.Time{}
	return true
}

// key never holds the lock across the fetch (see authz.Refreshing.refresh for why on Workers);
// concurrent misses may each fetch the key set.
func (v *verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	age := v.now().Sub(v.fetched)
	cached := v.keys[kid]
	stale := v.keys == nil || age >= jwksMinRefresh
	v.mu.Unlock()
	if cached != nil && age < jwksTTL {
		return cached, nil
	}
	if stale {
		keys, err := v.fetch(ctx)
		if err != nil {
			if cached != nil {
				return cached, nil // keep serving the last good set through a brief outage
			}
			return nil, err
		}
		v.mu.Lock()
		v.keys, v.fetched = keys, v.now()
		v.mu.Unlock()
		if k := keys[kid]; k != nil {
			return k, nil
		}
		return nil, errors.New("unknown access signing key")
	}
	if cached != nil {
		return cached, nil
	}
	return nil, errors.New("unknown access signing key")
}

func (v *verifier) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, certsURL(v.cfg.TeamDomain), nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("access certs: status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			continue
		}
		out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	if len(out) == 0 {
		return nil, errors.New("access certs: no usable RSA keys")
	}
	return out, nil
}

type service struct {
	v        *verifier
	identity *identity.Service
	tokens   *authn.JWT
}

// revalidate binds an Access-derived session to a live assertion for the same person.
func (s *service) revalidate(r *http.Request, p *authn.Principal) error {
	a, err := s.v.Verify(r)
	if err != nil {
		return err
	}
	id, err := s.identity.FindByEmail(r.Context(), a.Email)
	if err != nil || id != p.UserID {
		return errors.New("access identity changed")
	}
	return nil
}

func (s *service) register(rt *httpx.Router) {
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/auth/access", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			a, err := s.v.Verify(r)
			out := map[string]any{"available": err == nil}
			if err == nil {
				// The caller's own Access identity, partly hidden for the "sign in as" button.
				out["email"] = MaskEmail(a.Email)
			}
			httpx.WriteJSON(w, http.StatusOK, out)
			return nil
		}})
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/auth/access/session", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if !auth.Secure(r) {
				return apperr.New(http.StatusForbidden, "insecure_transport", "sign-in requires HTTPS")
			}
			a, err := s.v.Verify(r)
			if err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), "access:"+a.Email)
			id, err := s.identity.FindByEmail(r.Context(), a.Email)
			if err != nil {
				return err
			}
			tok, _, exp, err := s.tokens.IssueUntil(id, authn.SourceAccess, a.Expires)
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"token": tok, "expiresAt": exp})
			return nil
		}})
}

// MaskEmail hides the middle of the local part: email@example.com -> em*il@example.com.
func MaskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	local := []rune(email[:at])
	switch {
	case len(local) <= 2:
		return string(local[:1]) + "*" + email[at:]
	case len(local) <= 4:
		return string(local[:1]) + "*" + string(local[len(local)-1:]) + email[at:]
	}
	return string(local[:2]) + "*" + string(local[len(local)-2:]) + email[at:]
}
