package jobnotify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HookConfig is the configuration of the Slurm prolog/epilog hook.
type HookConfig struct {
	URL      string   `yaml:"url"`
	TLS      TLSFiles `yaml:"tls"`
	Timeout  string   `yaml:"timeout"`
	SlurmURL string   `yaml:"slurm-url"`
	SlurmTLS TLSFiles `yaml:"slurm-tls"`
}

// tls.Conn.Close may replace the caller's deadline with a five-second
// close_notify write. The hook has a stricter total budget and no response
// contract, so request-owned sockets close their underlying transport directly.
type hookTLSConnection struct{ *tls.Conn }

func (c *hookTLSConnection) Close() error { return c.NetConn().Close() }
func closeHookConnection(connection net.Conn) error {
	if secured, ok := connection.(*tls.Conn); ok {
		return secured.NetConn().Close()
	}
	return connection.Close()
}

func hookHTTPClient(config *tls.Config, timeout time.Duration) *http.Client {
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: config}
	return &http.Client{Timeout: timeout, Transport: &http.Transport{
		DisableKeepAlives: true,
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &hookTLSConnection{connection.(*tls.Conn)}, nil
		},
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("hook redirects are forbidden") }}
}

// LoadHookConfig reads and validates HookConfig.
func LoadHookConfig(path string) (HookConfig, error) {
	var cfg HookConfig
	if err := ReadPrivateYAML(path, &cfg); err != nil {
		return cfg, err
	}
	u, err := httpsURL(cfg.URL)
	if err != nil {
		return cfg, err
	}
	if u.Path != CallbackPath {
		return cfg, errors.New("invalid notification callback path")
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "1s"
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout < time.Millisecond || timeout > 2*time.Second {
		return cfg, errors.New("hook deadline must be between 1ms and 2s")
	}
	if cfg.TLS.CertFile == "" || cfg.TLS.KeyFile == "" {
		return cfg, errors.New("hook mTLS identity is required")
	}
	observer, err := httpsURL(cfg.SlurmURL)
	if err != nil || (observer.Path != "" && observer.Path != "/") || cfg.SlurmTLS.CertFile == "" || cfg.SlurmTLS.KeyFile == "" {
		return cfg, errors.New("hook runtime observer mTLS origin is required")
	}
	return cfg, nil
}

// HookRuntimeWindow uses controller-generated environment timestamps as a cheap
// upper bound. Short/unallocated jobs return before config, TLS or any network.
// Longer wall time still needs an observer read to exclude suspended time.
func HookRuntimeWindow(startValue, endValue string) (start, end int64, candidate bool, err error) {
	parse := func(value string) (int64, error) {
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, errors.New("invalid controller timestamps")
		}
		return strconv.ParseInt(value, 10, 64)
	}
	start, err = parse(startValue)
	if err != nil {
		return 0, 0, false, errors.New("invalid controller timestamps")
	}
	end, err = parse(endValue)
	if err != nil {
		return 0, 0, false, errors.New("invalid controller timestamps")
	}
	if start == 0 {
		return start, end, false, nil
	}
	if end < start || end-start > (1<<53)-1 {
		return 0, 0, false, errors.New("invalid controller timestamps")
	}
	return start, end, end-start > MinActiveSeconds, nil
}

// HookEligible makes one bounded GET against the existing read-only gateway.
// It does not queue, retry, execute Slurm commands, mint JWTs, or log job data.
func HookEligible(ctx context.Context, cfg HookConfig, callback Callback, start, end int64) (bool, error) {
	if callback.Validate() != nil || start <= 0 || end < start || end-start <= MinActiveSeconds {
		return false, nil
	}
	u, err := httpsURL(cfg.SlurmURL)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return false, errors.New("invalid hook observer endpoint")
	}
	tlsConfig, err := clientTLS(cfg.SlurmTLS)
	if err != nil || len(tlsConfig.Certificates) == 0 {
		return false, errors.New("hook observer identity unavailable")
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout <= 0 || timeout > 2*time.Second {
		return false, errors.New("invalid hook timeout")
	}
	client := hookHTTPClient(tlsConfig, timeout)
	defer client.CloseIdleConnections()
	u.Path = "/slurm/" + SlurmAPIVersion + "/job/" + strconv.FormatUint(uint64(callback.JobID), 10)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false, errors.New("hook runtime unavailable")
	}
	request.Header.Set("Accept", "application/json")
	request.Close = true
	response, err := client.Do(request)
	if err != nil {
		return false, errors.New("hook runtime unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return false, errors.New("hook runtime unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return false, errors.New("hook runtime unavailable")
	}
	var payload struct {
		Jobs   []map[string]json.RawMessage `json:"jobs"`
		Errors []json.RawMessage            `json:"errors"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Errors) != 0 {
		return false, errors.New("hook runtime cannot be verified")
	}
	var selected map[string]json.RawMessage
	for _, row := range payload.Jobs {
		id, ok := number(row["job_id"])
		if !ok || id != int64(callback.JobID) {
			continue
		}
		if selected != nil {
			return false, errors.New("hook runtime cannot be verified")
		}
		selected = row
	}
	if selected == nil {
		return false, errors.New("hook runtime cannot be verified")
	}
	var step struct {
		SLUID string `json:"sluid"`
	}
	if json.Unmarshal(selected["step_id"], &step) != nil || canonicalSLUID(step.SLUID) != canonicalSLUID(callback.SLUID) {
		return false, nil
	}
	s, sok := number(selected["start_time"])
	e, eok := number(selected["end_time"])
	suspend, suspendOK := number(selected["suspend_time"])
	pre, preOK := number(selected["pre_sus_time"])
	if !sok || !eok || !suspendOK || !preOK || s != start || e != end {
		return false, errors.New("hook runtime cannot be verified")
	}
	seconds, ok := activeRuntimeSeconds(s, e, suspend, pre)
	if !ok {
		return false, errors.New("hook runtime cannot be verified")
	}
	return seconds > MinActiveSeconds, nil
}

// SendHook performs exactly one connection and one complete HTTP/1.1 request
// write. It intentionally never reads an HTTP response or retries. TLS and TCP
// handshakes still occur before the write; local write success is not an ACK.
func SendHook(ctx context.Context, cfg HookConfig, callback Callback) error {
	if err := callback.Validate(); err != nil {
		return err
	}
	callback.SLUID = canonicalSLUID(callback.SLUID)
	u, err := httpsURL(cfg.URL)
	if err != nil || u.Path != CallbackPath {
		return errors.New("invalid notification callback endpoint")
	}
	timeout, err := time.ParseDuration(cfg.Timeout)
	if err != nil || timeout < time.Millisecond || timeout > 2*time.Second {
		return errors.New("invalid hook timeout")
	}
	tlsConfig, err := clientTLS(cfg.TLS)
	if err != nil {
		return err
	}
	if len(tlsConfig.Certificates) == 0 {
		return errors.New("hook mTLS identity is required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: tlsConfig}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return errors.New("hook connection failed")
	}
	defer func() { _ = closeHookConnection(connection) }()
	deadline, _ := ctx.Deadline()
	if connection.SetDeadline(deadline) != nil {
		return errors.New("hook deadline failed")
	}
	body, _ := json.Marshal(callback)
	request, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid hook request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "go-ldap-admin-slurm-hook/1")
	request.Close = true
	if request.Write(connection) != nil {
		return errors.New("hook request write failed")
	}
	return nil
}
