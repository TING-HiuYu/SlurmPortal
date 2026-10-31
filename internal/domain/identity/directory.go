// Package identity owns users, roles and the live principal lookup.
package identity

import (
	"context"
	"errors"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const statusActive = 1

// Directory reads the existing users/roles/user_roles schema so data is preserved across the rewrite.
type Directory struct{ DB store.Querier }

// LoadPrincipal loads an active user's current roles for authentication.
func (d Directory) LoadPrincipal(ctx context.Context, userID int64) (*authn.Principal, error) {
	var (
		username string
		status   int
	)
	err := store.QueryRow(ctx, d.DB,
		"SELECT username, status FROM users WHERE id = ? AND deleted_at IS NULL",
		[]any{userID}, &username, &status)
	if errors.Is(err, store.ErrNoRows) {
		return nil, authn.ErrInactive
	}
	if err != nil {
		return nil, err
	}
	if status != statusActive {
		return nil, authn.ErrInactive
	}
	rows, err := d.DB.Query(ctx, `SELECT r.keyword FROM roles r
		JOIN user_roles ur ON ur.role_id = r.id
		WHERE ur.user_id = ? AND r.status = ? AND r.deleted_at IS NULL
		ORDER BY r.keyword`, userID, statusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := &authn.Principal{UserID: userID, Username: username}
	for rows.Next() {
		var kw string
		if err := rows.Scan(&kw); err != nil {
			return nil, err
		}
		p.Roles = append(p.Roles, kw)
	}
	return p, rows.Err()
}
