package eventbus

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// LocalTokens are generated locally and never sourced from master policy.
// Callers must not log or include this value in manifest/config responses.
type LocalTokens struct {
	ReadOnly  string `json:"read_only"`
	ReadWrite string `json:"read_write"`
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func NewLocalTokens() (LocalTokens, error) {
	ro, err := randomToken()
	if err != nil {
		return LocalTokens{}, err
	}
	rw, err := randomToken()
	if err != nil {
		return LocalTokens{}, err
	}
	return LocalTokens{ReadOnly: ro, ReadWrite: rw}, nil
}

func (t LocalTokens) Validate() error {
	for _, token := range []string{t.ReadOnly, t.ReadWrite} {
		raw, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(raw) != 32 {
			return errors.New("invalid local event credentials")
		}
	}
	if t.ReadOnly == t.ReadWrite {
		return errors.New("event credential scopes must differ")
	}
	return nil
}

func tokenEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (t LocalTokens) CanRead(token string) bool {
	return token != "" && (tokenEqual(token, t.ReadOnly) || tokenEqual(token, t.ReadWrite))
}
func (t LocalTokens) CanWrite(token string) bool {
	return token != "" && tokenEqual(token, t.ReadWrite)
}

// LoadOrCreateLocalTokens atomically publishes a fully written 0600 credential
// file. The containing directory must already exist. Event data is never stored.
func LoadOrCreateLocalTokens(path string) (LocalTokens, error) {
	if path == "" {
		return LocalTokens{}, errors.New("local event credential path is required")
	}
	read := func() (LocalTokens, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return LocalTokens{}, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
			return LocalTokens{}, errors.New("local event credentials require a private regular file")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return LocalTokens{}, err
		}
		var tokens LocalTokens
		if json.Unmarshal(body, &tokens) != nil {
			return LocalTokens{}, errors.New("invalid local event credential file")
		}
		return tokens, tokens.Validate()
	}
	if tokens, err := read(); err == nil {
		return tokens, nil
	} else if !os.IsNotExist(err) {
		return LocalTokens{}, err
	}
	tokens, err := NewLocalTokens()
	if err != nil {
		return LocalTokens{}, err
	}
	body, err := json.Marshal(tokens)
	if err != nil {
		return LocalTokens{}, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".event-credentials-*")
	if err != nil {
		return LocalTokens{}, err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err = tmp.Write(append(body, '\n')); err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return LocalTokens{}, err
	}
	if err = os.Link(name, path); err != nil {
		if os.IsExist(err) {
			return read()
		}
		return LocalTokens{}, err
	}
	return tokens, nil
}
