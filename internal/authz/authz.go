// Package authz declares route permissions and evaluates them.
package authz

import (
	"context"
	"errors"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

// Kind says which check a route's Permission requires.
type Kind uint8

const (
	kindUnset Kind = iota
	KindPublic
	KindAuthenticated
	KindPolicy
	// KindPeer admits only a verified machine identity (mTLS), never a user token.
	KindPeer
)

// Permission must be declared on every route; the zero value is rejected at registration.
type Permission struct {
	Kind   Kind
	Object string
	Action string
}

// Public lets anyone call the route.
func Public() Permission { return Permission{Kind: KindPublic} }

// Authenticated requires a signed-in user.
func Authenticated() Permission { return Permission{Kind: KindAuthenticated} }

// Peer requires a machine: a slave or gateway with a verified client certificate.
func Peer() Permission { return Permission{Kind: KindPeer} }

// Policy requires the principal's roles to be granted (object, action) by the Authorizer.
func Policy(object, action string) Permission {
	return Permission{Kind: KindPolicy, Object: object, Action: action}
}

// Validate rejects an unset or incomplete permission, so a route cannot be registered without one.
func (p Permission) Validate() error {
	switch p.Kind {
	case KindPublic, KindAuthenticated, KindPeer:
		return nil
	case KindPolicy:
		if p.Object == "" || p.Action == "" {
			return errors.New("policy permission needs object and action")
		}
		return nil
	default:
		return errors.New("route permission not declared")
	}
}

// Authorizer decides whether a principal's roles grant (object, action).
type Authorizer interface {
	Allow(ctx context.Context, p *authn.Principal, object, action string) (bool, error)
}
