package gateway

import (
	"bufio"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// An explicitly configured file is authoritative. Its current symlink target
// is opened once so an atomic slave trust-generation flip is safe. These are
// public keys only; no CA private key is required or copied.
func (c Config) trustedUserCAs() ([]ssh.PublicKey, error) {
	values := c.UserCAKeys
	if c.UserCAKeysFile != "" {
		if !filepath.IsAbs(c.UserCAKeysFile) {
			return nil, errors.New("CA bundle path must be absolute")
		}
		f, e := os.Open(c.UserCAKeysFile)
		if e != nil {
			return nil, errors.New("user CA bundle unavailable")
		}
		defer func() { _ = f.Close() }()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() < 1 || info.Size() > 1<<20 {
			return nil, errors.New("unsafe or oversized user CA bundle")
		}
		values = nil
		scanner := bufio.NewScanner(io.LimitReader(f, (1<<20)+1))
		scanner.Buffer(make([]byte, 4096), 32768)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			values = append(values, line)
			if len(values) > 32 {
				return nil, errors.New("too many user CA keys")
			}
		}
		if scanner.Err() != nil {
			return nil, errors.New("invalid user CA bundle")
		}
	}
	if len(values) == 0 || len(values) > 32 {
		return nil, errors.New("user CA bundle or legacy pins required")
	}
	keys := make([]ssh.PublicKey, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		key, e := publicKey(value)
		if e != nil {
			return nil, e
		}
		fingerprint := string(key.Marshal())
		if !seen[fingerprint] {
			keys = append(keys, key)
			seen[fingerprint] = true
		}
	}
	return keys, nil
}
