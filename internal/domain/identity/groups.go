package identity

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const groupColumns = `g.id, COALESCE(g.group_name,''), COALESCE(g.remark,''), COALESCE(g.group_type,''), COALESCE(g.group_class,''),
	COALESCE(g.gid_number,0), COALESCE(g.parent_id,0), g.group_dn, COALESCE(g.source,''), COALESCE(g.creator,''),
	COALESCE(CAST(g.created_at AS TEXT),''), (SELECT COUNT(*) FROM group_users gu WHERE gu.group_id = g.id),
	COALESCE(g.home_root,''), COALESCE(g.login_shell,'')`

func scanGroup(rows store.Rows) (Group, error) {
	var g Group
	err := rows.Scan(&g.ID, &g.Name, &g.Remark, &g.Type, &g.Class, &g.GIDNumber, &g.ParentID, &g.DN, &g.Source, &g.Creator, &g.CreatedAt, &g.MemberSize, &g.HomeRoot, &g.LoginShell)
	return g, err
}

func (s *Service) queryGroups(ctx context.Context, where string, args ...any) ([]Group, error) {
	rows, err := s.DB.Query(ctx, "SELECT "+groupColumns+" FROM groups g WHERE g.deleted_at IS NULL AND ("+where+") ORDER BY g.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) groupBy(ctx context.Context, where string, args ...any) (Group, error) {
	gs, err := s.queryGroups(ctx, where, args...)
	if err != nil {
		return Group{}, err
	}
	if len(gs) == 0 {
		return Group{}, apperr.ErrNotFound
	}
	return gs[0], nil
}

// GroupsByIDs returns the named groups; an unknown id is an error.
func (s *Service) GroupsByIDs(ctx context.Context, ids []int64) ([]Group, error) {
	return s.groupsByIDs(ctx, ids)
}

func (s *Service) groupsByIDs(ctx context.Context, ids []int64) ([]Group, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	gs, err := s.queryGroups(ctx, "g.id IN ("+placeholders(len(ids))+")", int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	if len(gs) != len(ids) {
		return nil, apperr.BadRequest("unknown group id")
	}
	return gs, nil
}

// ListGroups returns every group.
func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	return s.queryGroups(ctx, "1 = 1")
}

// GetGroup returns one group.
func (s *Service) GetGroup(ctx context.Context, id int64) (Group, error) {
	return s.groupBy(ctx, "g.id = ?", id)
}

// GroupInput is the body for creating a group.
type GroupInput struct {
	Name      string `json:"groupName"`
	Type      string `json:"groupType"`
	Class     string `json:"groupClass"`
	GIDNumber int64  `json:"gidNumber"`
	ParentID  int64  `json:"parentId"`
	Remark    string `json:"remark"`
	// HomeRoot and LoginShell are presets for accounts whose main group this is (POSIX groups only).
	HomeRoot   string `json:"homeRoot"`
	LoginShell string `json:"loginShell"`
}

// validPresets checks the account presets a POSIX group carries.
func validPresets(class, home, shell string) error {
	if class != ClassPOSIX && (home != "" || shell != "") {
		return errors.New("only a posixGroup carries home and shell presets")
	}
	if home != "" {
		if err := validHome(home); err != nil {
			return errors.New("homeRoot must be an absolute path")
		}
	}
	if shell != "" {
		return ValidShell(shell)
	}
	return nil
}

// Validate checks names, type and the POSIX options of a new group.
func (in GroupInput) Validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return errors.New("groupName is required")
	}
	if err := validText("groupName", in.Name, 128); err != nil {
		return err
	}
	if err := validText("remark", in.Remark, 128); err != nil {
		return err
	}
	if in.Type != TypeCN && in.Type != TypeOU {
		return errors.New("groupType must be cn or ou")
	}
	if in.Class != "" && in.Class != ClassPOSIX && in.Class != ClassUnique {
		return errors.New("groupClass must be posixGroup or groupOfUniqueNames")
	}
	if in.Class == ClassPOSIX && in.Type != TypeCN {
		return errors.New("posixGroup requires groupType cn")
	}
	if in.ParentID < 0 {
		return errors.New("parentId is invalid")
	}
	if err := validPresets(in.Class, in.HomeRoot, in.LoginShell); err != nil {
		return err
	}
	return validPOSIX(in.GIDNumber, "gidNumber")
}

