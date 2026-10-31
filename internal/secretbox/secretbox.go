// Package secretbox encrypts small configuration secrets at rest with keys derived from the master key.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

const prefix = "sb1:"

var ErrNoKey = errors.New("secret storage needs PKI_MASTER_KEY")

// Box seals and opens secrets with keys derived from PKI_MASTER_KEY.
type Box struct{ master []byte }

// New decodes a base64 32-byte master key. An empty key gives a Box that cannot seal.
func New(encodedMaster string) (*Box, error) {
	if encodedMaster == "" {
		return &Box{}, nil
	}
	k, err := base64.StdEncoding.DecodeString(encodedMaster)
	if err != nil || len(k) != 32 {
		return nil, errors.New("master key must be base64 of 32 bytes")
	}
	return &Box{master: k}, nil
}

func (b *Box) aead(purpose string) (cipher.AEAD, error) {
	if len(b.master) == 0 {
		return nil, ErrNoKey
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, b.master, nil, []byte("go-ldap-admin/"+purpose)), key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal binds the ciphertext to purpose so it cannot be replayed into another field.
func (b *Box) Seal(purpose string, plain []byte) (string, error) {
	a, err := b.aead(purpose)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return prefix + base64.RawStdEncoding.EncodeToString(a.Seal(nonce, nonce, plain, []byte(purpose))), nil
}

// Open decrypts a value sealed for the same purpose.
func (b *Box) Open(purpose, sealed string) ([]byte, error) {
	raw, ok := strings.CutPrefix(sealed, prefix)
	if !ok {
		return nil, errors.New("not a sealed value")
	}
	data, err := base64.RawStdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	a, err := b.aead(purpose)
	if err != nil {
		return nil, err
	}
	if len(data) < a.NonceSize() {
		return nil, errors.New("sealed value truncated")
	}
	return a.Open(nil, data[:a.NonceSize()], data[a.NonceSize():], []byte(purpose))
}

// IsSealed reports whether v is a sealed value rather than plaintext.
func IsSealed(v string) bool { return strings.HasPrefix(v, prefix) }
