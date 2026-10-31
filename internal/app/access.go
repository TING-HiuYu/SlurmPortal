package app

import (
	"net/http"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
)

type grant struct {
	Object string `json:"object"`
	Action string `json:"action"`
}

// registerAccess adds GET /api/v2/me/access: the enabled plugins and every route policy the caller
// holds. Clients build navigation from it; the server still checks each request independently.
func registerAccess(rt *httpx.Router, plugins []plugin.Plugin) {
	ids := make([]string, 0, len(plugins))
	for _, p := range plugins {
		ids = append(ids, p.ID())
	}
	policies := rt.Policies()
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/me/access", Permission: authz.Authenticated(), Handler: func(w http.ResponseWriter, r *http.Request) error {
		p := authn.FromContext(r.Context())
		granted := []grant{}
		for _, pol := range policies {
			ok, err := rt.Authorizer().Allow(r.Context(), p, pol.Object, pol.Action)
			if err != nil {
				return err
			}
			if ok {
				granted = append(granted, grant{pol.Object, pol.Action})
			}
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"userId": p.UserID, "username": p.Username, "roles": p.Roles, "plugins": ids, "permissions": granted})
		return nil
	}})
}
