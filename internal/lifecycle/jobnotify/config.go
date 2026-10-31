package jobnotify

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// TLSFiles names a certificate, its key and the CA used to verify the peer.
type TLSFiles struct {
	CertFile           string   `yaml:"cert-file"`
	KeyFile            string   `yaml:"key-file"`
	CAFile             string   `yaml:"ca-file"`
	ServerName         string   `yaml:"server-name"`
	ClientFingerprints []string `yaml:"client-fingerprints"`
}

// Config is the job-notification client configuration on a cluster.
type Config struct {
	ClusterID      string   `yaml:"cluster-id"`
	Listen         string   `yaml:"listen"`
	CallbackTLS    TLSFiles `yaml:"callback-tls"`
	SlurmURL       string   `yaml:"slurm-url"`
	SlurmTLS       TLSFiles `yaml:"slurm-tls"`
	MaxEvents      int      `yaml:"max-events"`
	MaxBytes       int64    `yaml:"max-bytes"`
	MaxEventBytes  int      `yaml:"max-event-bytes"`
	ResolveWorkers int      `yaml:"resolve-workers"`
	ResolveTTL     string   `yaml:"resolve-ttl"`
	SendTTL        string   `yaml:"send-ttl"`
	MaxAttempts    int      `yaml:"max-attempts"`
	RequestTimeout string   `yaml:"request-timeout"`
	RetryMax       string   `yaml:"retry-max"`
}

// ReadPrivateYAML decodes a YAML file that must not be readable by group or others.
func ReadPrivateYAML(path string, dst any) error {
	// Follow the target like existing slave configuration validation. This
	// supports Kubernetes Secret projections while still requiring 0600.
	info, err := os.Stat(path)
	if err != nil {
		return errors.New("notification configuration is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 64<<10 {
		return errors.New("notification configuration must be a regular file with permissions 0600 or stricter, at most 64KiB")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return errors.New("notification configuration is unavailable")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err = decoder.Decode(dst); err != nil {
		return errors.New("invalid notification configuration")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("notification configuration must contain one document")
	}
	return nil
}

// LoadConfig reads and validates Config.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	if err := ReadPrivateYAML(path, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.defaults()
}

func (c *Config) defaults() error {
	if !clusterPattern.MatchString(c.ClusterID) {
		return errors.New("invalid notification cluster identity")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return errors.New("notification listen must be an explicit IP and port")
	}
	host, _, _ := net.SplitHostPort(c.Listen)
	if net.ParseIP(host) == nil {
		return errors.New("notification listen must use an IP address")
	}
	if _, err := httpsURL(c.SlurmURL); err != nil {
		return err
	}
	if len(c.CallbackTLS.ClientFingerprints) == 0 {
		return errors.New("controller client certificate fingerprints are required")
	}
	for _, f := range c.CallbackTLS.ClientFingerprints {
		b, e := hex.DecodeString(f)
		if e != nil || len(b) != 32 {
			return errors.New("invalid controller certificate fingerprint")
		}
	}
	if c.MaxEvents == 0 {
		c.MaxEvents = 1000
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 64 << 20
	}
	if c.MaxEventBytes == 0 {
		c.MaxEventBytes = 256 << 10
	}
	if c.ResolveWorkers == 0 {
		c.ResolveWorkers = 2
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 300
	}
	if c.ResolveTTL == "" {
		c.ResolveTTL = "15m"
	}
	if c.SendTTL == "" {
		c.SendTTL = "24h"
	}
	if c.RequestTimeout == "" {
		c.RequestTimeout = "20s"
	}
	if c.RetryMax == "" {
		c.RetryMax = "5m"
	}
	if c.MaxEvents < 1 || c.MaxEvents > 10000 || c.MaxBytes < 4096 || c.MaxBytes > 1<<30 || c.MaxEventBytes < 2048 || c.MaxEventBytes > 256<<10 || int64(c.MaxEventBytes) > c.MaxBytes || c.ResolveWorkers < 1 || c.ResolveWorkers > 4 || c.MaxAttempts < 1 || c.MaxAttempts > 1000 {
		return errors.New("invalid notification memory or retry limits")
	}
	for _, s := range []string{c.ResolveTTL, c.SendTTL, c.RequestTimeout, c.RetryMax} {
		d, e := time.ParseDuration(s)
		if e != nil || d < time.Millisecond || d > 7*24*time.Hour {
			return errors.New("invalid notification duration")
		}
	}
	d, _ := time.ParseDuration(c.RequestTimeout)
	if d > 30*time.Second {
		return errors.New("notification request timeout must not exceed 30s")
	}
	return nil
}

func httpsURL(value string) (*url.URL, error) {
	u, e := url.Parse(value)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("notification endpoints must be HTTPS without credentials, query or fragment")
	}
	return u, nil
}

func clientTLS(files TLSFiles) (*tls.Config, error) {
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: files.ServerName}
	if files.CAFile != "" {
		b, e := os.ReadFile(files.CAFile)
		if e != nil {
			return nil, errors.New("notification CA is unavailable")
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid notification CA")
		}
		config.RootCAs = pool
	}
	if (files.CertFile == "") != (files.KeyFile == "") {
		return nil, errors.New("both notification client certificate and key are required")
	}
	if files.CertFile != "" {
		cert, e := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
		if e != nil {
			return nil, errors.New("notification client certificate is unavailable")
		}
		config.Certificates = []tls.Certificate{cert}
	}
	return config, nil
}

func serverTLS(files TLSFiles) (*tls.Config, error) {
	cert, e := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if e != nil {
		return nil, errors.New("notification callback server certificate is unavailable")
	}
	b, e := os.ReadFile(files.CAFile)
	if e != nil {
		return nil, errors.New("notification callback CA is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b) {
		return nil, errors.New("invalid callback CA")
	}
	allowed := map[string]bool{}
	for _, f := range files.ClientFingerprints {
		allowed[strings.ToLower(f)] = true
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("controller certificate required")
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
		if !allowed[hex.EncodeToString(sum[:])] {
			return errors.New("controller certificate is not authorized")
		}
		return nil
	}}, nil
}

func certificateFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func newHTTPClient(config *tls.Config, timeout time.Duration) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: config, MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("notification redirects are forbidden") }}
}
