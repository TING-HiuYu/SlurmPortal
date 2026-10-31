package identity

import (
	"context"
	"errors"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// ListRoles returns every role ordered by rank.
func (s *Service) ListRoles(ctx context.Context) ([]Role, error) {
	rows, err := s.DB.Query(ctx, "SELECT id, name, keyword, COALESCE(remark,''), COALESCE(status,1), COALESCE(sort,999), COALESCE(creator,'') FROM roles WHERE deleted_at IS NULL ORDER BY sort, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Role{}
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Keyword, &r.Remark, &r.Status, &r.Sort, &r.Creator); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) getRole(ctx context.Context, id int64) (Role, error) {
	rs, err := s.rolesByIDs(ctx, []int64{id})
	if err != nil {
		return Role{}, apperr.ErrNotFound
	}
	return rs[0], nil
}

// RoleInput is the body for creating a role.
type RoleInput struct {
	Name    string `json:"name"`
	Keyword string `json:"keyword"`
	Remark  string `json:"remark"`
	Sort    int    `json:"sort"`
}

// Validate checks the role's name and keyword.
func (in RoleInput) Validate() error {
	if in.Name == "" {
		return errors.New("name is required")
	}
	if err := validText("name", in.Name, 20); err != nil {
		return err
	}
	if err := validText("remark", in.Remark, 100); err != nil {
		return err
	}
	if !keywordPattern.MatchString(in.Keyword) {
		return errors.New("keyword must be lowercase letters, digits, '_' or '-' (max 20)")
	}
	if in.Sort < 1 || in.Sort > 999 {
		return errors.New("sort must be within [1, 999]")
	}
	return nil
}

// CreateRole adds a role ranked below the caller's own.
func (s *Service) CreateRole(ctx context.Context, in RoleInput) (Role, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return Role{}, err
	}
	if !a.outranks(in.Sort) || in.Sort == superSort && !a.super() {
		return Role{}, errOutranked
	}
	now := s.now().UTC()
	_, err = s.DB.Exec(ctx, "INSERT INTO roles(created_at, updated_at, name, keyword, remark, status, sort, creator) VALUES (?, ?, ?, ?, ?, 1, ?, ?)",
		now, now, in.Name, in.Keyword, in.Remark, in.Sort, a.p.Username)
	if isUnique(err, "") {
		return Role{}, conflict("role name or keyword already exists")
	}
	if err != nil {
		return Role{}, err
	}
	var id int64
	if err := store.QueryRow(ctx, s.DB, "SELECT id FROM roles WHERE keyword = ?", []any{in.Keyword}, &id); err != nil {
		return Role{}, err
	}
	return s.getRole(ctx, id)
}

// RolePatch holds the role fields an update may change; nil fields are left alone.
type RolePatch struct {
	Name   *string `json:"name"`
	Remark *string `json:"remark"`
	Sort   *int    `json:"sort"`
	Status *int    `json:"status"`
}

func (p RolePatch) Validate() error {
	in := RoleInput{Name: "x", Keyword: "x", Sort: 1}
	if p.Name != nil {
		in.Name = *p.Name
	}
	if p.Remark != nil {
		in.Remark = *p.Remark
	}
	if p.Sort != nil {
		in.Sort = *p.Sort
	}
	if p.Status != nil && *p.Status != 1 && *p.Status != 2 {
		return errors.New("status must be 1 or 2")
	}
	return in.Validate()
}

// guardRole: only roles strictly below the actor; the super role is immutable through the API.
func (s *Service) guardRole(ctx context.Context, id int64) (actor, Role, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return a, Role{}, err
	}
	r, err := s.getRole(ctx, id)
	if err != nil {
		return a, r, err
	}
	if r.Sort == superSort || !a.outranks(r.Sort) {
		return a, r, errOutranked
	}
	return a, r, nil
}

