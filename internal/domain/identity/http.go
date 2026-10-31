package identity

import (
	"net/http"
	"strconv"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
)

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.ErrNotFound
	}
	httpx.SetTarget(r.Context(), r.Pattern[len(r.Method)+1:]+"#"+r.PathValue("id"))
	return id, nil
}

func queryInt(r *http.Request, key string, def, lo, hi int) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return 0, apperr.BadRequest(key + " is out of range")
	}
	return n, nil
}

func ok(w http.ResponseWriter, status int, v any) error {
	httpx.WriteJSON(w, status, v)
	return nil
}

func decodeAnd[T any](w http.ResponseWriter, r *http.Request, f func(T) (any, error), status int) error {
	var in T
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		return err
	}
	out, err := f(in)
	if err != nil {
		return err
	}
	return ok(w, status, out)
}

// Register adds the users, groups and roles routes.
func (s *Service) Register(rt *httpx.Router) {
	type H = httpx.HandlerFunc
	route := func(method, path string, perm authz.Permission, h H) {
		rt.Handle(httpx.Route{Method: method, Path: path, Permission: perm, Handler: h})
	}
	pol := authz.Policy

	route(http.MethodGet, "/api/v2/me", authz.Authenticated(), func(w http.ResponseWriter, r *http.Request) error {
		p := authn.FromContext(r.Context())
		u, err := s.GetUser(r.Context(), p.UserID)
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, map[string]any{"user": u, "roles": p.Roles})
	})
	route(http.MethodPut, "/api/v2/me/shell", authz.Authenticated(), func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			LoginShell string `json:"loginShell"`
		}
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		if err := s.SetMyShell(r.Context(), in.LoginShell); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})

	route(http.MethodGet, "/api/v2/users", pol("user", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		var f UserFilter
		var err error
		if f.Page, err = queryInt(r, "page", 1, 1, 1<<20); err != nil {
			return err
		}
		if f.Size, err = queryInt(r, "size", 20, 1, 100); err != nil {
			return err
		}
		if f.Status, err = queryInt(r, "status", 0, 0, 2); err != nil {
			return err
		}
		gid, err := queryInt(r, "groupId", 0, 0, 1<<31-1)
		if err != nil {
			return err
		}
		f.GroupID, f.Query = int64(gid), r.URL.Query().Get("q")
		if len(f.Query) > 100 {
			return apperr.BadRequest("q is too long")
		}
		page, err := s.ListUsers(r.Context(), f)
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, page)
	})
	route(http.MethodGet, "/api/v2/users/{id}", pol("user", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		u, err := s.GetUser(r.Context(), id)
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, u)
	})
	route(http.MethodPost, "/api/v2/users", pol("user", authz.ActionCreate), func(w http.ResponseWriter, r *http.Request) error {
		return decodeAnd(w, r, func(in UserInput) (any, error) {
			httpx.SetTarget(r.Context(), "user:"+in.Username)
			return s.CreateUser(r.Context(), in)
		}, http.StatusCreated)
	})
	route(http.MethodPatch, "/api/v2/users/{id}", pol("user", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return decodeAnd(w, r, func(p UserPatch) (any, error) { return s.UpdateUser(r.Context(), id, p) }, http.StatusOK)
	})
	route(http.MethodPut, "/api/v2/users/{id}/status", pol("user", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var in struct {
			Status int `json:"status"`
		}
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		if err := s.SetStatus(r.Context(), id, in.Status); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})
	route(http.MethodPost, "/api/v2/users/{id}/password-reset", pol("user", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		pw, err := s.ResetPassword(r.Context(), id)
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, map[string]string{"password": pw})
	})
	route(http.MethodDelete, "/api/v2/users/{id}", pol("user", authz.ActionDelete), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		if err := s.DeleteUser(r.Context(), id); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})

	route(http.MethodGet, "/api/v2/groups", pol("group", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		gs, err := s.ListGroups(r.Context())
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, gs)
	})
	route(http.MethodGet, "/api/v2/groups/{id}", pol("group", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		g, err := s.GetGroup(r.Context(), id)
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, g)
	})
	route(http.MethodPost, "/api/v2/groups", pol("group", authz.ActionCreate), func(w http.ResponseWriter, r *http.Request) error {
		return decodeAnd(w, r, func(in GroupInput) (any, error) {
			httpx.SetTarget(r.Context(), "group:"+in.Name)
			return s.CreateGroup(r.Context(), in)
		}, http.StatusCreated)
	})
	route(http.MethodPatch, "/api/v2/groups/{id}", pol("group", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return decodeAnd(w, r, func(p GroupPatch) (any, error) { return s.UpdateGroup(r.Context(), id, p) }, http.StatusOK)
	})
	route(http.MethodDelete, "/api/v2/groups/{id}", pol("group", authz.ActionDelete), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		if err := s.DeleteGroup(r.Context(), id); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})
	route(http.MethodPost, "/api/v2/groups/{id}/members", pol("group", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var in MembershipChange
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		if err := s.ChangeMembers(r.Context(), id, in); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})

	route(http.MethodGet, "/api/v2/roles", pol("role", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		rs, err := s.ListRoles(r.Context())
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, rs)
	})
	route(http.MethodPost, "/api/v2/roles", pol("role", authz.ActionCreate), func(w http.ResponseWriter, r *http.Request) error {
		return decodeAnd(w, r, func(in RoleInput) (any, error) {
			httpx.SetTarget(r.Context(), "role:"+in.Keyword)
			return s.CreateRole(r.Context(), in)
		}, http.StatusCreated)
	})
	route(http.MethodPatch, "/api/v2/roles/{id}", pol("role", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return decodeAnd(w, r, func(p RolePatch) (any, error) { return s.UpdateRole(r.Context(), id, p) }, http.StatusOK)
	})
	route(http.MethodDelete, "/api/v2/roles/{id}", pol("role", authz.ActionDelete), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		if err := s.DeleteRole(r.Context(), id); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})
	route(http.MethodGet, "/api/v2/roles/{id}/permissions", pol("role", authz.ActionRead), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		ps, err := s.RolePermissions(r.Context(), id)
		if err != nil {
			return err
		}
		return ok(w, http.StatusOK, ps)
	})
	route(http.MethodPut, "/api/v2/roles/{id}/permissions", pol("role", authz.ActionUpdate), func(w http.ResponseWriter, r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var in PermissionSet
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			return err
		}
		if err := s.SetRolePermissions(r.Context(), id, in); err != nil {
			return err
		}
		return ok(w, http.StatusNoContent, nil)
	})
}
