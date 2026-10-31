package pki

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
)

func validCAName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "\x00\r\n") {
		return "", apperr.BadRequest("CA name must be 1-100 characters")
	}
	return name, nil
}

func (s *Service) insertCA(ctx context.Context, name, kind, public, ciphertext, crl string, crlNumber int64) (CA, error) {
	now := s.now()
	_, err := s.DB.Exec(ctx, `INSERT INTO certificate_authorities (created_at, updated_at, name, kind, certificate_pem, private_key_ciphertext, crl_pem, crl_number, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`, now, now, name, kind, public, ciphertext, crl, crlNumber)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return CA{}, apperr.New(409, "conflict", "a CA with this name already exists")
		}
		return CA{}, err
	}
	cas, err := s.loadCAs(ctx, "name = ?", name)
	if err != nil || len(cas) == 0 {
		return CA{}, errors.Join(err, errors.New("created CA not found"))
	}
	return cas[0], nil
}

// GenerateX509 creates a 10-year P-256 CA and returns the private key once for offline backup.
func (s *Service) GenerateX509(ctx context.Context, name string) (CA, []byte, error) {
	if !s.Ready() {
		return CA{}, nil, ErrNotConfigured
	}
	name, err := validCAName(name)
	if err != nil {
		return CA{}, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CA{}, nil, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return CA{}, nil, err
	}
	skid := sha256.Sum256(pub)
	now := s.now()
	tmpl := &x509.Certificate{SerialNumber: randomSerial(128), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Minute),
		NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, SubjectKeyId: skid[:20]}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return CA{}, nil, err
	}
	priv, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return CA{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: priv})
	ca, err := s.ImportX509(ctx, name, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM)
	return ca, keyPEM, err
}

// ImportX509 stores an existing X.509 CA; its private key is sealed with the master key.
func (s *Service) ImportX509(ctx context.Context, name string, certPEM, keyPEM []byte) (CA, error) {
	if !s.Ready() {
		return CA{}, ErrNotConfigured
	}
	name, err := validCAName(name)
	if err != nil {
		return CA{}, err
	}
	if len(certPEM) > 64<<10 || len(keyPEM) > 64<<10 {
		return CA{}, apperr.BadRequest("PEM input too large")
	}
	cert, signer, privDER, err := s.parsePair(certPEM, keyPEM)
	if err != nil {
		return CA{}, apperr.BadRequest(err.Error())
	}
	crl, err := s.signCRL(cert, signer, 1, nil)
	if err != nil {
		return CA{}, err
	}
	ct, err := s.seal(cert.Raw, privDER)
	if err != nil {
		return CA{}, err
	}
	return s.insertCA(ctx, name, KindX509, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})), ct, crl, 1)
}

func (s *Service) parsePair(certPEM, keyPEM []byte) (*x509.Certificate, crypto.Signer, []byte, error) {
	cb, rest := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, nil, errors.New("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, nil, err
	}
	now := s.now()
	if !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.KeyUsage&x509.KeyUsageCRLSign == 0 || now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return nil, nil, nil, errors.New("certificate is not a currently valid signing CA")
	}
	kb, rest := pem.Decode(keyPEM)
	if kb == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, nil, errors.New("invalid CA private key PEM")
	}
	var key any
	switch kb.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(kb.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(kb.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(kb.Bytes)
	default:
		return nil, nil, nil, errors.New("unsupported CA private key PEM")
	}
	if err != nil {
		return nil, nil, nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, nil, nil, errors.New("CA private key cannot sign")
	}
	a, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, nil, nil, err
	}
	b, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil || subtle.ConstantTimeCompare(a, b) != 1 {
		return nil, nil, nil, errors.New("CA certificate and private key do not match")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	return cert, signer, der, err
}

// GenerateSSH creates a new Ed25519 SSH CA and returns its public key.
func (s *Service) GenerateSSH(ctx context.Context, name string) (CA, []byte, error) {
	if !s.Ready() {
		return CA{}, nil, ErrNotConfigured
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return CA{}, nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, name)
	if err != nil {
		return CA{}, nil, err
	}
	priv := pem.EncodeToMemory(block)
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		return CA{}, nil, err
	}
	ca, err := s.ImportSSH(ctx, name, ssh.MarshalAuthorizedKey(pub), priv)
	return ca, priv, err
}

// ImportSSH stores an existing SSH CA key pair; the private key is sealed with the master key.
func (s *Service) ImportSSH(ctx context.Context, name string, publicKey, privateKey []byte) (CA, error) {
	if !s.Ready() {
		return CA{}, ErrNotConfigured
	}
	name, err := validCAName(name)
	if err != nil {
		return CA{}, err
	}
	if len(publicKey) == 0 || len(publicKey) > 64<<10 || len(privateKey) == 0 || len(privateKey) > 64<<10 {
		return CA{}, apperr.BadRequest("invalid SSH CA key size")
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey(publicKey)
	if err != nil || len(bytes.TrimSpace(rest)) != 0 || pub.Type() != ssh.KeyAlgoED25519 {
		return CA{}, apperr.BadRequest("SSH CA public key must be one Ed25519 authorized key")
	}
	signer, err := ssh.ParsePrivateKey(privateKey)
	if err != nil || !bytes.Equal(pub.Marshal(), signer.PublicKey().Marshal()) {
		return CA{}, apperr.BadRequest("SSH CA public key and private key do not match")
	}
	ct, err := s.seal(pub.Marshal(), privateKey)
	if err != nil {
		return CA{}, err
	}
	return s.insertCA(ctx, name, KindSSH, string(ssh.MarshalAuthorizedKey(pub)), ct, "", 0)
}

func (s *Service) sshSigner(ca CA) (ssh.Signer, error) {
	if ca.Kind != KindSSH {
		return nil, errors.New("CA is not an OpenSSH user CA")
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(ca.CertificatePEM))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 || pub.Type() != ssh.KeyAlgoED25519 {
		return nil, errors.New("stored SSH CA public key is invalid")
	}
	priv, err := s.open(pub.Marshal(), ca.ciphertext)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil || !bytes.Equal(pub.Marshal(), signer.PublicKey().Marshal()) {
		return nil, errors.New("stored SSH CA private key does not match")
	}
	return signer, nil
}

// RenewExpiring re-signs X.509 CRLs that expire within 12 hours.
func (s *Service) RenewExpiring(ctx context.Context) error {
	if !s.Ready() {
		return nil
	}
	cas, err := s.loadCAs(ctx, "kind = ? AND (enabled = 1 OR enabled = 'true')", KindX509)
	if err != nil {
		return err
	}
	var errs []error
	for _, ca := range cas {
		if b, _ := pem.Decode([]byte(ca.CRLPEM)); b != nil {
			if crl, err := x509.ParseRevocationList(b.Bytes); err == nil && crl.NextUpdate.After(s.now().Add(12*time.Hour)) {
				continue
			}
		}
		cert, signer, err := s.x509Signer(ca)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		revoked, err := s.revocations(ctx, ca.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		crl, err := s.signCRL(cert, signer, ca.CRLNumber+1, revoked)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		// a concurrent revocation already produced a fresher CRL when this matches nothing
		if _, err := s.DB.Exec(ctx, "UPDATE certificate_authorities SET crl_pem = ?, crl_number = crl_number + 1, updated_at = ? WHERE id = ? AND crl_number = ?",
			crl, s.now(), ca.ID, ca.CRLNumber); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
