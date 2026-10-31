package pki

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"golang.org/x/crypto/ssh"
)

// MaxTemporarySSH bounds certificates signed for caller-held keys (web access sessions).
const MaxTemporarySSH = 5 * time.Minute

// TemporarySSH is a short-lived OpenSSH user certificate for a public key the caller keeps.
type TemporarySSH struct {
	Certificate string
	Serial      uint64
	ExpiresAt   time.Time
	CAID        int64
}

// SignTemporarySSH signs key for one principal with an enabled SSH CA. No certificate record is
// written: the lifetime is bounded by MaxTemporarySSH instead of revocation.
func (s *Service) SignTemporarySSH(ctx context.Context, caID int64, key ssh.PublicKey, principal string, keyID func(serial uint64) string, ttl time.Duration) (TemporarySSH, error) {
	if !s.Ready() {
		return TemporarySSH{}, errors.New("pki: master key not configured")
	}
	if ttl <= 0 || ttl > MaxTemporarySSH || key == nil || principal == "" {
		return TemporarySSH{}, errors.New("pki: invalid temporary certificate request")
	}
	ca, err := s.CA(ctx, caID)
	if err != nil {
		return TemporarySSH{}, err
	}
	if !ca.Enabled {
		return TemporarySSH{}, errors.New("pki: CA disabled")
	}
	signer, err := s.sshSigner(ca)
	if err != nil {
		return TemporarySSH{}, err
	}
	serial := randomSerial(63).Uint64()
	if serial == 0 {
		serial = 1
	}
	now := s.now().Truncate(time.Second)
	expires := now.Add(ttl)
	cert := &ssh.Certificate{Key: key, Serial: serial, CertType: ssh.UserCert, KeyId: keyID(serial), ValidPrincipals: []string{principal},
		ValidAfter: uint64(now.Unix()), ValidBefore: uint64(expires.Unix()),
		Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}, CriticalOptions: map[string]string{}}}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		return TemporarySSH{}, err
	}
	return TemporarySSH{Certificate: string(ssh.MarshalAuthorizedKey(cert)), Serial: serial, ExpiresAt: expires, CAID: ca.ID}, nil
}
