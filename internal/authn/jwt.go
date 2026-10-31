package authn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrInvalidToken = errors.New("invalid token")
	ErrRevoked      = errors.New("token revoked")
	ErrInactive     = errors.New("principal inactive")
)

// Directory loads live account state so disabled users and role changes take effect immediately.
type Directory interface {
	LoadPrincipal(ctx context.Context, userID int64) (*Principal, error)
}

// Revocations reports whether a token ID was revoked (on logout, or when a refresh replaced it).
type Revocations interface {
	IsRevoked(ctx context.Context, tokenID string) (bool, error)
}

// Keyring holds the HMAC keys for session tokens by key ID. New tokens are signed with ActiveKID;
// the other keys still verify tokens issued before a rotation.
type Keyring struct {
	ActiveKID string
	Keys      map[string][]byte
}

func (k Keyring) validate() error {
	if len(k.Keys) == 0 || k.ActiveKID == "" {
		return errors.New("jwt keyring is empty")
	}
	if _, ok := k.Keys[k.ActiveKID]; !ok {
		return errors.New("jwt active key missing")
	}
	for kid, key := range k.Keys {
		if len(key) < 32 {
			return fmt.Errorf("jwt key %q shorter than 32 bytes", kid)
		}
	}
	return nil
}

// JWT issues and verifies the portal's session tokens (HS256) and resolves them to a live Principal.
type JWT struct {
	Issuer      string
	Audience    string
	TTL         time.Duration
	Keys        Keyring
	Directory   Directory
	Revocations Revocations
	Now         func() time.Time
	// Validators re-check, on every request, tokens whose source demands it (e.g. a live Access assertion).
	Validators map[Source]func(*http.Request, *Principal) error
}

type claims struct {
	Source Source `json:"src"`
	jwt.RegisteredClaims
}

// NewJWT validates the keyring and returns a token service.
func NewJWT(issuer, audience string, ttl time.Duration, keys Keyring, dir Directory, rev Revocations) (*JWT, error) {
	if err := keys.validate(); err != nil {
		return nil, err
	}
	if issuer == "" || audience == "" || ttl <= 0 || dir == nil {
		return nil, errors.New("jwt issuer, audience, ttl and directory are required")
	}
	return &JWT{Issuer: issuer, Audience: audience, TTL: ttl, Keys: keys, Directory: dir, Revocations: rev, Now: time.Now}, nil
}

// Issue signs a token for userID with the configured TTL.
func (j *JWT) Issue(userID int64, src Source) (token string, tokenID string, expires time.Time, err error) {
	return j.IssueUntil(userID, src, time.Time{})
}

// IssueUntil issues a token that never outlives notAfter (zero means the default TTL).
func (j *JWT) IssueUntil(userID int64, src Source, notAfter time.Time) (token string, tokenID string, expires time.Time, err error) {
	var raw [16]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", "", time.Time{}, err
	}
	now := j.Now()
	tokenID = hex.EncodeToString(raw[:])
	expires = now.Add(j.TTL)
	if !notAfter.IsZero() && notAfter.Before(expires) {
		expires = notAfter
	}
	c := claims{
		Source: src,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    j.Issuer,
			Subject:   strconv.FormatInt(userID, 10),
			Audience:  jwt.ClaimStrings{j.Audience},
			ExpiresAt: jwt.NewNumericDate(expires),
			NotBefore: jwt.NewNumericDate(now),
			IssuedAt:  jwt.NewNumericDate(now),
			ID:        tokenID,
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	t.Header["kid"] = j.Keys.ActiveKID
	token, err = t.SignedString(j.Keys.Keys[j.Keys.ActiveKID])
	return token, tokenID, expires, err
}

// Authenticate verifies a Bearer token and loads the user's current roles. It returns (nil, nil)
// when the request has no Authorization header.
func (j *JWT) Authenticate(r *http.Request) (*Principal, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, nil
	}
	raw, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || raw == "" {
		return nil, ErrInvalidToken
	}
	c := new(claims)
	_, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		key, ok := j.Keys.Keys[kid]
		if !ok {
			return nil, ErrInvalidToken
		}
		return key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(j.Issuer),
		jwt.WithAudience(j.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(5*time.Second),
		jwt.WithTimeFunc(j.Now),
	)
	if err != nil || c.ID == "" {
		return nil, ErrInvalidToken
	}
	userID, err := strconv.ParseInt(c.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return nil, ErrInvalidToken
	}
	if j.Revocations != nil {
		revoked, err := j.Revocations.IsRevoked(r.Context(), c.ID)
		if err != nil {
			return nil, err
		}
		if revoked {
			return nil, ErrRevoked
		}
	}
	p, err := j.Directory.LoadPrincipal(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrInactive
	}
	out := *p
	out.Source = c.Source
	out.TokenID = c.ID
	if v := j.Validators[c.Source]; v != nil {
		if err := v(r, &out); err != nil {
			return nil, ErrInvalidToken
		}
	}
	return &out, nil
}
