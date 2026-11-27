package config

import (
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestUnknownFieldsRejected(t *testing.T) {
	c := defaults()
	if err := Parse([]byte("role: master\nbogus: 1\n"), &c); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestAuditCanOnlyBeEnabled(t *testing.T) {
	c := defaults()
	c.Audit.Enabled = true
	if err := applyEnv(&c, env(map[string]string{"AUDIT_ENABLED": "false"})); err != nil || !c.Audit.Enabled {
		t.Fatal("env disabled audit enabled in config")
	}
	c = defaults()
	if err := applyEnv(&c, env(map[string]string{"AUDIT_ENABLED": "true"})); err != nil || !c.Audit.Enabled {
		t.Fatal("env could not enable audit")
	}
	if err := applyEnv(&c, env(map[string]string{"AUDIT_ENABLED": "maybe"})); err == nil {
		t.Fatal("invalid bool accepted")
	}
}

func TestJWTKeysFromEnv(t *testing.T) {
	c := defaults()
	if err := applyEnv(&c, env(map[string]string{"JWT_KEYS": "a:MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=", "JWT_ACTIVE_KID": "a"})); err != nil {
		t.Fatal(err)
	}
	if len(c.JWTKeys.Keys["a"]) != 32 || c.JWTKeys.ActiveKID != "a" {
		t.Fatalf("keys = %+v", c.JWTKeys)
	}
	if err := applyEnv(&c, env(map[string]string{"JWT_KEYS": "nokid"})); err == nil {
		t.Fatal("malformed key accepted")
	}
}

func TestValidate(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"role":           func(c *Config) { c.Role = "root" },
		"cors any":       func(c *Config) { c.HTTP.CORSOrigins = []string{"*"} },
		"cors http":      func(c *Config) { c.HTTP.CORSOrigins = []string{"http://evil.example"} },
		"ttl":            func(c *Config) { c.Auth.TokenTTL = 0 },
		"ssh ttl zero":   func(c *Config) { c.PKI.SSHUserTTL = 0 },
		"ssh ttl neg":    func(c *Config) { c.PKI.SSHUserTTL = -time.Hour },
		"ssh ttl > 10y":  func(c *Config) { c.PKI.SSHUserTTL = 87601 * time.Hour },
		"x509 ttl zero":  func(c *Config) { c.PKI.X509UserTTL = 0 },
		"x509 ttl neg":   func(c *Config) { c.PKI.X509UserTTL = -time.Hour },
		"x509 ttl > 10y": func(c *Config) { c.PKI.X509UserTTL = 3651 * 24 * time.Hour },
	} {
		c := defaults()
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: invalid config accepted", name)
		}
	}
}

func TestPluginDecoderStrict(t *testing.T) {
	c := defaults()
	if err := Parse([]byte("plugins:\n  enabled: [x]\n  config:\n    x:\n      endpoint: https://otel\n      extra: 1\n"), &c); err != nil {
		t.Fatal(err)
	}
	var dst struct {
		Endpoint string `yaml:"endpoint"`
	}
	if err := c.PluginDecoder("x")(&dst); err == nil {
		t.Fatal("unknown plugin field accepted")
	}
	if err := c.PluginDecoder("absent")(&dst); err != nil {
		t.Fatal(err)
	}
}

// The master role was called "main"; configurations that still say so keep working.
func TestLegacyMainRole(t *testing.T) {
	c := defaults()
	c.Role = "main"
	if err := c.Validate(); err != nil || c.Role != plugin.RoleMaster {
		t.Fatalf("role main: err %v, role %q", err, c.Role)
	}
}

func TestPKILifetimesFromConfigAndEnv(t *testing.T) {
	c := defaults()
	if c.PKI.SSHUserTTL != 30*24*time.Hour || c.PKI.X509UserTTL != 365*24*time.Hour {
		t.Fatalf("defaults = %+v", c.PKI)
	}

	if err := Parse([]byte("pki:\n  ssh-user-ttl: 8760h\n  x509-user-ttl: 4380h\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.PKI.SSHUserTTL != 365*24*time.Hour || c.PKI.X509UserTTL != 4380*time.Hour {
		t.Fatalf("parsed yaml = %+v", c.PKI)
	}

	c = defaults()
	if err := applyEnv(&c, env(map[string]string{
		"PKI_SSH_USER_TTL":  "365d",
		"PKI_X509_USER_TTL": "48h",
	})); err != nil {
		t.Fatal(err)
	}
	if c.PKI.SSHUserTTL != 365*24*time.Hour || c.PKI.X509UserTTL != 48*time.Hour {
		t.Fatalf("env = %+v", c.PKI)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid env failed validation: %v", err)
	}

	c = defaults()
	if err := applyEnv(&c, env(map[string]string{
		"PKI_SSH_USER_TTL":  "8760h",
		"PKI_X509_USER_TTL": "90d",
	})); err != nil {
		t.Fatal(err)
	}
	if c.PKI.SSHUserTTL != 8760*time.Hour || c.PKI.X509UserTTL != 90*24*time.Hour {
		t.Fatalf("env = %+v", c.PKI)
	}

	for _, bad := range []string{"bogus", "1h2d", "999999999999d", "-999999999999d"} {
		c = defaults()
		if err := applyEnv(&c, env(map[string]string{"PKI_SSH_USER_TTL": bad})); err == nil {
			t.Errorf("invalid PKI_SSH_USER_TTL %q accepted", bad)
		}
	}
	for _, bad := range []string{"0s", "-1h", "0d", "87601h", "3651d"} {
		c = defaults()
		if err := applyEnv(&c, env(map[string]string{"PKI_SSH_USER_TTL": bad})); err != nil {
			t.Fatalf("applyEnv(%q): %v", bad, err)
		}
		if err := c.Validate(); err == nil {
			t.Errorf("out-of-range PKI_SSH_USER_TTL %q passed Validate", bad)
		}
	}
}
