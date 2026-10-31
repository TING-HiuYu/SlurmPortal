package identity

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// Layout is the directory shape used to derive DNs. Home and shell presets live on each POSIX
// group; DefaultShell only applies when the main group has no shell preset.
type Layout struct {
	BaseDN       string
	UserDN       string
	DefaultShell string
}

// DeactivateHook runs before a user is disabled or deleted (e.g. certificate revocation);
// if the later user change fails the hook's effect deliberately stays.
type DeactivateHook func(ctx context.Context, userID int64) error

// Service owns users, groups and roles. Every mutation checks that the caller outranks the target
// and never grants a permission the caller does not hold.
type Service struct {
	DB           store.Store
	Layout       Layout
	Policies     *authz.Casbin
	OnDeactivate DeactivateHook
	Now          func() time.Time
}

const superSort = 1

var (
	errSelf        = apperr.New(http.StatusForbidden, "self_modification", "you cannot change your own roles, status or account")
	errOutranked   = apperr.New(http.StatusForbidden, "insufficient_rank", "target has an equal or higher role than you")
	errNoMainGroup = apperr.New(http.StatusBadRequest, "main_group_required", "an account needs a main POSIX group")
	errSuperOnly   = apperr.New(http.StatusForbidden, "super_admin_required", "only a super administrator may do this")
)

type actor struct {
	p       *authn.Principal
	minSort int
}

func (a actor) super() bool { return a.minSort == superSort }

// outranks is true when a may manage something whose best role sort is target.
func (a actor) outranks(target int) bool { return a.super() || a.minSort < target }

func (s *Service) actor(ctx context.Context) (actor, error) {
	p := authn.FromContext(ctx)
	if p == nil {
		return actor{}, apperr.ErrUnauthenticated
	}
	a := actor{p: p, minSort: 1 << 30}
	if len(p.Roles) == 0 {
		return a, nil
	}
	args := make([]any, 0, len(p.Roles))
	for _, r := range p.Roles {
		args = append(args, r)
	}
	var m *int64
	err := store.QueryRow(ctx, s.DB, "SELECT MIN(sort) FROM roles WHERE status = 1 AND deleted_at IS NULL AND keyword IN ("+placeholders(len(args))+")", args, &m)
	if err != nil {
		return a, err
	}
	if m != nil {
		a.minSort = int(*m)
	}
	return a, nil
}

// userRank is the best (lowest) active role sort of a user; users without roles rank lowest.
func (s *Service) userRank(ctx context.Context, userID int64) (int, error) {
	var m *int64
	err := store.QueryRow(ctx, s.DB, `SELECT MIN(r.sort) FROM roles r JOIN user_roles ur ON ur.role_id = r.id
		WHERE ur.user_id = ? AND r.deleted_at IS NULL`, []any{userID}, &m)
	if err != nil || m == nil {
		return 1 << 30, err
	}
	return int(*m), nil
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, v := range ids {
		out[i] = v
	}
	return out
}

func dedupe(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func isUnique(err error, column string) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") && (column == "" || strings.Contains(err.Error(), column))
}

func conflict(msg string) error { return apperr.New(http.StatusConflict, "conflict", msg) }

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// nextID reserves a POSIX number; reservations are never returned, matching the v1 allocator.
func nextID(ctx context.Context, q store.Querier, kind string) (int64, error) {
	var id int64
	err := store.QueryRow(ctx, q, "UPDATE posix_id_counters SET last_id = last_id + 1 WHERE kind = ? AND last_id < ? RETURNING last_id", []any{kind, MaxPOSIXID}, &id)
	if errors.Is(err, store.ErrNoRows) {
		return 0, fmt.Errorf("%s space exhausted or counter missing", kind)
	}
	return id, err
}

const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789-_"

func randomPassword() string {
	b := make([]byte, 20)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(passwordAlphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = passwordAlphabet[n.Int64()]
	}
	return string(b)
}
