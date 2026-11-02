// Package config loads YAML configuration with environment overrides for secrets and safety switches.
package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
)

// Config is the whole process configuration: YAML first, then environment overrides for secrets.
type Config struct {
	Role      plugin.Role `yaml:"role"`
	HTTP      HTTP        `yaml:"http"`
	Auth      Auth        `yaml:"auth"`
	DB        DB          `yaml:"db"`
	Directory Directory   `yaml:"directory"`
	Sync      Sync        `yaml:"sync"`
	// Audit can only be turned on (config or AUDIT_ENABLED); nothing turns it off at runtime.
	Audit   Audit   `yaml:"audit"`
	Plugins Plugins `yaml:"plugins"`
	// Clusters are the slave sites allowed to report status; each fingerprint must also be a sync peer.
	Clusters plugin.Clusters `yaml:"clusters"`
	// Telemetry.SampleRatio is the probability a new root trace is recorded by exporter plugins.
	Telemetry struct {
		SampleRatio float64 `yaml:"sample-ratio"`
	} `yaml:"telemetry"`

	JWTKeys      authn.Keyring `yaml:"-"`
	PKIMasterKey string        `yaml:"-"`
	// BootstrapAdmin creates the first super administrator only while none exists.
	BootstrapAdmin struct{ Username, Password string } `yaml:"-"`
	// Getenv resolves "env:NAME" secret references in plugin configuration.
	Getenv func(string) string `yaml:"-"`
}

// HTTP configures the listener, CORS, client-IP detection and the per-IP rate limit.
type HTTP struct {
	Listen          string   `yaml:"listen"`
	CORSOrigins     []string `yaml:"cors-origins"`
	TrustedIPHeader string   `yaml:"trusted-ip-header"`
	RatePerSecond   float64  `yaml:"rate-per-second"`
	RateBurst       int      `yaml:"rate-burst"`
	// StaticDir, when set, makes cmd/server serve the built web app (web/dist) from this directory.
	StaticDir string `yaml:"static-dir"`
	// BehindTLSProxy tells cmd/server that its listener is reachable only through a reverse proxy
	// that terminates HTTPS and sets X-Forwarded-Proto. Never enable it on a port clients reach directly.
	BehindTLSProxy bool `yaml:"behind-tls-proxy"`
}

// Auth configures session tokens.
type Auth struct {
	Issuer   string        `yaml:"issuer"`
	Audience string        `yaml:"audience"`
	TokenTTL time.Duration `yaml:"token-ttl"`
}

// DB is the native database: a database/sql driver name (cmd/server links "sqlite") and its DSN.
type DB struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

// Audit configures the hash-chained audit log.
type Audit struct {
	Enabled bool `yaml:"enabled"`
	// MaxRecords bounds the audit log (0 = unbounded); past it, each new record replaces the oldest.
	MaxRecords int64 `yaml:"max-records"`
}

// Sync configures how master serves snapshots to slaves and announces their hash over DNS.
type Sync struct {
	Listen           string   `yaml:"listen"`
	CertFile         string   `yaml:"cert-file"`
	KeyFile          string   `yaml:"key-file"`
	ClientCAFile     string   `yaml:"client-ca-file"`
	PeerFingerprints []string `yaml:"peer-fingerprints"`
	DNSName          string   `yaml:"dns-name"`
	ZoneID           string   `yaml:"cloudflare-zone-id"`
	RecordID         string   `yaml:"cloudflare-record-id"`
	TTL              int      `yaml:"ttl"`

	CloudflareToken string `yaml:"-"`
}

// Directory holds the identity layout master uses to derive DNs. Account home and shell presets are
// set per POSIX group; DefaultShell is the fallback when a main group has no shell preset.
type Directory struct {
	BaseDN       string `yaml:"base-dn"`
	UserDN       string `yaml:"user-dn"`
	DefaultShell string `yaml:"default-shell"`
}

type Plugins struct {
	Enabled []string             `yaml:"enabled"`
	Config  map[string]yaml.Node `yaml:"config"`
}

