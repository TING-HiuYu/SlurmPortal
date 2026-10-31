package identity

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

type external struct {
	name, userID, unionID string
	status                int
}

// ExternalProfile is an identity already verified by an IdP plugin (OAuth, Access).
type ExternalProfile struct {
	Email        string
	UsernameHint string
	Name         string
	Avatar       string
	Source       string
	SubjectID    string
}

// Provisioning describes how a first-time external login becomes an account.
// MainGroupID (a POSIX group) is required: it gives the account its GID, home and shell presets.
// GroupIDs are further groups the account joins.
type Provisioning struct {
	RoleIDs     []int64
	MainGroupID int64
	GroupIDs    []int64
	Status      int
}

var (
	ErrNoAccount       = apperr.New(http.StatusForbidden, "no_matching_user", "no active account matches this identity")
	errAccountDisabled = apperr.New(http.StatusForbidden, "account_disabled", "account is disabled")
	errAmbiguous       = apperr.New(http.StatusConflict, "ambiguous_identity", "several accounts share this email")
	unsafeName         = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
)

// FindByEmail returns the single active account with this email (case-insensitive).
func (s *Service) FindByEmail(ctx context.Context, email string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return 0, ErrNoAccount
	}
	// trim(mail) <> '' lets SQLite use the partial unique index on lower(trim(mail)) (email is never
	// empty here), so a sign-in reads one row instead of every user.
	rows, err := s.DB.Query(ctx, "SELECT id, status FROM users WHERE lower(trim(mail)) = ? AND trim(mail) <> '' AND deleted_at IS NULL LIMIT 2", email)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	var status int
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id, &status); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	switch {
	case len(ids) == 0:
		return 0, ErrNoAccount
	case len(ids) > 1:
		return 0, errAmbiguous
	case status != StatusActive:
		return 0, errAccountDisabled
	}
	return ids[0], nil
}

// CheckProvisioning verifies the caller could have created such an account themselves, so an IdP
// configuration cannot grant roles or groups beyond the configurer's own reach.
func (s *Service) CheckProvisioning(ctx context.Context, prov Provisioning) error {
	a, err := s.actor(ctx)
	if err != nil {
		return err
	}
	roles, err := s.rolesByIDs(ctx, dedupe(prov.RoleIDs))
	if err != nil {
		return err
	}
	if err := s.checkAssignable(a, roles); err != nil {
		return err
	}
	groups, err := s.groupsByIDs(ctx, dedupe(prov.GroupIDs))
	if err != nil {
		return err
	}
	for _, g := range groups {
		if g.IsPOSIX() && !a.super() {
			return errOutranked
		}
	}
	if prov.MainGroupID == 0 {
		return errNoMainGroup
	}
	main, err := s.groupsByIDs(ctx, []int64{prov.MainGroupID})
	if err != nil {
		return err
	}
	if !main[0].IsPOSIX() || main[0].GIDNumber == 0 {
		return apperr.BadRequest("the main group must be a POSIX group with a GID")
	}
	if !a.super() {
		return errOutranked
	}
	if prov.Status != StatusActive && prov.Status != StatusDisabled {
		return apperr.BadRequest("status must be 1 (active) or 2 (disabled)")
	}
	return nil
}

// LinkOrProvision links a verified external identity to the account with the same email, or creates
// one when prov is non-nil. A created account's random password is returned once.
func (s *Service) LinkOrProvision(ctx context.Context, p ExternalProfile, prov *Provisioning) (id int64, initialPassword string, err error) {
	email := strings.ToLower(strings.TrimSpace(p.Email))
	if email == "" || p.Source == "" || p.SubjectID == "" {
		return 0, "", ErrNoAccount
	}
	id, err = s.FindByEmail(ctx, email)
	if err == nil {
		_, err = s.DB.Exec(ctx, "UPDATE users SET source = ?, source_user_id = ?, source_union_id = ?, updated_at = ? WHERE id = ?",
			p.Source, p.SubjectID, email, s.now().UTC(), id)
		return id, "", err
	}
	if !errors.Is(err, ErrNoAccount) || prov == nil {
		return 0, "", err
	}

	base := strings.Trim(unsafeName.ReplaceAllString(p.UsernameHint, "-"), "-._")
	if len(base) > 40 {
		base = base[:40]
	}
	if ValidUsername(base) != nil {
		base = "user"
	}
	name := base
	for i := 1; ; i++ {
		var n int64
		if err := store.QueryRow(ctx, s.DB, "SELECT COUNT(*) FROM users WHERE username = ?", []any{name}, &n); err != nil {
			return 0, "", err
		}
		if n == 0 {
			break
		}
		if i > 100 {
			return 0, "", conflict("could not find a free username")
		}
		name = base + "-" + strconv.Itoa(i)
	}
	trim := func(v string, max int) string {
		if r := []rune(v); len(r) > max {
			return string(r[:max])
		}
		return v
	}
	in := UserInput{Username: name, Nickname: trim(p.Name, 50), GivenName: trim(p.Name, 50), Mail: email, Avatar: trim(p.Avatar, 255),
		RoleIDs: dedupe(prov.RoleIDs), GroupIDs: dedupe(prov.GroupIDs),
		source: external{name: p.Source, userID: p.SubjectID, unionID: email, status: prov.Status}}
	if _, err := s.rolesByIDs(ctx, in.RoleIDs); err != nil {
		return 0, "", err
	}
	groups, err := s.groupsByIDs(ctx, in.GroupIDs)
	if err != nil {
		return 0, "", err
	}
	// The rule's main group is the account's main group; home and shell come from its presets.
	if prov.MainGroupID == 0 {
		return 0, "", errNoMainGroup
	}
	for _, g := range groups {
		if g.IsPOSIX() && g.ID != prov.MainGroupID {
			return 0, "", apperr.BadRequest("a provisioned account can have only one main POSIX group")
		}
	}
	main, err := s.groupsByIDs(ctx, []int64{prov.MainGroupID})
	if err != nil {
		return 0, "", err
	}
	if !main[0].IsPOSIX() || main[0].GIDNumber == 0 {
		return 0, "", apperr.BadRequest("the main group must be a POSIX group with a GID")
	}
	in.GIDNumber = main[0].GIDNumber
	if !slices.Contains(in.GroupIDs, prov.MainGroupID) {
		in.GroupIDs = append(in.GroupIDs, prov.MainGroupID)
	}
	autoUID, err := s.resolvePOSIX(ctx, &in, groups)
	if err != nil {
		return 0, "", err
	}
	initialPassword = randomPassword()
	cred, err := password.Hash(initialPassword)
	if err != nil {
		return 0, "", err
	}
	sys := actor{p: &authn.Principal{Username: p.Source}, minSort: superSort}
	for attempt := 0; ; attempt++ {
		err = s.insertUser(ctx, sys, in, cred)
		if err == nil || !autoUID || !isUnique(err, "uid_number") || attempt == 4 {
			break
		}
		if in.UIDNumber, err = nextID(ctx, s.DB, "uid"); err != nil {
			return 0, "", err
		}
	}
	if err != nil {
		return 0, "", err
	}
	if err := store.QueryRow(ctx, s.DB, "SELECT id FROM users WHERE username = ?", []any{name}, &id); err != nil {
		return 0, "", err
	}
	return id, initialPassword, nil
}
