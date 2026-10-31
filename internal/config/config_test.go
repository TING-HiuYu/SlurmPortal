package config

import (
	"testing"

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
		"role":      func(c *Config) { c.Role = "root" },
		"cors any":  func(c *Config) { c.HTTP.CORSOrigins = []string{"*"} },
		"cors http": func(c *Config) { c.HTTP.CORSOrigins = []string{"http://evil.example"} },
		"ttl":       func(c *Config) { c.Auth.TokenTTL = 0 },
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