func defaults() Config {
	c := Config{
		Role:      plugin.RoleStandalone,
		HTTP:      HTTP{Listen: "127.0.0.1:8888", RatePerSecond: 20, RateBurst: 200},
		Auth:      Auth{Issuer: "slurm-portal", Audience: "slurm-portal-api", TokenTTL: 12 * time.Hour},
		DB:        DB{Driver: "sqlite", DSN: "file:data/slurm-portal.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"},
		Directory: Directory{DefaultShell: "/bin/bash"},
	}
	c.Telemetry.SampleRatio = 1
	return c
}

// Load reads the YAML file at path (optional), applies environment overrides and validates.
func Load(path string) (*Config, error) {
	c := defaults()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if err := Parse(raw, &c); err != nil {
			return nil, err
		}
	}
	if err := applyEnv(&c, os.Getenv); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// FromEnv builds a configuration purely from environment bindings (the Worker has no filesystem).
func FromEnv(getenv func(string) string) (*Config, error) {
	c := defaults()
	c.Role = plugin.RoleMaster
	if err := applyEnv(&c, getenv); err != nil {
		return nil, err
	}
	return &c, c.Validate()
}

// Parse decodes YAML into c, rejecting unknown keys.
func Parse(raw []byte, c *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	return nil
}

func applyEnv(c *Config, getenv func(string) string) error {
	for env, dst := range map[string]*string{
		"AUTH_ISSUER": &c.Auth.Issuer, "AUTH_AUDIENCE": &c.Auth.Audience, "TRUSTED_IP_HEADER": &c.HTTP.TrustedIPHeader,
		"LDAP_BASE_DN": &c.Directory.BaseDN, "LDAP_USER_DN": &c.Directory.UserDN, "SYNC_DNS_NAME": &c.Sync.DNSName,
		"CLOUDFLARE_ZONE_ID": &c.Sync.ZoneID, "CLOUDFLARE_RECORD_ID": &c.Sync.RecordID,
		"STATIC_DIR": &c.HTTP.StaticDir, "SYNC_LISTEN": &c.Sync.Listen, "SYNC_CERT_FILE": &c.Sync.CertFile,
		"SYNC_KEY_FILE": &c.Sync.KeyFile, "SYNC_CLIENT_CA_FILE": &c.Sync.ClientCAFile,
	} {
		if v := getenv(env); v != "" {
			*dst = v
		}
	}
	for env, dst := range map[string]*[]string{
		"CORS_ORIGINS": &c.HTTP.CORSOrigins, "SYNC_CLIENT_FINGERPRINTS": &c.Sync.PeerFingerprints, "PLUGINS_ENABLED": &c.Plugins.Enabled,
	} {
		if v := getenv(env); v != "" {
			*dst = nil
			for _, x := range strings.Split(v, ",") {
				if x = strings.TrimSpace(x); x != "" {
					*dst = append(*dst, strings.ToLower(x))
				}
			}
		}
	}
	if v := getenv("TELEMETRY_SAMPLE_RATIO"); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || r < 0 || r > 1 {
			return errors.New("TELEMETRY_SAMPLE_RATIO must be within [0, 1]")
		}
		c.Telemetry.SampleRatio = r
	}
	if v := getenv("SYSTEM_ROLE"); v != "" {
		c.Role = plugin.Role(v)
	}
	if v := getenv("HTTP_LISTEN"); v != "" {
		c.HTTP.Listen = v
	}
	if v := getenv("DB_DSN"); v != "" {
		c.DB.DSN = v
	}
	if v := getenv("AUDIT_ENABLED"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("AUDIT_ENABLED: %w", err)
		}
		c.Audit.Enabled = c.Audit.Enabled || on
	}
	if v := getenv("BEHIND_TLS_PROXY"); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("BEHIND_TLS_PROXY: %w", err)
		}
		c.HTTP.BehindTLSProxy = on
	}
	if v := getenv("AUDIT_MAX_RECORDS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("AUDIT_MAX_RECORDS must be a non-negative integer")
		}
		c.Audit.MaxRecords = n
	}
	keys, err := parseKeys(getenv("JWT_KEYS"))
	if err != nil {
		return err
	}
	c.JWTKeys = authn.Keyring{ActiveKID: getenv("JWT_ACTIVE_KID"), Keys: keys}
	c.PKIMasterKey = getenv("PKI_MASTER_KEY")
	c.Sync.CloudflareToken = getenv("CLOUDFLARE_API_TOKEN")
	c.BootstrapAdmin.Username, c.BootstrapAdmin.Password = getenv("BOOTSTRAP_ADMIN_USERNAME"), getenv("BOOTSTRAP_ADMIN_PASSWORD")
	if c.BootstrapAdmin.Username == "" {
		c.BootstrapAdmin.Username = "admin"
	}
	// PLUGINS_CONFIG is a JSON/YAML map of plugin id to its section; it replaces same-id file sections.
	if v := getenv("PLUGINS_CONFIG"); v != "" {
		var m map[string]yaml.Node
		if err := yaml.Unmarshal([]byte(v), &m); err != nil {
			return fmt.Errorf("PLUGINS_CONFIG: %w", err)
		}
		if c.Plugins.Config == nil {
			c.Plugins.Config = map[string]yaml.Node{}
		}
		for id, n := range m {
			c.Plugins.Config[strings.ToLower(id)] = n
		}
	}
	c.Getenv = getenv
	if v := getenv("CLUSTERS"); v != "" {
		c.Clusters = nil
		if err := yaml.Unmarshal([]byte(v), &c.Clusters); err != nil {
			return fmt.Errorf("CLUSTERS: %w", err)
		}
	}
	return nil
}

