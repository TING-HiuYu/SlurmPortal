// Package webaccessgateway is the native, user-scoped SSH data plane. It never
// possesses a CA private key or accepts the application's ordinary login JWT.
package gateway

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// Cluster is a cluster the gateway reaches over SSH, for sessions and transfers.
type Cluster struct {
	ID         string   `yaml:"id"`
	SSHAddress string   `yaml:"ssh_address"`
	HostKeys   []string `yaml:"host_keys"` // Legacy compatibility input, intentionally ignored.
}

// Config is the gateway's YAML configuration (deploy/gateway/*.yaml).
type Config struct {
	GatewayID               string            `yaml:"gateway_id"`
	InstanceID              string            `yaml:"instance_id"`
	ClusterID               string            `yaml:"cluster_id"`
	Listen                  string            `yaml:"listen"`
	TLSCertificate          string            `yaml:"tls_certificate"`
	TLSKey                  string            `yaml:"tls_key"`
	CallbackListen          string            `yaml:"callback_listen"`
	AllowedOrigins          []string          `yaml:"allowed_origins"`
	TicketPublicKeys        map[string]string `yaml:"ticket_public_keys"`
	UserCAKeys              []string          `yaml:"user_ca_keys"`
	UserCAKeysFile          string            `yaml:"user_ca_keys_file"`
	FileRoot                string            `yaml:"file_root"`
	Clusters                []Cluster         `yaml:"clusters"`
	StateDirectory          string            `yaml:"state_directory"`
	MasterOrigin            string            `yaml:"master_origin"`
	MasterClientCertificate string            `yaml:"master_client_certificate"`
	MasterClientKey         string            `yaml:"master_client_key"`
	// The former names of the three keys above; each is used when its new key is empty.
	LegacyMainOrigin            string `yaml:"main_origin"`
	LegacyMainClientCertificate string `yaml:"main_client_certificate"`
	LegacyMainClientKey         string `yaml:"main_client_key"`
	MaxSessions                 int    `yaml:"max_sessions"`
	MaxSessionsPerUser          int    `yaml:"max_sessions_per_user"`
	SessionIdleSeconds          int    `yaml:"session_idle_seconds"`
	MaxDownloadBytes            int64  `yaml:"max_download_bytes"`
	MaxTransfers                int    `yaml:"max_transfers"`
	parsedKeys                  map[string]ed25519.PublicKey
	userCAs                     []ssh.PublicKey
}

var identifier = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var usernamePattern = regexp.MustCompile(`(?i)^[a-z_][a-z0-9_-]{0,31}\$?$`)

// LoadConfig reads and validates the gateway configuration.
func LoadConfig(path string) (Config, error) {
	var c Config
	f, e := os.Open(path)
	if e != nil {
		return c, e
	}
	defer func() { _ = f.Close() }()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return c, errors.New("configuration must be a private regular file")
	}
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, e
	}
	return c, c.Validate()
}

