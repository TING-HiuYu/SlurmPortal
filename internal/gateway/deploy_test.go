package gateway

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// loadExample loads deploy/gateway/gateway.yaml.example with its placeholders filled, after edit.
func loadExample(t *testing.T, edit func(string) string) Config {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/gateway/gateway.yaml.example")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	pub, _, _ := ed25519.GenerateKey(nil)
	key, _ := ssh.NewPublicKey(pub)
	bundle := filepath.Join(dir, "trusted-user-ca-keys.pub")
	if err := os.WriteFile(bundle, ssh.MarshalAuthorizedKey(key), 0o644); err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(raw), "/etc/go-ldap-admin-trust/current/ssh/trusted-user-ca-keys.pub", bundle, 1)
	s = strings.Replace(s, "REPLACE_WITH_ED25519_TICKET_PUBLIC_KEY", base64.RawURLEncoding.EncodeToString(pub), 1)
	p := filepath.Join(dir, "gateway.yaml")
	if err := os.WriteFile(p, []byte(edit(s)), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The example must stay loadable once its placeholders are filled in.
func TestDeployExampleLoads(t *testing.T) {
	c := loadExample(t, func(s string) string { return s })
	if _, ok := c.parsedKeys["web-access-1"]; !ok || len(c.parsedKeys) != 1 || c.MasterOrigin != "https://sync.portal.example.org" {
		t.Fatalf("unexpected keys or origin: %v %q", c.parsedKeys, c.MasterOrigin)
	}
}

// The master_* keys were called main_*; configurations that still use those names keep working.
func TestLegacyMainKeys(t *testing.T) {
	c := loadExample(t, func(s string) string { return strings.ReplaceAll(s, "\nmaster_", "\nmain_") })
	if c.MasterOrigin != "https://sync.portal.example.org" || !strings.HasSuffix(c.MasterClientKey, "master-client.key.pem") {
		t.Fatalf("legacy keys not used: %q %q", c.MasterOrigin, c.MasterClientKey)
	}
}
