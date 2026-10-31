// Package authn resolves the authenticated principal for a request.
package authn

import (
	"context"
	"net/http"
	"slices"
)

// Source records how a principal signed in.
type Source string

const (
	SourcePassword Source = "password"
	SourceOAuth    Source = "oauth"
	SourceAccess   Source = "cloudflare-access"
	SourcePeer     Source = "peer-mtls"
)

// Principal is always derived server-side; handlers must never take identity from request input.
type Principal struct {
	UserID   int64
	Username string
	Roles    []string
	Source   Source
	TokenID  string
}

// HasRole reports whether the principal holds the role keyword. It is safe on nil.
func (p *Principal) HasRole(keyword string) bool {
	return p != nil && slices.Contains(p.Roles, keyword)
}

// Authenticator turns request credentials into a Principal.
type Authenticator interface {
	// Authenticate returns (nil, nil) when the request carries no credentials.
	Authenticate(r *http.Request) (*Principal, error)
}

type ctxKey struct{}

// WithPrincipal stores p in the request context.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the request's principal, or nil for an anonymous request.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}