// Validate checks the configuration and fills in defaults.
func (c *Config) Validate() error {
	for _, k := range []struct {
		dst *string
		old string
	}{
		{&c.MasterOrigin, c.LegacyMainOrigin}, {&c.MasterClientCertificate, c.LegacyMainClientCertificate}, {&c.MasterClientKey, c.LegacyMainClientKey},
	} {
		if *k.dst == "" {
			*k.dst = k.old
		}
	}
	if !identifier.MatchString(c.GatewayID) || !identifier.MatchString(c.ClusterID) {
		return errors.New("invalid gateway or cluster ID")
	}
	if c.InstanceID == "" {
		c.InstanceID = c.GatewayID
	}
	if !identifier.MatchString(c.InstanceID) {
		return errors.New("invalid instance ID")
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:19934"
	}
	if c.CallbackListen == "" {
		c.CallbackListen = "127.0.0.1:19935"
	}
	callbackHost, _, e := net.SplitHostPort(c.CallbackListen)
	if e != nil || net.ParseIP(callbackHost) == nil || !net.ParseIP(callbackHost).IsLoopback() {
		return errors.New("callback must bind a literal loopback IP")
	}
	if len(c.AllowedOrigins) == 0 || len(c.AllowedOrigins) > 16 {
		return errors.New("origins required")
	}
	for _, o := range c.AllowedOrigins {
		u, e := url.Parse(o)
		if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && (u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback())) {
			return errors.New("invalid exact origin")
		}
	}
	if len(c.TicketPublicKeys) == 0 || len(c.TicketPublicKeys) > 8 {
		return errors.New("ticket public keys required")
	}
	c.parsedKeys = map[string]ed25519.PublicKey{}
	for kid, value := range c.TicketPublicKeys {
		if !identifier.MatchString(kid) {
			return errors.New("invalid key ID")
		}
		var key ed25519.PublicKey
		if b, _ := pem.Decode([]byte(value)); b != nil {
			v, e := x509.ParsePKIXPublicKey(b.Bytes)
			if e != nil {
				return e
			}
			var ok bool
			key, ok = v.(ed25519.PublicKey)
			if !ok {
				return errors.New("ticket key must be Ed25519")
			}
		} else {
			v, e := base64.RawURLEncoding.DecodeString(value)
			if e != nil || len(v) != ed25519.PublicKeySize {
				return errors.New("invalid ticket public key")
			}
			key = ed25519.PublicKey(v)
		}
		c.parsedKeys[kid] = key
	}
	if c.FileRoot == "" {
		c.FileRoot = "/HOME"
	}
	if !strings.HasPrefix(c.FileRoot, "/") || c.FileRoot == "/" || path.Clean(c.FileRoot) != c.FileRoot || len(c.FileRoot) > 4096 || strings.ContainsAny(c.FileRoot, "\x00\r\n") {
		return errors.New("invalid shared file root")
	}
	if c.UserCAKeysFile == "" && len(c.UserCAKeys) == 0 {
		c.UserCAKeysFile = "/etc/ssh/user_ca_list.pub"
	}
	c.userCAs, e = c.trustedUserCAs()
	if e != nil {
		return e
	}
	if len(c.Clusters) > 64 {
		return errors.New("too many clusters")
	}
	source := false
	seenClusters := map[string]bool{}
	for _, v := range c.Clusters {
		if !identifier.MatchString(v.ID) || seenClusters[v.ID] {
			return errors.New("invalid cluster identifier")
		}
		seenClusters[v.ID] = true
		host, port, e := net.SplitHostPort(v.SSHAddress)
		number, portErr := strconv.Atoi(port)
		if e != nil || host == "" || portErr != nil || number < 1 || number > 65535 || (net.ParseIP(host) == nil && !regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`).MatchString(host)) {
			return errors.New("invalid SSH address")
		}
		if v.ID == c.ClusterID {
			source = true
			if host != "localhost" && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
				return errors.New("source SSH must be on this login node loopback")
			}
		}
	}
	if !source {
		return errors.New("source cluster missing")
	}
	if c.StateDirectory == "" || !strings.HasPrefix(c.StateDirectory, "/") {
		return errors.New("absolute state directory required")
	}
	if c.MaxSessions == 0 {
		c.MaxSessions = 128
	}
	if c.MaxSessionsPerUser == 0 {
		c.MaxSessionsPerUser = 4
	}
	if c.SessionIdleSeconds == 0 {
		c.SessionIdleSeconds = 1800
	}
	if c.MaxDownloadBytes == 0 {
		c.MaxDownloadBytes = 1 << 40
	}
	if c.MaxTransfers == 0 {
		c.MaxTransfers = 128
	}
	if c.MaxSessions < 1 || c.MaxSessions > 1024 || c.MaxSessionsPerUser < 1 || c.MaxSessionsPerUser > 32 || c.SessionIdleSeconds < 60 || c.SessionIdleSeconds > 86400 || c.MaxTransfers < 1 || c.MaxTransfers > 1024 || c.MaxDownloadBytes < 1 {
		return errors.New("invalid resource limits")
	}
	if c.MasterOrigin != "" {
		u, e := url.Parse(c.MasterOrigin)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid master HTTPS origin")
		}
	}
	return nil
}
func publicKey(value string) (ssh.PublicKey, error) {
	k, _, _, rest, e := ssh.ParseAuthorizedKey([]byte(value))
	if e != nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid SSH public key")
	}
	if _, ok := k.(*ssh.Certificate); ok {
		return nil, errors.New("expected pinned raw public key")
	}
	return k, nil
}
func (c Config) cluster(id string) (Cluster, bool) {
	for _, v := range c.Clusters {
		if v.ID == id {
			return v, true
		}
	}
	return Cluster{}, false
}

const sshTimeout = 10 * time.Second