// UpdateRole changes a role the caller outranks.
func (s *Service) UpdateRole(ctx context.Context, id int64, p RolePatch) (Role, error) {
	a, _, err := s.guardRole(ctx, id)
	if err != nil {
		return Role{}, err
	}
	if p.Sort != nil && (!a.outranks(*p.Sort) || *p.Sort == superSort) {
		return Role{}, errOutranked
	}
	sets, args := []string{"updated_at = ?"}, []any{s.now().UTC()}
	if p.Name != nil {
		sets, args = append(sets, "name = ?"), append(args, *p.Name)
	}
	if p.Remark != nil {
		sets, args = append(sets, "remark = ?"), append(args, *p.Remark)
	}
	if p.Sort != nil {
		sets, args = append(sets, "sort = ?"), append(args, *p.Sort)
	}
	if p.Status != nil {
		sets, args = append(sets, "status = ?"), append(args, *p.Status)
	}
	q := "UPDATE roles SET "
	for i, c := range sets {
		if i > 0 {
			q += ", "
		}
		q += c
	}
	if _, err := s.DB.Exec(ctx, q+" WHERE id = ?", append(args, id)...); err != nil {
		if isUnique(err, "") {
			return Role{}, conflict("role name already exists")
		}
		return Role{}, err
	}
	return s.getRole(ctx, id)
}

// DeleteRole removes a role the caller outranks.
func (s *Service) DeleteRole(ctx context.Context, id int64) error {
	_, r, err := s.guardRole(ctx, id)
	if err != nil {
		return err
	}
	var n int64
	if err := store.QueryRow(ctx, s.DB, "SELECT COUNT(*) FROM user_roles WHERE role_id = ?", []any{id}, &n); err != nil {
		return err
	}
	if n > 0 {
		return conflict("role is still assigned to users")
	}
	if _, err := s.DB.Batch(ctx, []store.Statement{
		{Query: "DELETE FROM v2_role_permissions WHERE role = ?", Args: []any{r.Keyword}},
		{Query: "DELETE FROM role_menus WHERE role_id = ?", Args: []any{id}},
		{Query: "DELETE FROM roles WHERE id = ?", Args: []any{id}},
	}); err != nil {
		return err
	}
	return s.reloadPolicies(ctx)
}

// Permission is one (object, action) pair a role grants.
type Permission struct {
	Object string `json:"object"`
	Action string `json:"action"`
}

// RolePermissions lists what a role grants.
func (s *Service) RolePermissions(ctx context.Context, id int64) ([]Permission, error) {
	r, err := s.getRole(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, "SELECT object, action FROM v2_role_permissions WHERE role = ? ORDER BY object, action", r.Keyword)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Permission{}
	for rows.Next() {
		var p Permission
		if err := rows.Scan(&p.Object, &p.Action); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PermissionSet is the body for replacing a role's permissions.
type PermissionSet struct {
	Permissions []Permission `json:"permissions"`
}

func (p PermissionSet) Validate() error {
	if len(p.Permissions) > 80 {
		return errors.New("at most 80 permissions per role")
	}
	for _, x := range p.Permissions {
		if err := (authz.Rule{Role: "x", Object: x.Object, Action: x.Action}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

// SetRolePermissions replaces a role's grants; non-super actors may only delegate what they hold.
func (s *Service) SetRolePermissions(ctx context.Context, id int64, set PermissionSet) error {
	a, r, err := s.guardRole(ctx, id)
	if err != nil {
		return err
	}
	rules := make([]authz.Rule, 0, len(set.Permissions))
	for _, p := range set.Permissions {
		if !a.super() {
			ok, err := s.Policies.Allow(ctx, a.p, p.Object, p.Action)
			if err != nil {
				return err
			}
			if !ok || p.Object == "*" || p.Action == authz.ActionAll {
				return errOutranked
			}
		}
		rules = append(rules, authz.Rule{Role: r.Keyword, Object: p.Object, Action: p.Action})
	}
	if err := authz.SetRoleRules(ctx, s.DB, r.Keyword, rules); err != nil {
		return err
	}
	return s.reloadPolicies(ctx)
}

func (s *Service) reloadPolicies(ctx context.Context) error {
	if s.Policies == nil {
		return nil
	}
	rules, err := authz.LoadRules(ctx, s.DB)
	if err != nil {
		return err
	}
	return s.Policies.Replace(rules)
}
