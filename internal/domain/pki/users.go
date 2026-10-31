package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

var (
	errDisabledUser = apperr.New(http.StatusConflict, "user_disabled", "user is disabled")
	errHasCert      = apperr.New(http.StatusConflict, "certificate_exists", "user already has a certificate; rotate it instead")
	errCAUnusable   = apperr.New(http.StatusConflict, "ca_unusable", "signing CA is disabled or expired")
	errNoAssignedCA = apperr.New(http.StatusConflict, "no_signing_ca", "no SSH signing CA is assigned to this account")
)

func (s *Service) user(ctx context.Context, id int64) (username, source string, err error) {
	var status int
	err = store.QueryRow(ctx, s.DB, "SELECT username, COALESCE(source,''), COALESCE(status,1) FROM users WHERE id = ? AND deleted_at IS NULL", []any{id}, &username, &source, &status)
	if errors.Is(err, store.ErrNoRows) {
		return "", "", apperr.ErrNotFound
	}
	if err == nil && status != 1 {
		err = errDisabledUser
	}
	return
}

func (s *Service) current(ctx context.Context, userID int64) (UserCertificate, error) {
	var c UserCertificate
	err := store.QueryRow(ctx, s.DB, `SELECT uc.id, uc.user_id, uc.certificate_authority_id, COALESCE(ca.kind,'x509'), uc.serial_hex, uc.certificate_pem
		FROM user_certificates uc JOIN certificate_authorities ca ON ca.id = uc.certificate_authority_id
		WHERE uc.user_id = ? AND uc.deleted_at IS NULL`, []any{userID}, &c.ID, &c.UserID, &c.CAID, &c.Kind, &c.SerialHex, &c.CertificatePEM)
	if errors.Is(err, store.ErrNoRows) {
		return c, errNoCert
	}
	c.ExpiresAt = certificateExpiry(c.CertificatePEM)
	return c, err
}

func certificateExpiry(text string) *time.Time {
	if k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(text)); err == nil {
		if c, ok := k.(*ssh.Certificate); ok && c.ValidBefore != ssh.CertTimeInfinity {
			t := time.Unix(int64(c.ValidBefore), 0).UTC()
			return &t
		}
		return nil
	}
	if b, _ := pem.Decode([]byte(text)); b != nil {
		if c, err := x509.ParseCertificate(b.Bytes); err == nil {
			t := c.NotAfter.UTC()
			return &t
		}
	}
	return nil
}

// Mine returns the caller's current public certificate; the private key is never stored.
func (s *Service) Mine(ctx context.Context, userID int64) (UserCertificate, error) {
	return s.current(ctx, userID)
}

func (s *Service) issue(ca CA, username string) (Issued, error) {
	if !ca.Enabled {
		return Issued{}, errCAUnusable
	}
	if ca.Kind == KindSSH {
		return s.issueSSH(ca, username)
	}
	cert, signer, err := s.x509Signer(ca)
	if err != nil {
		return Issued{}, err
	}
	now := s.now()
	if now.After(cert.NotAfter) {
		return Issued{}, errCAUnusable
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Issued{}, err
	}
	serial := randomSerial(128)
	leaf := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: username}, NotBefore: now.Add(-time.Minute),
		NotAfter: now.AddDate(x509UserYears, 0, 0), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if leaf.NotAfter.After(cert.NotAfter) {
		leaf.NotAfter = cert.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, cert, &key.PublicKey, signer)
	if err != nil {
		return Issued{}, err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Issued{}, err
	}
	return Issued{
		CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PrivateKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})),
		SerialHex:      hex.EncodeToString(serial.Bytes()), CAID: ca.ID, Kind: KindX509,
	}, nil
}

func (s *Service) issueSSH(ca CA, username string) (Issued, error) {
	signer, err := s.sshSigner(ca)
	if err != nil {
		return Issued{}, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Issued{}, err
	}
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return Issued{}, err
	}
	serial := randomSerial(63)
	now := s.now()
	cert := &ssh.Certificate{
		Key: pub, Serial: serial.Uint64(), CertType: ssh.UserCert, KeyId: username, ValidPrincipals: []string{username},
		ValidAfter: uint64(now.Add(-5 * time.Minute).Unix()), ValidBefore: uint64(now.Add(sshUserLifetime).Unix()),
		Permissions: ssh.Permissions{Extensions: map[string]string{
			"permit-pty": "", "permit-X11-forwarding": "", "permit-agent-forwarding": "", "permit-port-forwarding": "",
		}},
	}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		return Issued{}, err
	}
	block, err := ssh.MarshalPrivateKey(key, username)
	if err != nil {
		return Issued{}, err
	}
	return Issued{CertificatePEM: string(ssh.MarshalAuthorizedKey(cert)), PrivateKeyPEM: string(pem.EncodeToMemory(block)),
		SerialHex: hex.EncodeToString(serial.Bytes()), CAID: ca.ID, Kind: KindSSH}, nil
}

// Assign issues a user's first certificate.
func (s *Service) Assign(ctx context.Context, userID, caID int64) (Issued, error) {
	if !s.Ready() {
		return Issued{}, ErrNotConfigured
	}
	username, _, err := s.user(ctx, userID)
	if err != nil {
		return Issued{}, err
	}
	if _, err := s.current(ctx, userID); err == nil {
		return Issued{}, errHasCert
	} else if !errors.Is(err, errNoCert) {
		return Issued{}, err
	}
	ca, err := s.CA(ctx, caID)
	if err != nil {
		return Issued{}, apperr.BadRequest("unknown CA")
	}
	issued, err := s.issue(ca, username)
	if err != nil {
		return Issued{}, err
	}
	now := s.now()
	_, err = s.DB.Exec(ctx, `INSERT INTO user_certificates (created_at, updated_at, user_id, certificate_authority_id, serial_hex, certificate_pem)
		VALUES (?, ?, ?, ?, ?, ?)`, now, now, userID, caID, issued.SerialHex, issued.CertificatePEM)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return Issued{}, errHasCert
	}
	return issued, err
}

