// Package password hashes credentials one-way. Plaintext is never stored or transmitted beyond the login request.
package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters follow the OWASP minimum and stay well inside the Worker memory limit.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16

	LDAPCryptRounds = 100000
	MinLength       = 10
	MaxLength       = 128
)

var (
	ErrPolicy    = fmt.Errorf("password must be %d-%d characters", MinLength, MaxLength)
	ErrMalformed = errors.New("malformed password hash")
)

// Credential is what gets persisted: App verifies logins, LDAP is written verbatim to userPassword.
type Credential struct {
	App  string
	LDAP string
}

// CheckPolicy enforces the length limits on a new password.
func CheckPolicy(pw string) error {
	if !utf8.ValidString(pw) {
		return ErrPolicy
	}
	if n := utf8.RuneCountInString(pw); n < MinLength || n > MaxLength {
		return ErrPolicy
	}
	return nil
}

var b64 = base64.RawStdEncoding

func randomSalt() []byte {
	s := make([]byte, saltLen)
	if _, err := rand.Read(s); err != nil {
		panic(err)
	}
	return s
}

func cryptSalt() string {
	raw := randomSalt()
	out := make([]byte, cryptMaxSalt)
	for i := range out {
		out[i] = cryptAlphabet[int(raw[i])%len(cryptAlphabet)]
	}
	return string(out)
}

// HashArgon2id returns a PHC-format argon2id hash with a random salt.
func HashArgon2id(pw string) string {
	salt := randomSalt()
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// Hash builds both hashes for a new password; it enforces the policy.
func Hash(pw string) (Credential, error) {
	if err := CheckPolicy(pw); err != nil {
		return Credential{}, err
	}
	return derive(pw)
}

// Derive builds both hashes without the length policy; only for converting existing legacy passwords.
func Derive(pw string) (Credential, error) {
	if pw == "" {
		return Credential{}, ErrPolicy
	}
	return derive(pw)
}

func derive(pw string) (Credential, error) {
	ldap, err := CryptSHA512([]byte(pw), cryptSalt(), LDAPCryptRounds)
	if err != nil {
		return Credential{}, err
	}
	return Credential{App: HashArgon2id(pw), LDAP: "{CRYPT}" + ldap}, nil
}

// Verify checks pw against an argon2id hash in constant time.
func Verify(hash, pw string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrMalformed
	}
	var version int
	var m uint32
	var t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrMalformed
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, ErrMalformed
	}
	// bound attacker-influenced cost if a hash row were ever tampered with
	if m == 0 || m > 256*1024 || t == 0 || t > 10 || p == 0 || p > 4 {
		return false, ErrMalformed
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, ErrMalformed
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, ErrMalformed
	}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// VerifyLDAPCrypt checks pw against a "{CRYPT}$6$..." value.
func VerifyLDAPCrypt(stored, pw string) bool {
	h, ok := strings.CutPrefix(stored, "{CRYPT}")
	if !ok {
		return false
	}
	salt, rounds, ok := parseCrypt(h)
	if !ok {
		return false
	}
	got, err := CryptSHA512([]byte(pw), salt, rounds)
	return err == nil && subtle.ConstantTimeCompare([]byte(got), []byte(h)) == 1
}

// dummyHash equalises timing for unknown users.
var dummyHash = HashArgon2id("timing-equaliser-not-a-password")

// VerifyDummy spends the same time as a real check, so unknown usernames are not revealed by timing.
func VerifyDummy(pw string) { _, _ = Verify(dummyHash, pw) }