// CreateGroup adds a group under its parent, allocating a GID for a POSIX group.
func (s *Service) CreateGroup(ctx context.Context, in GroupInput) (Group, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return Group{}, err
	}
	if in.Class == ClassPOSIX && !a.super() {
		return Group{}, errSuperOnly
	}
	parentDN := s.Layout.BaseDN
	if in.ParentID != 0 {
		parent, err := s.GetGroup(ctx, in.ParentID)
		if errors.Is(err, apperr.ErrNotFound) {
			return Group{}, apperr.BadRequest("parent group not found")
		}
		if err != nil {
			return Group{}, err
		}
		if parent.IsPOSIX() {
			return Group{}, apperr.BadRequest("a POSIX group cannot contain child groups")
		}
		parentDN = parent.DN
	}
	if parentDN == "" {
		return Group{}, apperr.ErrUnavailable.Wrap(errors.New("directory base DN is not configured"))
	}
	dn := in.Type + "=" + EscapeDNValue(in.Name) + "," + parentDN
	if _, err := s.groupBy(ctx, "lower(g.group_dn) = lower(?)", dn); err == nil {
		return Group{}, conflict("a group with this DN already exists")
	}
	auto := in.GIDNumber == 0
	if auto {
		if in.GIDNumber, err = nextID(ctx, s.DB, "gid"); err != nil {
			return Group{}, err
		}
	}
	now := s.now().UTC()
	for attempt := 0; ; attempt++ {
		stmts := []store.Statement{{
			Query: `INSERT INTO groups(created_at, updated_at, group_name, remark, creator, group_type, group_class, gid_number,
				parent_id, source_dept_id, source, source_dept_parent_id, source_user_num, group_dn, sync_state, home_root, login_shell)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'platform_0', 'platform', ?, 0, ?, 1, ?, ?)`,
			Args: []any{now, now, in.Name, in.Remark, a.p.Username, in.Type, in.Class, in.GIDNumber, in.ParentID,
				parentSourceID(in.ParentID), dn, strings.TrimRight(in.HomeRoot, "/"), in.LoginShell},
		}}
		if in.Class != ClassPOSIX {
			// groupOfUniqueNames needs at least one member; v1 used the bootstrap admin.
			stmts = append(stmts, store.Statement{
				Query: "INSERT OR IGNORE INTO group_users(group_id, user_id) SELECT (SELECT id FROM groups WHERE group_dn = ?), id FROM users WHERE id = 1",
				Args:  []any{dn},
			})
		}
		_, err = s.DB.Batch(ctx, stmts)
		if err == nil || !auto || !isUnique(err, "gid_number") || attempt == 4 {
			break
		}
		if in.GIDNumber, err = nextID(ctx, s.DB, "gid"); err != nil {
			return Group{}, err
		}
	}
	if isUnique(err, "gid_number") {
		return Group{}, conflict("gidNumber already in use")
	}
	if err != nil {
		return Group{}, err
	}
	return s.groupBy(ctx, "g.group_dn = ?", dn)
}

func parentSourceID(parent int64) string {
	if parent == 0 {
		return "platform_0"
	}
	return "platform_" + strconv.FormatInt(parent, 10)
}

// GroupPatch holds the group fields an update may change; nil fields are left alone.
type GroupPatch struct {
	Remark     *string `json:"remark"`
	HomeRoot   *string `json:"homeRoot"`
	LoginShell *string `json:"loginShell"`
}

func (p GroupPatch) Validate() error {
	if p.Remark == nil && p.HomeRoot == nil && p.LoginShell == nil {
		return errors.New("nothing to update")
	}
	if p.Remark != nil {
		return validText("remark", *p.Remark, 128)
	}
	return nil
}

