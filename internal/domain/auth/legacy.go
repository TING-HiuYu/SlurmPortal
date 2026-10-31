package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/password"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

// LegacyDecrypter opens the RSA-PKCS1v15 + base64 ciphertext that v1 stored in users.password.
type LegacyDecrypter struct{ key *rsa.PrivateKey }

// ParseLegacyKey reads v1's RSA private key, which decrypts the passwords v1 stored reversibly.
func ParseLegacyKey(pemBytes []byte) (LegacyDecrypter, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return LegacyDecrypter{}, errors.New("legacy RSA key: no PEM block")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return LegacyDecrypter{}, fmt.Errorf("legacy RSA key: %w", err)
	}
	return LegacyDecrypter{key: key}, nil
}

// Decrypt returns the plaintext of one v1 password ciphertext.
func (d LegacyDecrypter) Decrypt(ciphertext string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	pt, err := rsa.DecryptPKCS1v15(nil, d.key, raw)
	if err != nil {
		return "", err
	}
	if len(pt) == 0 {
		return "", errors.New("empty password")
	}
	return string(pt), nil
}

// ConversionReport summarises a v1 password conversion run (cmd/convert-passwords).
type ConversionReport struct {
	Converted []int64
	Existing  int
	Failed    map[int64]string
}

// ConvertLegacy derives one-way credentials for users lacking them. With apply=false nothing is written.
// users.password is left untouched so blue/green rollback keeps working; purge it separately after cutover.
func ConvertLegacy(ctx context.Context, db store.Store, dec LegacyDecrypter, apply bool) (ConversionReport, error) {
	rep := ConversionReport{Failed: map[int64]string{}}
	var migrated int
	if err := store.QueryRow(ctx, db, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'v2_user_credentials'", nil, &migrated); err != nil {
		return rep, err
	}
	if migrated == 0 && apply {
		return rep, errors.New("v2 migrations have not been applied")
	}
	query := `SELECT u.id, u.password, c.user_id IS NOT NULL FROM users u
		LEFT JOIN v2_user_credentials c ON c.user_id = u.id
		WHERE u.deleted_at IS NULL ORDER BY u.id`
	if migrated == 0 {
		query = "SELECT id, password, 0 FROM users WHERE deleted_at IS NULL ORDER BY id"
	}
	rows, err := db.Query(ctx, query)
	if err != nil {
		return rep, err
	}
	type pending struct {
		id   int64
		cred password.Credential
	}
	var todo []pending
	for rows.Next() {
		var (
			id     int64
			cipher string
			has    bool
		)
		if err := rows.Scan(&id, &cipher, &has); err != nil {
			rows.Close()
			return rep, err
		}
		if has {
			rep.Existing++
			continue
		}
		pt, err := dec.Decrypt(cipher)
		if err != nil {
			rep.Failed[id] = "undecryptable legacy password"
			continue
		}
		c, err := password.Derive(pt)
		if err != nil {
			rep.Failed[id] = err.Error()
			continue
		}
		todo = append(todo, pending{id, c})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, err
	}
	rows.Close()

	now := time.Now().UTC()
	const chunk = 90
	for start := 0; start < len(todo); start += chunk {
		part := todo[start:min(start+chunk, len(todo))]
		stmts := make([]store.Statement, len(part))
		for i, p := range part {
			stmts[i] = store.Statement{
				Query: "INSERT INTO v2_user_credentials(user_id, app_hash, ldap_hash, updated_at) VALUES (?, ?, ?, ?)",
				Args:  []any{p.id, p.cred.App, p.cred.LDAP, now},
			}
		}
		if apply {
			if _, err := db.Batch(ctx, stmts); err != nil {
				return rep, err
			}
		}
		for _, p := range part {
			rep.Converted = append(rep.Converted, p.id)
		}
	}
	return rep, nil
}
