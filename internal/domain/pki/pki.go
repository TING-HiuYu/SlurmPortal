// Package pki owns CA signing keys on master. Private keys are AES-256-GCM encrypted with the master key,
// bound to the CA's public material, and never leave this package except as one-time issuance output.
package pki

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const (
	KindX509 = "x509"
	KindSSH  = "ssh"

	crlLifetime     = 48 * time.Hour
	sshUserLifetime = 30 * 24 * time.Hour
	x509UserYears   = 1

	ReasonUnspecified = 0
	ReasonSuperseded  = 4
	ReasonCessation   = 5
)

var (
	ErrNotConfigured = apperr.New(http.StatusServiceUnavailable, "pki_unconfigured", "PKI master key is not configured")
	ErrChanged       = apperr.New(http.StatusConflict, "certificate_changed", "certificate changed concurrently; reload and retry")
	errNoCert        = apperr.New(http.StatusNotFound, "no_certificate", "user has no certificate")
)

// CA is a certificate authority as the API returns it (never with its private key).
type CA struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Kind           string `json:"kind"`
	CertificatePEM string `json:"certificatePem"`
	CRLPEM         string `json:"crlPem,omitempty"`
	CRLNumber      int64  `json:"crlNumber"`
	Enabled        bool   `json:"enabled"`
	CreatedAt      string `json:"createdAt"`

	ciphertext string
}

// Issued is a newly issued certificate with its private key, returned once to the user.
type Issued struct {
	CertificatePEM string `json:"certificatePem"`
	PrivateKeyPEM  string `json:"privateKeyPem"`
	SerialHex      string `json:"serialHex"`
	CAID           int64  `json:"caId"`
	Kind           string `json:"kind"`
}

// UserCertificate is a stored user certificate and its revocation state.
type UserCertificate struct {
	ID             int64  `json:"id"`
	UserID         int64  `json:"userId"`
	CAID           int64  `json:"caId"`
	Kind           string `json:"kind"`
	SerialHex      string `json:"serialHex"`
	CertificatePEM string `json:"certificatePem"`
	// ExpiresAt is read from the certificate itself (OpenSSH ValidBefore or X.509 NotAfter).
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// Service manages CAs and user certificates. CA private keys are sealed with PKI_MASTER_KEY.
type Service struct {
	DB  store.Store
	key []byte
	Now func() time.Time
}

// New verifies the master key can open every stored CA so a wrong key fails at startup, not at first use.
func New(ctx context.Context, db store.Store, encodedMasterKey string) (*Service, error) {
	s := &Service{DB: db, Now: time.Now}
	if encodedMasterKey != "" {
		key, err := base64.StdEncoding.DecodeString(encodedMasterKey)
		if err != nil || len(key) != 32 {
			return nil, errors.New("PKI_MASTER_KEY must be base64 of 32 random bytes")
		}
		s.key = key
	}
	cas, err := s.loadCAs(ctx, "1 = 1")
	if err != nil {
		return nil, err
	}
	if len(cas) > 0 && !s.Ready() {
		return nil, errors.New("PKI_MASTER_KEY is required while CA records exist")
	}
	for _, ca := range cas {
		var err error
		if ca.Kind == KindSSH {
			_, err = s.sshSigner(ca)
		} else {
			_, _, err = s.x509Signer(ca)
		}
		if err != nil {
			return nil, fmt.Errorf("PKI_MASTER_KEY cannot open CA %d: %w", ca.ID, err)
		}
	}
	return s, nil
}

// Ready reports whether a master key is configured.
func (s *Service) Ready() bool { return len(s.key) == 32 }

func (s *Service) now() time.Time { return s.Now().UTC() }

const caColumns = `id, name, COALESCE(kind,'x509'), certificate_pem, COALESCE(private_key_ciphertext,''), COALESCE(crl_pem,''),
	COALESCE(crl_number,0), COALESCE(enabled,0), COALESCE(CAST(created_at AS TEXT),'')`

func (s *Service) loadCAs(ctx context.Context, where string, args ...any) ([]CA, error) {
	rows, err := s.DB.Query(ctx, "SELECT "+caColumns+" FROM certificate_authorities WHERE deleted_at IS NULL AND ("+where+") ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CA
	for rows.Next() {
		var ca CA
		var enabled any
		if err := rows.Scan(&ca.ID, &ca.Name, &ca.Kind, &ca.CertificatePEM, &ca.ciphertext, &ca.CRLPEM, &ca.CRLNumber, &enabled, &ca.CreatedAt); err != nil {
			return nil, err
		}
		ca.Enabled = truthy(enabled)
		out = append(out, ca)
	}
	return out, rows.Err()
}

// truthy accepts the encodings v1 left behind for the boolean column across SQLite and D1.
func truthy(v any) bool {
	switch x := v.(type) {
	case int64:
		return x != 0
	case float64:
		return x != 0
	case bool:
		return x
	case string:
		return x == "1" || strings.EqualFold(x, "true")
	case []byte:
		return string(x) == "1" || strings.EqualFold(string(x), "true")
	}
	return false
}

// CA returns one CA.
func (s *Service) CA(ctx context.Context, id int64) (CA, error) {
	cas, err := s.loadCAs(ctx, "id = ?", id)
	if err != nil {
		return CA{}, err
	}
	if len(cas) == 0 {
		return CA{}, apperr.ErrNotFound
	}
	return cas[0], nil
}

// ListCAs returns every CA.
func (s *Service) ListCAs(ctx context.Context) ([]CA, error) {
	cas, err := s.loadCAs(ctx, "1 = 1")
	if cas == nil {
		cas = []CA{}
	}
	return cas, err
}

func (s *Service) seal(aad, plain []byte) (string, error) {
	if !s.Ready() {
		return "", ErrNotConfigured
	}
	aead, err := s.aead()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	fp := sha256.Sum256(aad)
	return base64.StdEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, fp[:])), nil
}