// UpdateGroup changes the remark and, for POSIX groups (super admins only), the account presets.
// Presets apply to accounts created afterwards; existing homes and shells are left alone.
func (s *Service) UpdateGroup(ctx context.Context, id int64, p GroupPatch) (Group, error) {
	g, err := s.GetGroup(ctx, id)
	if err != nil {
		return Group{}, err
	}
	sets, args := []string{"updated_at = ?"}, []any{s.now().UTC()}
	if p.Remark != nil {
		sets, args = append(sets, "remark = ?"), append(args, *p.Remark)
	}
	if p.HomeRoot != nil || p.LoginShell != nil {
		a, err := s.actor(ctx)
		if err != nil {
			return Group{}, err
		}
		if !a.super() {
			return Group{}, errSuperOnly
		}
		home, shell := g.HomeRoot, g.LoginShell
		if p.HomeRoot != nil {
			home = strings.TrimRight(strings.TrimSpace(*p.HomeRoot), "/")
		}
		if p.LoginShell != nil {
			shell = strings.TrimSpace(*p.LoginShell)
		}
		if err := validPresets(g.Class, home, shell); err != nil {
			return Group{}, apperr.BadRequest(err.Error())
		}
		sets, args = append(sets, "home_root = ?", "login_shell = ?"), append(args, home, shell)
	}
	if _, err := s.DB.Exec(ctx, "UPDATE groups SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(args, id)...); err != nil {
		return Group{}, err
	}
	return s.GetGroup(ctx, id)
}

// DeleteGroup removes a group the caller may manage.
func (s *Service) DeleteGroup(ctx context.Context, id int64) error {
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	g, err := s.GetGroup(ctx, id)
	if err != nil {
		return err
	}
	if g.IsPOSIX() && !a.super() {
		return errSuperOnly
	}
	var n int64
	if err := store.QueryRow(ctx, s.DB, "SELECT COUNT(*) FROM groups WHERE parent_id = ? AND deleted_at IS NULL", []any{id}, &n); err != nil {
		return err
	}
	if n > 0 {
		return conflict("group still has child groups")
	}
	if g.GIDNumber != 0 {
		if err := store.QueryRow(ctx, s.DB, "SELECT COUNT(*) FROM users WHERE gid_number = ? AND deleted_at IS NULL", []any{g.GIDNumber}, &n); err != nil {
			return err
		}
		if n > 0 {
			return conflict("group is the primary group of existing users")
		}
	}
	_, err = s.DB.Batch(ctx, []store.Statement{
		{Query: "DELETE FROM group_users WHERE group_id = ?", Args: []any{id}},
		{Query: "DELETE FROM groups WHERE id = ?", Args: []any{id}},
	})
	return err
}

// MembershipChange lists users to add to and remove from a group.
type MembershipChange struct {
	Add    []int64 `json:"add"`
	Remove []int64 `json:"remove"`
}

func (m MembershipChange) Validate() error {
	if len(m.Add)+len(m.Remove) == 0 || len(m.Add)+len(m.Remove) > 90 {
		return errors.New("provide 1-90 user ids to add or remove")
	}
	return nil
}

// ChangeMembers applies a MembershipChange in one batch.
func (s *Service) ChangeMembers(ctx context.Context, groupID int64, m MembershipChange) error {
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	g, err := s.GetGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if g.IsPOSIX() && !a.super() {
		return errSuperOnly
	}
	all := dedupe(append(append([]int64{}, m.Add...), m.Remove...))
	var n int64
	if err := store.QueryRow(ctx, s.DB, "SELECT COUNT(*) FROM users WHERE deleted_at IS NULL AND id IN ("+placeholders(len(all))+")", int64Args(all), &n); err != nil {
		return err
	}
	if int(n) != len(all) {
		return apperr.BadRequest("unknown user id")
	}
	var stmts []store.Statement
	for _, uid := range dedupe(m.Add) {
		stmts = append(stmts, store.Statement{Query: "INSERT OR IGNORE INTO group_users(group_id, user_id) VALUES (?, ?)", Args: []any{groupID, uid}})
	}
	for _, uid := range dedupe(m.Remove) {
		stmts = append(stmts, store.Statement{Query: "DELETE FROM group_users WHERE group_id = ? AND user_id = ?", Args: []any{groupID, uid}})
	}
	_, err = s.DB.Batch(ctx, stmts)
	return err
}