// parseKeys reads "kid:base64,kid2:base64"; keys never come from the YAML file.
func parseKeys(v string) (map[string][]byte, error) {
	keys := map[string][]byte{}
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		kid, b64, ok := strings.Cut(part, ":")
		if !ok || kid == "" {
			return nil, errors.New("JWT_KEYS entries must be kid:base64")
		}
		key, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("JWT_KEYS %q: %w", kid, err)
		}
		keys[kid] = key
	}
	return keys, nil
}

// Validate checks the role and every section needed by it.
func (c *Config) Validate() error {
	if c.Role == "main" { // the role's former name
		c.Role = plugin.RoleMaster
	}
	switch c.Role {
	case plugin.RoleStandalone, plugin.RoleMaster, plugin.RoleSlave, plugin.RoleGateway:
	default:
		return fmt.Errorf("invalid role %q", c.Role)
	}
	if c.HTTP.RatePerSecond <= 0 || c.HTTP.RateBurst <= 0 {
		return errors.New("http rate limit must be positive")
	}
	if c.Auth.TokenTTL <= 0 || c.Auth.TokenTTL > 7*24*time.Hour {
		return errors.New("auth.token-ttl must be within (0, 168h]")
	}
	if err := c.Clusters.Validate(); err != nil {
		return err
	}
	for _, cl := range c.Clusters {
		if !slices.Contains(c.Sync.PeerFingerprints, cl.Fingerprint) {
			return fmt.Errorf("cluster %s fingerprint is not an allowed sync peer", cl.ID)
		}
	}
	for _, o := range c.HTTP.CORSOrigins {
		if o == "*" || !strings.HasPrefix(o, "https://") && !strings.HasPrefix(o, "http://localhost") && !strings.HasPrefix(o, "http://127.0.0.1") {
			return fmt.Errorf("cors origin %q must be an exact https origin", o)
		}
	}
	return nil
}

// PluginDecoder returns a strict decoder for one plugin's section; a missing section decodes nothing.
func (c *Config) PluginDecoder(id string) func(dst any) error {
	return func(dst any) error {
		node, ok := c.Plugins.Config[id]
		if !ok {
			return nil
		}
		raw, err := yaml.Marshal(&node)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("plugin %s config: %w", id, err)
		}
		return nil
	}
}
