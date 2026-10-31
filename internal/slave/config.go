package slave

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/clusterstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcestatus"
)

// Config is the slave's YAML configuration. The file must be private (mode 0600 or stricter).
type Config struct {
	MasterURL string `yaml:"master-url"`
	// LegacyMainURL is master-url under its former name, main-url; it is used when master-url is empty.
	LegacyMainURL string        `yaml:"main-url"`
	DNSName       string        `yaml:"dns-name"`
	DNSServer     string        `yaml:"dns-server"`
	CheckInterval time.Duration `yaml:"check-interval"`
	CheckTimeout  time.Duration `yaml:"check-timeout"`
	DNSTimeout    time.Duration `yaml:"dns-timeout"`
	HealthListen  string        `yaml:"health-listen"`
	StateDir      string        `yaml:"state-dir"`
	TrustDirs     []string      `yaml:"trust-dirs"`
	TriggerFile   string        `yaml:"trigger-file"`
	SourceBaseDN  string        `yaml:"source-base-dn"`
	LocalBaseDN   string        `yaml:"local-base-dn"`
	LocalGroupDN  string        `yaml:"local-group-dn"`
	// ClusterID must match the id master binds to this slave's certificate.
	ClusterID string `yaml:"cluster-id"`
	// LifecycleFile enables the Slurm job lifecycle runtime (job e-mail, GPU resource summaries).
	LifecycleFile string `yaml:"lifecycle-file"`
	ClusterStatus struct {
		clusterstatus.Config `yaml:",inline"`
		Interval             time.Duration `yaml:"interval"`
	} `yaml:"cluster-status"`
	ResourceStatus struct {
		resourcestatus.Config `yaml:",inline"`
		Interval              time.Duration `yaml:"interval"`
	} `yaml:"resource-status"`
	// CurrentStatus publishes both status sections as one R2 object through master; it reuses the
	// command settings of cluster-status and resource-status, which need not be enabled themselves.
	CurrentStatus struct {
		Enabled  bool          `yaml:"enabled"`
		Interval time.Duration `yaml:"interval"`
	} `yaml:"current-status"`
	// Limits bound one sync (default: at most 5 user deletions, never drop a trusted SSH CA).
	Limits Limits `yaml:"limits"`
	TLS    struct {
		CertFile   string `yaml:"cert-file"`
		KeyFile    string `yaml:"key-file"`
		CAFile     string `yaml:"ca-file"`
		ServerName string `yaml:"server-name"`
	} `yaml:"tls"`
	LDAP struct {
		URL          string `yaml:"url"`
		BindDN       string `yaml:"bind-dn"`
		PasswordFile string `yaml:"password-file"`
		StartTLS     bool   `yaml:"start-tls"`
		ServerName   string `yaml:"server-name"`
	} `yaml:"ldap"`
}

// privateFile reads a secret file that must not be readable by group or others.
func privateFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s must be a regular file with mode 0600 or stricter", path)
	}
	return os.ReadFile(path)
}

