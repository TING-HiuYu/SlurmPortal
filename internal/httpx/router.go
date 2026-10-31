package httpx

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
)

// HandlerFunc is a route handler; a returned error is written with WriteError.
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error

// Middleware wraps the whole router.
type Middleware func(http.Handler) http.Handler

// Route is one endpoint. Every route must declare a Permission.
type Route struct {
	Method     string
	Path       string
	Permission authz.Permission
	Handler    HandlerFunc
	// Routine marks a periodic machine report (status snapshots, heartbeats): it is audited only
	// when it fails, so the bounded audit log keeps people's actions instead of heartbeats.
	Routine bool
}

// Router registers routes with their permissions and checks them before calling a handler.
type Router struct {
	mux        *http.ServeMux
	authn      authn.Authenticator
	peers      authn.Authenticator
	authz      authz.Authorizer
	middleware []Middleware
	routes     []Route
}

// NewRouter returns a router that authenticates users with an and authorizes them with az.
func NewRouter(an authn.Authenticator, az authz.Authorizer) *Router {
	return &Router{mux: http.NewServeMux(), authn: an, authz: az}
}

// SetPeerAuthenticator installs the machine (mTLS) identity check used by authz.Peer routes.
func (rt *Router) SetPeerAuthenticator(a authn.Authenticator) { rt.peers = a }

// Use appends global middleware; the first registered runs outermost.
func (rt *Router) Use(m ...Middleware) { rt.middleware = append(rt.middleware, m...) }

// Handle panics on undeclared permissions so a route can never ship without an access decision.
func (rt *Router) Handle(route Route) {
	if err := route.Permission.Validate(); err != nil {
		panic(fmt.Sprintf("route %s %s: %v", route.Method, route.Path, err))
	}
	if route.Method == "" || route.Path == "" || route.Handler == nil {
		panic(fmt.Sprintf("route %s %s: method, path and handler are required", route.Method, route.Path))
	}
	if route.Permission.Kind != authz.KindPublic && route.Permission.Kind != authz.KindPeer && rt.authn == nil {
		panic(fmt.Sprintf("route %s %s: protected route without authenticator", route.Method, route.Path))
	}
	if route.Permission.Kind == authz.KindPeer && rt.peers == nil {
		panic(fmt.Sprintf("route %s %s: peer route without peer authenticator", route.Method, route.Path))
	}
	if route.Permission.Kind == authz.KindPolicy && rt.authz == nil {
		panic(fmt.Sprintf("route %s %s: policy route without authorizer", route.Method, route.Path))
	}
	rt.mux.Handle(route.Method+" "+route.Path, rt.guard(route))
	rt.routes = append(rt.routes, route)
}

// Routes lists every registered route.
func (rt *Router) Routes() []Route { return slices.Clone(rt.routes) }

// Handler returns the router wrapped in its middleware, outermost first.
func (rt *Router) Handler() http.Handler {
	var h http.Handler = rt.mux
	for i := len(rt.middleware) - 1; i >= 0; i-- {
		h = rt.middleware[i](h)
	}
	return h
}

func (rt *Router) guard(route Route) http.Handler {
	perm := route.Permission
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := ScopeFrom(r.Context()); s != nil {
			s.Route = route.Method + " " + route.Path
			s.Routine = route.Routine
		}
		if perm.Kind != authz.KindPublic {
			an := rt.authn
			if perm.Kind == authz.KindPeer {
				an = rt.peers
			}
			p, err := an.Authenticate(r)
			if err != nil {
				if errors.Is(err, authn.ErrInvalidToken) || errors.Is(err, authn.ErrRevoked) || errors.Is(err, authn.ErrInactive) {
					WriteError(w, r, apperr.ErrUnauthenticated)
				} else {
					WriteError(w, r, apperr.ErrUnavailable.Wrap(err))
				}
				return
			}
			if p == nil {
				WriteError(w, r, apperr.ErrUnauthenticated)
				return
			}
			setPrincipal(r.Context(), p)
			if perm.Kind == authz.KindPolicy {
				ok, err := rt.authz.Allow(r.Context(), p, perm.Object, perm.Action)
				if err != nil {
					WriteError(w, r, apperr.ErrUnavailable.Wrap(err))
					return
				}
				if !ok {
					WriteError(w, r, apperr.ErrForbidden)
					return
				}
			}
			r = r.WithContext(authn.WithPrincipal(r.Context(), p))
		}
		if err := route.Handler(w, r); err != nil {
			WriteError(w, r, err)
		}
	})
}

// Policies lists the distinct (object, action) pairs that guard registered routes, sorted.
func (rt *Router) Policies() []authz.Permission {
	seen := map[[2]string]bool{}
	var out []authz.Permission
	for _, r := range rt.routes {
		k := [2]string{r.Permission.Object, r.Permission.Action}
		if r.Permission.Kind == authz.KindPolicy && !seen[k] {
			seen[k] = true
			out = append(out, r.Permission)
		}
	}
	slices.SortFunc(out, func(a, b authz.Permission) int {
		return cmp.Or(cmp.Compare(a.Object, b.Object), cmp.Compare(a.Action, b.Action))
	})
	return out
}

// Authorizer is the policy decision point routes are guarded with.
func (rt *Router) Authorizer() authz.Authorizer { return rt.authz }
