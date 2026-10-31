package identity

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const userColumns = `id, username, COALESCE(nickname,''), COALESCE(given_name,''), COALESCE(mail,''), COALESCE(job_number,''),
	COALESCE(mobile,''), COALESCE(uid_number,0), COALESCE(gid_number,0), COALESCE(home_directory,''), COALESCE(login_shell,''),
	COALESCE(avatar,''), COALESCE(postal_address,''), COALESCE(position,''), COALESCE(introduction,''), COALESCE(status,1),
	COALESCE(source,''), COALESCE(creator,''), COALESCE(user_dn,''), COALESCE(CAST(created_at AS TEXT),''), COALESCE(CAST(updated_at AS TEXT),'')`

func scanUser(rows store.Rows) (User, error) {
	var u User
	err := rows.Scan(&u.ID, &u.Username, &u.Nickname, &u.GivenName, &u.Mail, &u.JobNumber, &u.Mobile, &u.UIDNumber, &u.GIDNumber,
		&u.HomeDirectory, &u.LoginShell, &u.Avatar, &u.PostalAddress, &u.Position, &u.Introduction, &u.Status,
		&u.Source, &u.Creator, &u.UserDN, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

// UserFilter selects and pages the user list.
type UserFilter struct {
	Query   string
	Status  int
	GroupID int64
	Page    int
	Size    int
}

// UserPage is one page of users and the total match count.
type UserPage struct {
	Items []User `json:"items"`
	Total int64  `json:"total"`
}

// ListUsers returns the users matching f.
func (s *Service) ListUsers(ctx context.Context, f UserFilter) (UserPage, error) {
	where := []string{"deleted_at IS NULL"}
	var args []any
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where = append(where, `(username LIKE ? ESCAPE '\' OR nickname LIKE ? ESCAPE '\' OR mail LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like)
	}
	if f.Status != 0 {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.GroupID != 0 {
		where = append(where, "id IN (SELECT user_id FROM group_users WHERE group_id = ?)")
		args = append(args, f.GroupID)
	}
	cond := strings.Join(where, " AND ")
	var page UserPage
	if err := store.QueryRow(ctx, s.DB, "SELECT COUNT(*) FROM users WHERE "+cond, args, &page.Total); err != nil {
		return page, err
	}
	rows, err := s.DB.Query(ctx, "SELECT "+userColumns+" FROM users WHERE "+cond+" ORDER BY id LIMIT ? OFFSET ?",
		append(args, f.Size, (f.Page-1)*f.Size)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	page.Items = []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, u)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	return page, s.attachRelations(ctx, page.Items)
}

func (s *Service) attachRelations(ctx context.Context, users []User) error {
	if len(users) == 0 {
		return nil
	}
	idx := map[int64]*User{}
	ids := make([]int64, len(users))
	for i := range users {
		users[i].RoleIDs, users[i].GroupIDs = []int64{}, []int64{}
		idx[users[i].ID] = &users[i]
		ids[i] = users[i].ID
	}
	for _, q := range []struct {
		sql string
		dst func(*User, int64)
	}{
		{"SELECT user_id, role_id FROM user_roles WHERE user_id IN (" + placeholders(len(ids)) + ") ORDER BY role_id", func(u *User, v int64) { u.RoleIDs = append(u.RoleIDs, v) }},
		{"SELECT user_id, group_id FROM group_users WHERE user_id IN (" + placeholders(len(ids)) + ") ORDER BY group_id", func(u *User, v int64) { u.GroupIDs = append(u.GroupIDs, v) }},
	} {
		rows, err := s.DB.Query(ctx, q.sql, int64Args(ids)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var uid, v int64
			if err := rows.Scan(&uid, &v); err != nil {
				rows.Close()
				return err
			}
			q.dst(idx[uid], v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// GetUser returns one user that is not deleted.
func (s *Service) GetUser(ctx context.Context, id int64) (User, error) {
	rows, err := s.DB.Query(ctx, "SELECT "+userColumns+" FROM users WHERE id = ? AND deleted_at IS NULL", id)
	if err != nil {
		return User{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return User{}, err
		}
		return User{}, apperr.ErrNotFound
	}
	u, err := scanUser(rows)
	if err != nil {
		return User{}, err
	}
	rows.Close()
	us := []User{u}
	return us[0], s.attachRelations(ctx, us)
}

// UserInput is the body for creating a user.
type UserInput struct {
	Username      string `json:"username"`
	Nickname      string `json:"nickname"`
	GivenName     string `json:"givenName"`
	Mail          string `json:"mail"`
	JobNumber     string `json:"jobNumber"`
	Mobile        string `json:"mobile"`
	UIDNumber     int64  `json:"uidNumber"`
	GIDNumber     int64  `json:"gidNumber"`
	HomeDirectory string `json:"homeDirectory"`
	LoginShell    string `json:"loginShell"`
	Avatar        string `json:"avatar"`
	PostalAddress string `json:"postalAddress"`
	Position      string `json:"position"`
	Introduction  string `json:"introduction"`
	source        external
	Password      string  `json:"password"`
	GroupIDs      []int64 `json:"groupIds"`
	RoleIDs       []int64 `json:"roleIds"`
}

// Validate checks the new user's fields.
func (in UserInput) Validate() error {
	if err := ValidUsername(in.Username); err != nil {
		return err
	}
	for _, f := range []struct {
		name, v string
		max     int
	}{{"nickname", in.Nickname, 50}, {"givenName", in.GivenName, 50}, {"jobNumber", in.JobNumber, 20}, {"mobile", in.Mobile, 15},
		{"avatar", in.Avatar, 255}, {"postalAddress", in.PostalAddress, 255}, {"position", in.Position, 128}, {"introduction", in.Introduction, 255}} {
		if err := validText(f.name, f.v, f.max); err != nil {
			return err
		}
	}
	if err := validMail(in.Mail); err != nil {
		return err
	}
	if err := validPOSIX(in.UIDNumber, "uidNumber"); err != nil {
		return err
	}
	if err := validPOSIX(in.GIDNumber, "gidNumber"); err != nil {
		return err
	}
	if in.HomeDirectory != "" {
		if err := validHome(in.HomeDirectory); err != nil {
			return err
		}
	}
	if in.LoginShell != "" {
		if err := ValidShell(in.LoginShell); err != nil {
			return err
		}
	}
	if in.Password != "" {
		return password.CheckPolicy(in.Password)
	}
	return nil
}

// CreatedUser is the new user and, when the server generated it, the initial password.
type CreatedUser struct {
	User User `json:"user"`
	// InitialPassword is only present when the server generated it; it is never retrievable again.
	InitialPassword string `json:"initialPassword,omitempty"`
}

func (s *Service) rolesByIDs(ctx context.Context, ids []int64) ([]Role, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.DB.Query(ctx, "SELECT id, name, keyword, COALESCE(remark,''), COALESCE(status,1), COALESCE(sort,999), COALESCE(creator,'') FROM roles WHERE deleted_at IS NULL AND id IN ("+placeholders(len(ids))+")", int64Args(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.ID, &r.Name, &r.Keyword, &r.Remark, &r.Status, &r.Sort, &r.Creator); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, apperr.BadRequest("unknown role id")
	}
	return out, nil
}

func (s *Service) checkAssignable(a actor, roles []Role) error {
	for _, r := range roles {
		if !a.outranks(r.Sort) {
			return errOutranked
		}
	}
	return nil
}

// resolvePOSIX settles the account's POSIX identity. Every account has a main group: the group of
// gidNumber, or the single GID-bearing selected group. Home and shell default to that group's presets
// (home = <homeRoot>/<username>); there is no global default home. UID comes from the persistent counter.
func (s *Service) resolvePOSIX(ctx context.Context, in *UserInput, groups []Group) (autoUID bool, err error) {
	var primary *Group
	if in.GIDNumber != 0 {
		g, err := s.groupBy(ctx, "gid_number = ?", in.GIDNumber)
		if err != nil {
			return false, apperr.BadRequest(fmt.Sprintf("gidNumber %d has no managed group", in.GIDNumber))
		}
		primary = &g
	} else {
		for i := range groups {
			if groups[i].GIDNumber == 0 {
				continue
			}
			if primary != nil && primary.GIDNumber != groups[i].GIDNumber {
				return false, apperr.BadRequest("several selected groups carry a GID; choose one main group")
			}
			primary = &groups[i]
		}
		if primary == nil {
			return false, errNoMainGroup
		}
		in.GIDNumber = primary.GIDNumber
	}
	if err := validPOSIX(primary.GIDNumber, "primary gid"); err != nil || primary.GIDNumber == 0 {
		return false, apperr.BadRequest("primary group GID is outside the user range")
	}
	if in.UIDNumber == 0 {
		if in.UIDNumber, err = nextID(ctx, s.DB, "uid"); err != nil {
			return false, err
		}
		autoUID = true
	}
	if in.HomeDirectory == "" {
		if primary.HomeRoot == "" {
			return false, apperr.BadRequest(fmt.Sprintf("main group %s has no home root preset; set homeDirectory or the group's preset", primary.Name))
		}
		in.HomeDirectory = path.Join(primary.HomeRoot, in.Username)
	}
	if in.LoginShell == "" {
		in.LoginShell = primary.LoginShell
	}
	if in.LoginShell == "" {
		in.LoginShell = s.Layout.DefaultShell
	}
	return autoUID, nil
}

// CreateUser adds a user with a UID, a main POSIX group and the requested extra groups and roles.
func (s *Service) CreateUser(ctx context.Context, in UserInput) (CreatedUser, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return CreatedUser{}, err
	}
	in.RoleIDs, in.GroupIDs = dedupe(in.RoleIDs), dedupe(in.GroupIDs)
	if len(in.RoleIDs) == 0 {
		var id int64
		if err := store.QueryRow(ctx, s.DB, "SELECT id FROM roles WHERE keyword = 'user' AND deleted_at IS NULL", nil, &id); err == nil {
			in.RoleIDs = []int64{id}
		}
	}
	roles, err := s.rolesByIDs(ctx, in.RoleIDs)
	if err != nil {
		return CreatedUser{}, err
	}
	if err := s.checkAssignable(a, roles); err != nil {
		return CreatedUser{}, err
	}
	groups, err := s.groupsByIDs(ctx, in.GroupIDs)
	if err != nil {
		return CreatedUser{}, err
	}
	autoUID, err := s.resolvePOSIX(ctx, &in, groups)
	if err != nil {
		return CreatedUser{}, err
	}
	if !a.super() {
		for _, g := range groups {
			if g.IsPOSIX() && g.GIDNumber != in.GIDNumber {
				return CreatedUser{}, errSuperOnly
			}
		}
	}
	out := CreatedUser{}
	pw := in.Password
	if pw == "" {
		pw = randomPassword()
		out.InitialPassword = pw
	}
	cred, err := password.Hash(pw)
	if err != nil {
		return CreatedUser{}, apperr.BadRequest(err.Error())
	}

	for attempt := 0; ; attempt++ {
		err = s.insertUser(ctx, a, in, cred)
		if err == nil || !autoUID || !isUnique(err, "uid_number") || attempt == 4 {
			break
		}
		if in.UIDNumber, err = nextID(ctx, s.DB, "uid"); err != nil {
			return CreatedUser{}, err
		}
	}
	switch {
	case isUnique(err, "username"):
		return CreatedUser{}, conflict("username already exists")
	case isUnique(err, "mail"):
		return CreatedUser{}, conflict("mail already in use")
	case isUnique(err, "uid_number"):
		return CreatedUser{}, conflict("uidNumber already in use")
	case err != nil:
		return CreatedUser{}, err
	}
	var id int64
	if err := store.QueryRow(ctx, s.DB, "SELECT id FROM users WHERE username = ?", []any{in.Username}, &id); err != nil {
		return CreatedUser{}, err
	}
	out.User, err = s.GetUser(ctx, id)
	return out, err
}

func (s *Service) insertUser(ctx context.Context, a actor, in UserInput, cred password.Credential) error {
	now := s.now().UTC()
	idSub := "(SELECT id FROM users WHERE username = ?)"
	src := in.source
	if src.name == "" {
		src.name = "platform"
	}
	if src.status == 0 {
		src.status = StatusActive
	}
	stmts := []store.Statement{{
		Query: `INSERT INTO users (created_at, updated_at, username, password, nickname, given_name, mail, job_number, mobile,
			uid_number, gid_number, home_directory, login_shell, avatar, postal_address, departments, position, introduction,
			status, creator, source, department_id, source_user_id, source_union_id, user_dn, sync_state)
			VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, '', ?, ?, ?, 1)`,
		Args: []any{now, now, in.Username, in.Nickname, in.GivenName, in.Mail, in.JobNumber, in.Mobile,
			in.UIDNumber, in.GIDNumber, in.HomeDirectory, in.LoginShell, in.Avatar, in.PostalAddress, in.Position, in.Introduction,
			src.status, a.p.Username, src.name, src.userID, src.unionID, "uid=" + EscapeDNValue(in.Username) + "," + s.Layout.UserDN},
	}, {
		Query: "INSERT INTO v2_user_credentials(user_id, app_hash, ldap_hash, updated_at) VALUES (" + idSub + ", ?, ?, ?)",
		Args:  []any{in.Username, cred.App, cred.LDAP, now},
	}}
	for _, rid := range in.RoleIDs {
		stmts = append(stmts, store.Statement{Query: "INSERT INTO user_roles(user_id, role_id) VALUES (" + idSub + ", ?)", Args: []any{in.Username, rid}})
	}
	for _, gid := range in.GroupIDs {
		stmts = append(stmts, store.Statement{Query: "INSERT INTO group_users(user_id, group_id) VALUES (" + idSub + ", ?)", Args: []any{in.Username, gid}})
	}
	_, err := s.DB.Batch(ctx, stmts)
	return err
}

// UserPatch holds the user fields an update may change; nil fields are left alone.
type UserPatch struct {
	Nickname      *string  `json:"nickname"`
	GivenName     *string  `json:"givenName"`
	Mail          *string  `json:"mail"`
	JobNumber     *string  `json:"jobNumber"`
	Mobile        *string  `json:"mobile"`
	HomeDirectory *string  `json:"homeDirectory"`
	LoginShell    *string  `json:"loginShell"`
	Avatar        *string  `json:"avatar"`
	PostalAddress *string  `json:"postalAddress"`
	Position      *string  `json:"position"`
	Introduction  *string  `json:"introduction"`
	GroupIDs      *[]int64 `json:"groupIds"`
	RoleIDs       *[]int64 `json:"roleIds"`
}

func (p UserPatch) Validate() error {
	str := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	in := UserInput{Username: "x", Nickname: str(p.Nickname), GivenName: str(p.GivenName), Mail: str(p.Mail), JobNumber: str(p.JobNumber),
		Mobile: str(p.Mobile), HomeDirectory: str(p.HomeDirectory), LoginShell: str(p.LoginShell), Avatar: str(p.Avatar),
		PostalAddress: str(p.PostalAddress), Position: str(p.Position), Introduction: str(p.Introduction)}
	if p.HomeDirectory != nil && *p.HomeDirectory == "" || p.LoginShell != nil && *p.LoginShell == "" {
		return errors.New("homeDirectory and loginShell cannot be cleared")
	}
	return in.Validate()
}

// guardTarget enforces: never yourself, and only users you outrank.
func (s *Service) guardTarget(ctx context.Context, a actor, id int64) error {
	if id == a.p.UserID {
		return errSelf
	}
	rank, err := s.userRank(ctx, id)
	if err != nil {
		return err
	}
	if !a.outranks(rank) {
		return errOutranked
	}
	return nil
}

// UpdateUser changes a user the caller outranks.
func (s *Service) UpdateUser(ctx context.Context, id int64, p UserPatch) (User, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return User{}, err
	}
	cur, err := s.GetUser(ctx, id)
	if err != nil {
		return User{}, err
	}
	if p.RoleIDs != nil || p.GroupIDs != nil || id != a.p.UserID {
		if err := s.guardTarget(ctx, a, id); err != nil {
			return User{}, err
		}
	}
	now := s.now().UTC()
	sets, args := []string{"updated_at = ?"}, []any{now}
	for _, f := range []struct {
		col string
		v   *string
	}{{"nickname", p.Nickname}, {"given_name", p.GivenName}, {"mail", p.Mail}, {"job_number", p.JobNumber}, {"mobile", p.Mobile},
		{"home_directory", p.HomeDirectory}, {"login_shell", p.LoginShell}, {"avatar", p.Avatar}, {"postal_address", p.PostalAddress},
		{"position", p.Position}, {"introduction", p.Introduction}} {
		if f.v != nil {
			sets, args = append(sets, f.col+" = ?"), append(args, *f.v)
		}
	}
	stmts := []store.Statement{{Query: "UPDATE users SET " + strings.Join(sets, ", ") + " WHERE id = ?", Args: append(args, id)}}

	if p.RoleIDs != nil {
		ids := dedupe(*p.RoleIDs)
		roles, err := s.rolesByIDs(ctx, ids)
		if err != nil {
			return User{}, err
		}
		if err := s.checkAssignable(a, roles); err != nil {
			return User{}, err
		}
		stmts = append(stmts, store.Statement{Query: "DELETE FROM user_roles WHERE user_id = ?", Args: []any{id}})
		for _, rid := range ids {
			stmts = append(stmts, store.Statement{Query: "INSERT INTO user_roles(user_id, role_id) VALUES (?, ?)", Args: []any{id, rid}})
		}
	}
	if p.GroupIDs != nil {
		ids := dedupe(*p.GroupIDs)
		groups, err := s.groupsByIDs(ctx, ids)
		if err != nil {
			return User{}, err
		}
		before, err := s.groupsByIDs(ctx, cur.GroupIDs)
		if err != nil {
			return User{}, err
		}
		if !a.super() && posixSet(before) != posixSet(groups) {
			return User{}, errSuperOnly
		}
		// A POSIX account has exactly one main group, fixed by its gidNumber (v1 rule): editing
		// memberships may add or drop supplementary groups but never drop the main one.
		if m := mainGroups(before, cur.GIDNumber); cur.GIDNumber != 0 && m != "" && mainGroups(groups, cur.GIDNumber) == "" {
			return User{}, apperr.BadRequest("the main POSIX group cannot be removed when editing; only supplementary groups can")
		}
		stmts = append(stmts, store.Statement{Query: "DELETE FROM group_users WHERE user_id = ?", Args: []any{id}})
		for _, gid := range ids {
			stmts = append(stmts, store.Statement{Query: "INSERT INTO group_users(user_id, group_id) VALUES (?, ?)", Args: []any{id, gid}})
		}
	}
	if _, err := s.DB.Batch(ctx, stmts); err != nil {
		if isUnique(err, "mail") {
			return User{}, conflict("mail already in use")
		}
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

// mainGroups lists the POSIX groups carrying gid, i.e. the account's main group.
func mainGroups(gs []Group, gid int64) string {
	var ids []string
	for _, g := range gs {
		if g.IsPOSIX() && g.GIDNumber == gid {
			ids = append(ids, strconv.FormatInt(g.ID, 10))
		}
	}
	return strings.Join(ids, ",")
}

func posixSet(gs []Group) string {
	var ids []string
	for _, g := range gs {
		if g.IsPOSIX() {
			ids = append(ids, strconv.FormatInt(g.ID, 10))
		}
	}
	return strings.Join(ids, ",")
}

const (
	StatusActive   = 1
	StatusDisabled = 2
)

// SetStatus enables or disables a user. Disabling runs OnDeactivate first, which revokes the user's
// certificates.
func (s *Service) SetStatus(ctx context.Context, id int64, status int) error {
	if status != StatusActive && status != StatusDisabled {
		return apperr.BadRequest("status must be 1 (active) or 2 (disabled)")
	}
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	if _, err := s.GetUser(ctx, id); err != nil {
		return err
	}
	if err := s.guardTarget(ctx, a, id); err != nil {
		return err
	}
	stmts := []store.Statement{{Query: "UPDATE users SET status = ?, updated_at = ? WHERE id = ?", Args: []any{status, s.now().UTC(), id}}}
	if status == StatusDisabled && s.OnDeactivate != nil {
		if err := s.OnDeactivate(ctx, id); err != nil {
			return err
		}
	}
	_, err = s.DB.Batch(ctx, stmts)
	return err
}

// ResetPassword sets a random password for another user and returns it once.
func (s *Service) ResetPassword(ctx context.Context, id int64) (string, error) {
	a, err := s.actor(ctx)
	if err != nil {
		return "", err
	}
	if _, err := s.GetUser(ctx, id); err != nil {
		return "", err
	}
	if err := s.guardTarget(ctx, a, id); err != nil {
		return "", err
	}
	pw := randomPassword()
	cred, err := password.Hash(pw)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	_, err = s.DB.Batch(ctx, []store.Statement{{
		Query: `INSERT INTO v2_user_credentials(user_id, app_hash, ldap_hash, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET app_hash = excluded.app_hash, ldap_hash = excluded.ldap_hash, updated_at = excluded.updated_at`,
		Args: []any{id, cred.App, cred.LDAP, now},
	}, {Query: "UPDATE users SET updated_at = ? WHERE id = ?", Args: []any{now, id}}})
	return pw, err
}

// DeleteUser hard-deletes like v1; POSIX counters keep the number reserved.
func (s *Service) DeleteUser(ctx context.Context, id int64) error {
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	if _, err := s.GetUser(ctx, id); err != nil {
		return err
	}
	if err := s.guardTarget(ctx, a, id); err != nil {
		return err
	}
	if s.OnDeactivate != nil {
		if err := s.OnDeactivate(ctx, id); err != nil {
			return err
		}
	}
	var stmts []store.Statement
	for _, q := range []string{
		"DELETE FROM user_roles WHERE user_id = ?",
		"DELETE FROM group_users WHERE user_id = ?",
		"DELETE FROM v2_user_credentials WHERE user_id = ?",
		"DELETE FROM users WHERE id = ?",
	} {
		stmts = append(stmts, store.Statement{Query: q, Args: []any{id}})
	}
	_, err = s.DB.Batch(ctx, stmts)
	return err
}

// CanManage reports whether the caller may act on another user's account (not self, strictly outranked).
func (s *Service) CanManage(ctx context.Context, id int64) error {
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	if _, err := s.GetUser(ctx, id); err != nil {
		return err
	}
	return s.guardTarget(ctx, a, id)
}

// BootstrapAdmin creates the first super administrator when none exists; afterwards it is a no-op,
// so a leftover BOOTSTRAP_ADMIN_PASSWORD can never reset an existing account.
func (s *Service) BootstrapAdmin(ctx context.Context, username, pw string) (bool, error) {
	var n int64
	if err := store.QueryRow(ctx, s.DB, `SELECT COUNT(*) FROM users u JOIN user_roles ur ON ur.user_id = u.id
		JOIN roles r ON r.id = ur.role_id WHERE r.sort = ? AND r.deleted_at IS NULL AND u.deleted_at IS NULL`, []any{superSort}, &n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	if err := ValidUsername(username); err != nil {
		return false, err
	}
	cred, err := password.Hash(pw)
	if err != nil {
		return false, err
	}
	var roleID int64
	if err := store.QueryRow(ctx, s.DB, "SELECT id FROM roles WHERE sort = ? AND deleted_at IS NULL ORDER BY id LIMIT 1", []any{superSort}, &roleID); err != nil {
		return false, fmt.Errorf("super administrator role missing: %w", err)
	}
	a := actor{p: &authn.Principal{Username: "bootstrap"}, minSort: superSort}
	in := UserInput{Username: username, RoleIDs: []int64{roleID}}
	if err := s.insertUser(ctx, a, in, cred); err != nil {
		return false, err
	}
	return true, nil
}

// SetMyShell changes only the caller's login shell, and only for complete POSIX accounts.
func (s *Service) SetMyShell(ctx context.Context, shell string) error {
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	if err := ValidShell(shell); err != nil {
		return apperr.BadRequest(err.Error())
	}
	u, err := s.GetUser(ctx, a.p.UserID)
	if err != nil {
		return err
	}
	if u.UIDNumber == 0 || u.GIDNumber == 0 || u.HomeDirectory == "" {
		return apperr.BadRequest("account has no complete POSIX identity")
	}
	_, err = s.DB.Exec(ctx, "UPDATE users SET login_shell = ?, updated_at = ? WHERE id = ?", shell, s.now().UTC(), u.ID)
	return err
}