// LoadConfig reads and validates the slave configuration.
func LoadConfig(path string) (*Config, error) {
	raw, err := privateFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{CheckInterval: 60 * time.Second, CheckTimeout: 10 * time.Minute, DNSTimeout: 5 * time.Second, HealthListen: "127.0.0.1:8081"}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, fmt.Errorf("parse slave config: %w", err)
	}
	if c.MasterURL == "" {
		c.MasterURL = c.LegacyMainURL
	}
	if c.DNSServer != "" {
		if _, _, err := net.SplitHostPort(c.DNSServer); err != nil {
			if net.ParseIP(c.DNSServer) == nil {
				return nil, errors.New("dns-server must be an IP or IP:port")
			}
			c.DNSServer = net.JoinHostPort(c.DNSServer, "53")
		}
	}
	if c.CheckInterval < 0 || c.CheckTimeout <= 0 || c.DNSTimeout <= 0 {
		return nil, errors.New("intervals and timeouts must be positive")
	}
	if c.TLS.CertFile == "" || c.TLS.KeyFile == "" || c.TLS.ServerName == "" {
		return nil, errors.New("tls.cert-file, tls.key-file and tls.server-name are required")
	}
	if c.LDAP.URL == "" || c.LDAP.BindDN == "" || c.LocalBaseDN == "" || c.StateDir == "" {
		return nil, errors.New("ldap.url, ldap.bind-dn, local-base-dn and state-dir are required")
	}
	if !strings.HasPrefix(c.LDAP.URL, "ldaps://") && !c.LDAP.StartTLS && !isLoopbackLDAP(c.LDAP.URL) {
		return nil, errors.New("non-loopback LDAP must use ldaps:// or start-tls")
	}
	if c.ClusterStatus.Enabled {
		if c.ClusterID == "" {
			return nil, errors.New("cluster-status needs cluster-id")
		}
		if c.ClusterStatus.Interval, err = reportInterval(c.ClusterStatus.Interval); err != nil {
			return nil, err
		}
	}
	if c.ResourceStatus.Enabled {
		if c.ClusterID == "" {
			return nil, errors.New("resource-status needs cluster-id")
		}
		if c.ResourceStatus.Interval, err = reportInterval(c.ResourceStatus.Interval); err != nil {
			return nil, err
		}
	}
	if c.CurrentStatus.Enabled {
		if c.ClusterID == "" {
			return nil, errors.New("current-status needs cluster-id")
		}
		if c.CurrentStatus.Interval == 0 {
			c.CurrentStatus.Interval = time.Minute
		}
		if c.CurrentStatus.Interval, err = reportInterval(c.CurrentStatus.Interval); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func isLoopbackLDAP(u string) bool {
	host := strings.TrimPrefix(u, "ldap://")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) clientTLS() (*tls.Config, error) {
	if _, err := privateFile(c.TLS.KeyFile); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(c.TLS.CertFile, c.TLS.KeyFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	if c.TLS.CAFile != "" {
		ca, err := os.ReadFile(c.TLS.CAFile)
		if err != nil {
			return nil, err
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return nil, errors.New("no CA certificates in tls.ca-file")
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, RootCAs: pool, ServerName: c.TLS.ServerName}, nil
}

// Build assembles a Replicator; the LDAP password comes from LDAP_BIND_PASSWORD or ldap.password-file.
func (c *Config) Build(getenv func(string) string) (*Replicator, error) {
	tlsCfg, err := c.clientTLS()
	if err != nil {
		return nil, err
	}
	pw := getenv("LDAP_BIND_PASSWORD")
	if pw == "" && c.LDAP.PasswordFile != "" {
		b, err := privateFile(c.LDAP.PasswordFile)
		if err != nil {
			return nil, err
		}
		pw = strings.TrimRight(string(b), "\r\n")
	}
	if pw == "" {
		return nil, errors.New("LDAP bind password is not configured")
	}
	ldapCfg := LDAPConfig{URL: c.LDAP.URL, BindDN: c.LDAP.BindDN, Password: pw, StartTLS: c.LDAP.StartTLS, ServerName: c.LDAP.ServerName}
	r := &Replicator{
		MasterURL: c.MasterURL, DNSName: c.DNSName, StateDir: c.StateDir,
		SourceBaseDN: c.SourceBaseDN, LocalBaseDN: c.LocalBaseDN, LocalGroupDN: c.LocalGroupDN, AdminDN: c.LDAP.BindDN,
		TrustDirs: c.TrustDirs, CheckTimeout: c.CheckTimeout, DNSTimeout: c.DNSTimeout,
		Client: &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Dial:   func() (DirectoryCloser, error) { return DialLDAP(ldapCfg) },
		Limits: c.Limits,
	}
	if c.DNSServer != "" {
		server := c.DNSServer
		r.Resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server)
		}}
	}
	if c.TriggerFile != "" {
		rules, err := LoadTriggers(c.TriggerFile)
		if err != nil {
			return nil, err
		}
		r.Queue = &Queue{Dir: c.StateDir + "/triggers", Rules: rules, Now: time.Now}
	}
	return r, r.Open()
}

func reportInterval(d time.Duration) (time.Duration, error) {
	if d == 0 {
		return 15 * time.Second, nil
	}
	if d < 5*time.Second || d > 10*time.Minute {
		return 0, errors.New("report interval must be within [5s, 10m]")
	}
	return d, nil
}