// Rotate replaces the current certificate and revokes the old serial atomically.
func (s *Service) Rotate(ctx context.Context, userID int64) (Issued, error) {
	if !s.Ready() {
		return Issued{}, ErrNotConfigured
	}
	var out Issued
	err := retry(func() error {
		username, _, err := s.user(ctx, userID)
		if err != nil {
			return err
		}
		cur, err := s.current(ctx, userID)
		if err != nil {
			return err
		}
		ca, err := s.CA(ctx, cur.CAID)
		if err != nil {
			return err
		}
		issued, err := s.issue(ca, username)
		if err != nil {
			return err
		}
		stmts, err := s.revokeStatements(ctx, ca, cur.SerialHex, ReasonSuperseded, store.Statement{
			Query: "UPDATE user_certificates SET certificate_pem = ?, serial_hex = ?, updated_at = ? WHERE id = ? AND serial_hex = ?",
			Args:  []any{issued.CertificatePEM, issued.SerialHex, s.now(), cur.ID, cur.SerialHex},
		})
		if err != nil {
			return err
		}
		if err := s.commitGuarded(ctx, stmts); err != nil {
			return err
		}
		out = issued
		return nil
	})
	return out, err
}

// IssueMine lets a user rotate their SSH certificate, or obtain a first one from their OAuth connector's CA.
func (s *Service) IssueMine(ctx context.Context, userID int64) (Issued, error) {
	cur, err := s.current(ctx, userID)
	if err == nil {
		if cur.Kind != KindSSH {
			return Issued{}, apperr.New(http.StatusConflict, "not_ssh", "current certificate is not an SSH certificate")
		}
		return s.Rotate(ctx, userID)
	}
	if !errors.Is(err, errNoCert) {
		return Issued{}, err
	}
	_, source, err := s.user(ctx, userID)
	if err != nil {
		return Issued{}, err
	}
	caID, err := s.connectorCA(ctx, source)
	if err != nil {
		return Issued{}, err
	}
	return s.Assign(ctx, userID, caID)
}

// connectorCA resolves "oauth-<provider>-<id>" to the enabled connector's SSH CA.
func (s *Service) connectorCA(ctx context.Context, source string) (int64, error) {
	dash := strings.LastIndex(source, "-")
	if !strings.HasPrefix(source, "oauth-") || dash < len("oauth-") {
		return 0, errNoAssignedCA
	}
	id, err := strconv.ParseInt(source[dash+1:], 10, 64)
	if err != nil || id <= 0 {
		return 0, errNoAssignedCA
	}
	var provider string
	var caID int64
	var enabled any
	err = store.QueryRow(ctx, s.DB, "SELECT provider, certificate_authority_id, enabled FROM oauth_connectors WHERE id = ? AND deleted_at IS NULL", []any{id}, &provider, &caID, &enabled)
	if err != nil || !truthy(enabled) || source != fmt.Sprintf("oauth-%s-%d", provider, id) {
		return 0, errNoAssignedCA
	}
	ca, err := s.CA(ctx, caID)
	if err != nil || ca.Kind != KindSSH {
		return 0, errNoAssignedCA
	}
	return caID, nil
}

// RevokeUser revokes and unassigns the user's certificate; safe to repeat when none exists.
func (s *Service) RevokeUser(ctx context.Context, userID int64) error {
	return retry(func() error {
		cur, err := s.current(ctx, userID)
		if errors.Is(err, errNoCert) {
			return nil
		}
		if err != nil {
			return err
		}
		ca, err := s.CA(ctx, cur.CAID)
		if err != nil {
			return err
		}
		stmts, err := s.revokeStatements(ctx, ca, cur.SerialHex, ReasonCessation, store.Statement{
			Query: "DELETE FROM user_certificates WHERE id = ?", Args: []any{cur.ID},
		})
		if err != nil {
			return err
		}
		return s.commitGuarded(ctx, stmts)
	})
}

// Revoke revokes any serial under a CA and drops a matching assignment; repeating it is harmless.
func (s *Service) Revoke(ctx context.Context, caID int64, serialHex string, reason int) error {
	serial, ok := new(big.Int).SetString(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(serialHex)), "0x"), 16)
	if !ok || serial.Sign() <= 0 || serial.BitLen() > 160 || reason < 0 || reason > 10 || reason == 7 {
		return apperr.BadRequest("invalid serial or CRL reason")
	}
	canonical := hex.EncodeToString(serial.Bytes())
	return retry(func() error {
		ca, err := s.CA(ctx, caID)
		if err != nil {
			return err
		}
		if ca.Kind == KindSSH && serial.BitLen() > 64 {
			return apperr.BadRequest("OpenSSH certificate serials fit in 64 bits")
		}
		stmts, err := s.revokeStatements(ctx, ca, canonical, reason, store.Statement{
			Query: "DELETE FROM user_certificates WHERE certificate_authority_id = ? AND serial_hex = ?", Args: []any{caID, canonical},
		})
		if err != nil {
			return err
		}
		return s.commitGuarded(ctx, stmts)
	})
}
