package slave

import (
	"os"
	"path/filepath"
	"testing"
)

// The slave.yml template must stay loadable: LoadConfig rejects unknown keys.
func TestDeployExamplesLoad(t *testing.T) {
	files, _ := filepath.Glob("../../deploy/compose/slave/slave.yml.example")
	if len(files) == 0 {
		t.Fatal("no slave examples found")
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "config.yml")
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if c.Limits.MaxUserDeletions != 5 || c.ClusterID == "" || c.MasterURL == "" || len(c.TrustDirs) != 1 {
			t.Fatalf("%s: unexpected %+v", f, c)
		}
	}
}

// master-url was called main-url; a configuration that still uses the old key keeps working.
func TestLegacyMainURL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yml")
	raw := "main-url: https://master.example.org\nstate-dir: " + t.TempDir() + "\ntrust-dirs: [" + t.TempDir() + "]\ncluster-id: test\nlocal-base-dn: dc=example,dc=org\ndns-name: _sync.example.org\n" +
		"tls:\n  cert-file: /dev/null\n  key-file: /dev/null\n  server-name: master.example.org\nldap:\n  url: ldap://127.0.0.1\n  bind-dn: cn=admin,dc=example,dc=org\n  password-file: /dev/null\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if c == nil || c.MasterURL != "https://master.example.org" {
		t.Fatalf("main-url not used: %+v, %v", c, err)
	}
}