func (s *Service) open(aad []byte, encoded string) ([]byte, error) {
	if !s.Ready() {
		return nil, ErrNotConfigured
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	aead, err := s.aead()
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize() {
		return nil, errors.New("encrypted CA key is truncated")
	}
	fp := sha256.Sum256(aad)
	return aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], fp[:])
}

func (s *Service) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (s *Service) x509Signer(ca CA) (*x509.Certificate, crypto.Signer, error) {
	if ca.Kind != KindX509 {
		return nil, nil, errors.New("CA is not an X.509 CA")
	}
	block, _ := pem.Decode([]byte(ca.CertificatePEM))
	if block == nil {
		return nil, nil, errors.New("stored CA certificate is invalid")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	plain, err := s.open(cert.Raw, ca.ciphertext)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParsePKCS8PrivateKey(plain)
	if err != nil {
		return nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("stored CA key cannot sign")
	}
	return cert, signer, nil
}

func randomSerial(bits uint) *big.Int {
	v, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), bits))
	if err != nil {
		panic(err)
	}
	if v.Sign() == 0 {
		v.SetInt64(1)
	}
	return v
}

type revocation struct {
	serial string
	at     time.Time
	reason int
}

var timeLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"}

func parseTime(v string) (time.Time, error) {
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised time %q", v)
}

func (s *Service) revocations(ctx context.Context, caID int64) ([]revocation, error) {
	rows, err := s.DB.Query(ctx, "SELECT serial_hex, CAST(revoked_at AS TEXT), COALESCE(reason,0) FROM revoked_certificates WHERE certificate_authority_id = ? AND deleted_at IS NULL ORDER BY id", caID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []revocation
	for rows.Next() {
		var r revocation
		var at string
		if err := rows.Scan(&r.serial, &at, &r.reason); err != nil {
			return nil, err
		}
		if r.at, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) signCRL(cert *x509.Certificate, signer crypto.Signer, number int64, revoked []revocation) (string, error) {
	entries := make([]x509.RevocationListEntry, 0, len(revoked))
	for _, r := range revoked {
		serial, ok := new(big.Int).SetString(r.serial, 16)
		if !ok {
			return "", fmt.Errorf("invalid revoked serial %q", r.serial)
		}
		entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: r.at, ReasonCode: r.reason})
	}
	now := s.now()
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: big.NewInt(number), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(crlLifetime), RevokedCertificateEntries: entries,
	}, cert, signer)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})), nil
}

// revokeStatements revokes serial under ca. Every statement is conditioned on the CA's current CRL number,
// and the final UPDATE reports whether the snapshot still held; extra statements run under the same guard.
func (s *Service) revokeStatements(ctx context.Context, ca CA, serial string, reason int, extra ...store.Statement) ([]store.Statement, error) {
	now := s.now()
	guard := " AND EXISTS (SELECT 1 FROM certificate_authorities WHERE id = ? AND crl_number = ?)"
	guardArgs := []any{ca.ID, ca.CRLNumber}
	stmts := []store.Statement{{
		Query: `INSERT INTO revoked_certificates (created_at, updated_at, certificate_authority_id, serial_hex, revoked_at, reason)
			SELECT ?, ?, ?, ?, ?, ? WHERE 1 = 1` + guard + ` ON CONFLICT(certificate_authority_id, serial_hex) DO NOTHING`,
		Args: append([]any{now, now, ca.ID, serial, now, reason}, guardArgs...),
	}}
	for _, e := range extra {
		stmts = append(stmts, store.Statement{Query: e.Query + guard, Args: append(append([]any{}, e.Args...), guardArgs...)})
	}
	crl := ca.CRLPEM
	if ca.Kind == KindX509 {
		cert, signer, err := s.x509Signer(ca)
		if err != nil {
			return nil, err
		}
		revoked, err := s.revocations(ctx, ca.ID)
		if err != nil {
			return nil, err
		}
		already := false
		for _, r := range revoked {
			already = already || r.serial == serial
		}
		if !already {
			revoked = append(revoked, revocation{serial: serial, at: now, reason: reason})
		}
		if crl, err = s.signCRL(cert, signer, ca.CRLNumber+1, revoked); err != nil {
			return nil, err
		}
	}
	return append(stmts, store.Statement{
		Query: "UPDATE certificate_authorities SET crl_pem = ?, crl_number = crl_number + 1, updated_at = ? WHERE id = ? AND crl_number = ?",
		Args:  []any{crl, now, ca.ID, ca.CRLNumber},
	}), nil
}

// commitGuarded runs a guarded batch; a zero-row final UPDATE means another writer advanced the CRL first.
func (s *Service) commitGuarded(ctx context.Context, stmts []store.Statement) error {
	res, err := s.DB.Batch(ctx, stmts)
	if err != nil {
		return err
	}
	if res[len(res)-1].RowsAffected != 1 {
		return ErrChanged
	}
	return nil
}

// retry re-reads state and retries an optimistic operation a few times before reporting a conflict.
func retry(f func() error) error {
	var err error
	for range 4 {
		if err = f(); !errors.Is(err, ErrChanged) {
			return err
		}
	}
	return err
}
