package joblifecycle

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
)

// HookConfig is the configuration of cmd/slurm-lifecycle-hook.
type HookConfig struct {
	URL            string             `yaml:"url"`
	TLS            jobnotify.TLSFiles `yaml:"tls"`
	WriteTokenFile string             `yaml:"write-token-file"`
	Timeout        string             `yaml:"timeout"`
}

// LoadHookConfig reads and validates HookConfig.
func LoadHookConfig(path string) (HookConfig, error) {
	var c HookConfig
	if err := jobnotify.ReadPrivateYAML(path, &c); err != nil {
		return c, err
	}
	if c.Timeout == "" {
		c.Timeout = "1s"
	}
	d, err := time.ParseDuration(c.Timeout)
	if err != nil || d < time.Millisecond || d > 2*time.Second {
		return c, errors.New("lifecycle hook timeout must be between 1ms and 2s")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return c, errors.New("lifecycle hook requires an HTTPS slave origin")
	}
	if c.WriteTokenFile == "" || c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
		return c, errors.New("lifecycle hook local credential and mTLS identity required")
	}
	return c, nil
}

// SendHook performs a bounded authenticated handoff, never Slurm API work,
// resource sampling, SMTP work or a short-job filter. Both hooks use this path.
func SendHook(ctx context.Context, cfg HookConfig, eventType string, c jobnotify.Callback) error {
	if c.Validate() != nil {
		return errors.New("invalid lifecycle hook identity")
	}
	c.SLUID = jobnotify.CanonicalSLUID(c.SLUID)
	path := StartingPath
	if eventType == EndingType {
		path = EndingPath
	} else if eventType != StartingType {
		return errors.New("unknown lifecycle hook event")
	}
	info, err := os.Lstat(cfg.WriteTokenFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 256 {
		return errors.New("hook write credential unavailable")
	}
	b, err := os.ReadFile(cfg.WriteTokenFile)
	if err != nil {
		return errors.New("hook write credential unavailable")
	}
	token := strings.TrimSpace(string(b))
	if len(token) != 43 || strings.ContainsAny(token, "\r\n ") {
		return errors.New("invalid hook write credential")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.RawPath != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid lifecycle hook endpoint")
	}
	u.Path = path
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout < time.Millisecond || timeout > 2*time.Second {
		return errors.New("invalid lifecycle hook timeout")
	}
	tlsConfig, err := jobnotify.ClientTLS(cfg.TLS)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	conn, err := (&tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: tlsConfig}).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return errors.New("lifecycle hook connection failed")
	}
	defer func() { _ = conn.(*tls.Conn).NetConn().Close() }()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.(*tls.Conn).NetConn().Close() })
	defer stop()
	body, _ := json.Marshal(c)
	request, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Close = true
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	if request.Write(conn) != nil {
		return errors.New("lifecycle hook write failed")
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(conn, 8192)), request)
	if err != nil {
		return errors.New("lifecycle hook acceptance unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 202 {
		return errors.New("lifecycle hook was not accepted")
	}
	return nil
}
