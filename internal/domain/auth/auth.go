// Package auth implements password login, token refresh/logout, self-service password change and
// password reset by e-mailed code.
package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

var (
	errBadCredentials = apperr.New(http.StatusUnauthorized, "bad_credentials", "invalid username or password")
	errInsecure       = apperr.New(http.StatusForbidden, "insecure_transport", "HTTPS is required")
	errLocked         = apperr.New(http.StatusTooManyRequests, "login_throttled", "too many failed attempts, try later")
)

// Revocations stores revoked session token IDs until the tokens would have expired anyway.
type Revocations struct {
	DB  store.Store
	Now func() time.Time
}

// IsRevoked reports whether tokenID was revoked.
func (r Revocations) IsRevoked(ctx context.Context, tokenID string) (bool, error) {
	err := store.QueryRow(ctx, r.DB, "SELECT 1 FROM v2_revoked_tokens WHERE token_id = ?", []any{tokenID}, new(int))
	if errors.Is(err, store.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Revoke records tokenID until expires and drops entries that have already expired.
func (r Revocations) Revoke(ctx context.Context, tokenID string, expires time.Time) error {
	_, err := r.DB.Batch(ctx, []store.Statement{
		{Query: "DELETE FROM v2_revoked_tokens WHERE expires_at < ?", Args: []any{r.Now().UTC().Format(time.RFC3339Nano)}},
		{Query: "INSERT OR IGNORE INTO v2_revoked_tokens(token_id, expires_at) VALUES (?, ?)", Args: []any{tokenID, expires.UTC().Format(time.RFC3339Nano)}},
	})
	return err
}

// Service owns sign-in, token refresh and logout, password changes and e-mailed reset codes.
type Service struct {
	DB          store.Store
	JWT         *authn.JWT
	Revocations Revocations
	Throttle    *Throttle
	Now         func() time.Time
	// Mailer and ResetKey enable password reset by e-mailed code; without them it is unavailable.
	Mailer   Mailer
	ResetKey []byte
}

// New returns the auth service; sign-in allows 5 failures per 15 minutes per key.
func New(db store.Store, jwt *authn.JWT, rev Revocations) *Service {
	return &Service{
		DB:          db,
		JWT:         jwt,
		Revocations: rev,
		Throttle:    NewThrottle(5, 15*time.Minute),
		Now:         time.Now,
	}
}

// Login verifies the password; unknown and disabled accounts cost the same as a wrong password.
func (s *Service) Login(ctx context.Context, username, pw string) (int64, error) {
	var (
		id   int64
		hash string
	)
	err := store.QueryRow(ctx, s.DB, `SELECT u.id, c.app_hash FROM users u
		JOIN v2_user_credentials c ON c.user_id = u.id
		WHERE u.username = ? AND u.status = 1 AND u.deleted_at IS NULL`, []any{username}, &id, &hash)
	if errors.Is(err, store.ErrNoRows) {
		password.VerifyDummy(pw)
		return 0, errBadCredentials
	}
	if err != nil {
		return 0, err
	}
	ok, err := password.Verify(hash, pw)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, errBadCredentials
	}
	return id, nil
}

// SetPassword stores a user's portal (argon2id) and LDAP (crypt) hashes; the slaves then write the
// LDAP hash to userPassword. An empty LDAP hash leaves the directory's password untouched.
func (s *Service) SetPassword(ctx context.Context, userID int64, c password.Credential) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO v2_user_credentials(user_id, app_hash, ldap_hash, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET app_hash = excluded.app_hash, ldap_hash = excluded.ldap_hash, updated_at = excluded.updated_at`,
		userID, c.App, c.LDAP, s.Now().UTC())
	return err
}

// Secure accepts TLS, or plain HTTP only from a loopback peer (same-host reverse proxy).
func Secure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (l loginRequest) Validate() error {
	if l.Username == "" || len(l.Username) > 64 || l.Password == "" || len(l.Password) > 4*password.MaxLength {
		return errors.New("username and password are required")
	}
	return nil
}

// changeRequest proves the user either with the current password or with a code e-mailed by
// POST /api/v2/me/password/code; exactly one of them is given.
type changeRequest struct {
	Current string `json:"currentPassword"`
	Code    string `json:"code"`
	New     string `json:"newPassword"`
}

func (c changeRequest) Validate() error {
	switch {
	case (c.Current == "") == (c.Code == ""):
		return errors.New("give either the current password or an e-mailed code")
	case len(c.Current) > 4*password.MaxLength:
		return errors.New("current password is too long")
	case c.Code != "" && (len(strings.TrimSpace(c.Code)) != 6 || strings.Trim(strings.TrimSpace(c.Code), "0123456789") != ""):
		return errors.New("the code has 6 digits")
	}
	return password.CheckPolicy(c.New)
}

type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Service) issue(w http.ResponseWriter, userID int64, src authn.Source) error {
	tok, _, exp, err := s.JWT.Issue(userID, src)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, tokenResponse{Token: tok, ExpiresAt: exp})
	return nil
}

// Register adds the /api/v2/auth/* and password routes.
func (s *Service) Register(rt *httpx.Router, ip httpx.ClientIP) {
	s.registerReset(rt, ip)
	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/auth/login", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if !Secure(r) {
				return errInsecure
			}
			var req loginRequest
			if err := httpx.DecodeJSON(w, r, &req); err != nil {
				return err
			}
			httpx.SetTarget(r.Context(), "user:"+req.Username)
			key := ip(r) + "|" + strings.ToLower(req.Username)
			if !s.Throttle.Allow(key) {
				return errLocked
			}
			id, err := s.Login(r.Context(), req.Username, req.Password)
			if err != nil {
				if errors.Is(err, errBadCredentials) {
					s.Throttle.Fail(key)
				}
				return err
			}
			s.Throttle.Reset(key)
			return s.issue(w, id, authn.SourcePassword)
		}})

	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/auth/refresh", Permission: authz.Authenticated(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			p := authn.FromContext(r.Context())
			if err := s.Revocations.Revoke(r.Context(), p.TokenID, s.Now().Add(s.JWT.TTL)); err != nil {
				return err
			}
			return s.issue(w, p.UserID, p.Source)
		}})

	rt.Handle(httpx.Route{Method: http.MethodPost, Path: "/api/v2/auth/logout", Permission: authz.Authenticated(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			p := authn.FromContext(r.Context())
			if err := s.Revocations.Revoke(r.Context(), p.TokenID, s.Now().Add(s.JWT.TTL)); err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusNoContent, nil)
			return nil
		}})

	rt.Handle(httpx.Route{Method: http.MethodPut, Path: "/api/v2/me/password", Permission: authz.Authenticated(),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if !Secure(r) {
				return errInsecure
			}
			var req changeRequest
			if err := httpx.DecodeJSON(w, r, &req); err != nil {
				return err
			}
			p := authn.FromContext(r.Context())
			httpx.SetTarget(r.Context(), "user:"+p.Username)
			if req.Code != "" {
				if err := s.confirmCode(r.Context(), p.UserID, req.Code, req.New); err != nil {
					return err
				}
				httpx.WriteJSON(w, http.StatusNoContent, nil)
				return nil
			}
			key := "self|" + p.Username
			if !s.Throttle.Allow(key) {
				return errLocked
			}
			if _, err := s.Login(r.Context(), p.Username, req.Current); err != nil {
				if errors.Is(err, errBadCredentials) {
					s.Throttle.Fail(key)
				}
				return err
			}
			c, err := password.Hash(req.New)
			if err != nil {
				return apperr.BadRequest(err.Error())
			}
			if err := s.SetPassword(r.Context(), p.UserID, c); err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusNoContent, nil)
			return nil
		}})
}

// Throttle counts failures per key in a fixed window; in-memory per instance by design.
type Throttle struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	hits   map[string]*attempts
}

type attempts struct {
	n     int
	start time.Time
}

// NewThrottle allows max failures per key within window.
func NewThrottle(max int, window time.Duration) *Throttle {
	return &Throttle{max: max, window: window, now: time.Now, hits: map[string]*attempts{}}
}

// Allow reports whether key may try again (fewer than max failures in the current window).
func (t *Throttle) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.hits[key]
	if !ok {
		return true
	}
	if t.now().Sub(a.start) > t.window {
		delete(t.hits, key)
		return true
	}
	return a.n < t.max
}

// Fail counts a failure for key, starting a new window when the old one has passed.
func (t *Throttle) Fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if len(t.hits) > 100000 {
		for k, a := range t.hits {
			if now.Sub(a.start) > t.window {
				delete(t.hits, k)
			}
		}
	}
	a, ok := t.hits[key]
	if !ok || now.Sub(a.start) > t.window {
		t.hits[key] = &attempts{n: 1, start: now}
		return
	}
	a.n++
}

// Reset forgets key's failures, after a successful sign-in.
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.hits, key)
}
