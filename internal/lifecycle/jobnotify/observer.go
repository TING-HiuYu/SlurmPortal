package jobnotify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const observerMaxResponse = 4 << 20

// ObserverConfig is installed only on the controller. The JWT signing key must
// never be copied to a slave: its authority is much broader than this gateway.
type ObserverConfig struct {
	Listen         string   `yaml:"listen"`
	ServerTLS      TLSFiles `yaml:"server-tls"`
	UpstreamURL    string   `yaml:"upstream-url"`
	JWTKeyFile     string   `yaml:"jwt-key-file"`
	JWTUsername    string   `yaml:"jwt-username"`
	RequestTimeout string   `yaml:"request-timeout"`
}

// LoadObserverConfig reads and validates ObserverConfig.
func LoadObserverConfig(path string) (ObserverConfig, error) {
	var cfg ObserverConfig
	if err := ReadPrivateYAML(path, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.validate()
}

func (c *ObserverConfig) validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	n, perr := strconv.ParseUint(port, 10, 16)
	if err != nil || perr != nil || n == 0 || net.ParseIP(host) == nil {
		return errors.New("observer listen must use an explicit IP and nonzero port")
	}
	if c.UpstreamURL == "" {
		c.UpstreamURL = "http://127.0.0.1:6820"
	}
	u, err := url.Parse(c.UpstreamURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("observer upstream must be an HTTP loopback origin")
	}
	ip := net.ParseIP(u.Hostname())
	n, perr = strconv.ParseUint(u.Port(), 10, 16)
	if ip == nil || !ip.IsLoopback() || perr != nil || n == 0 {
		return errors.New("observer upstream must be a literal loopback IP and nonzero port")
	}
	if c.JWTUsername == "" {
		c.JWTUsername = "slurm"
	}
	if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,63}$`).MatchString(c.JWTUsername) || c.JWTKeyFile == "" {
		return errors.New("observer JWT identity or key file is invalid")
	}
	if c.RequestTimeout == "" {
		c.RequestTimeout = "10s"
	}
	d, err := time.ParseDuration(c.RequestTimeout)
	if err != nil || d < time.Millisecond || d > 20*time.Second {
		return errors.New("observer request timeout must be between 1ms and 20s")
	}
	if len(c.ServerTLS.ClientFingerprints) == 0 {
		return errors.New("observer client certificate fingerprints are required")
	}
	for _, value := range c.ServerTLS.ClientFingerprints {
		if len(value) != 64 {
			return errors.New("observer client certificate fingerprint is invalid")
		}
		for _, char := range strings.ToLower(value) {
			if !strings.ContainsRune("0123456789abcdef", char) {
				return errors.New("observer client certificate fingerprint is invalid")
			}
		}
	}
	return nil
}

func readObserverKey(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("observer JWT key is unavailable")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 32 || info.Size() > 4096 {
		return nil, errors.New("observer JWT key must be a private regular file of 32 to 4096 bytes")
	}
	key, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || len(key) < 32 || len(key) > 4096 {
		return nil, errors.New("observer JWT key is unavailable")
	}
	return key, nil
}

// Slurm 26.05 auth_p_token_generate uses the raw key bytes and iat/exp/sun.
// See SchedMD slurm-26-05-4-1/src/plugins/auth/jwt/auth_jwt.c. Identity lookup
// stays on the controller; no broad JWT is ever returned to the client.
func observerJWT(key []byte, username string, now time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims, _ := json.Marshal(struct {
		IssuedAt int64  `json:"iat"`
		Expires  int64  `json:"exp"`
		Username string `json:"sun"`
	}{now.Unix(), now.Unix() + 60, username})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type observerHandler struct {
	cfg     ObserverConfig
	key     []byte
	client  *http.Client
	allowed map[string]bool
	busy    chan struct{}
}

func newObserverHandler(cfg ObserverConfig, key []byte) *observerHandler {
	timeout, _ := time.ParseDuration(cfg.RequestTimeout)
	allowed := make(map[string]bool, len(cfg.ServerTLS.ClientFingerprints))
	for _, value := range cfg.ServerTLS.ClientFingerprints {
		allowed[strings.ToLower(value)] = true
	}
	return &observerHandler{cfg: cfg, key: key, allowed: allowed, busy: make(chan struct{}, 16), client: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: nil, DisableCompression: true,
			DialContext:     (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
			MaxConnsPerHost: 16, MaxIdleConns: 16, MaxIdleConnsPerHost: 16,
			IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: timeout,
			MaxResponseHeaderBytes: 32 << 10,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("observer redirects are forbidden") },
	}}
}

func observerError(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"observer request rejected"}`)
}

func (h *observerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 || !h.allowed[certificateFingerprint(r.TLS.PeerCertificates[0].Raw)] {
		observerError(w, http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		observerError(w, http.StatusMethodNotAllowed)
		return
	}
	const prefix = "/slurm/" + SlurmAPIVersion + "/job/"
	const resourcesPrefix = "/slurm/" + SlurmAPIVersion + "/resources/"
	allowedPrefix := prefix
	if strings.HasPrefix(r.URL.Path, resourcesPrefix) {
		allowedPrefix = resourcesPrefix
	}
	idText := strings.TrimPrefix(r.URL.Path, allowedPrefix)
	id, err := strconv.ParseUint(idText, 10, 32)
	if !strings.HasPrefix(r.URL.Path, allowedPrefix) || err != nil || id == 0 || id >= uint64(^uint32(0)-1) || strconv.FormatUint(id, 10) != idText || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.Fragment != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		observerError(w, http.StatusBadRequest)
		return
	}
	select {
	case h.busy <- struct{}{}:
		defer func() { <-h.busy }()
	default:
		observerError(w, http.StatusServiceUnavailable)
		return
	}
	token := observerJWT(h.key, h.cfg.JWTUsername, time.Now())
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimSuffix(h.cfg.UpstreamURL, "/")+r.URL.Path, nil)
	if err != nil {
		observerError(w, http.StatusBadGateway)
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-SLURM-USER-NAME", h.cfg.JWTUsername)
	req.Header.Set("X-SLURM-USER-TOKEN", token)
	response, err := h.client.Do(req)
	if err != nil {
		observerError(w, http.StatusBadGateway)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		status := http.StatusBadGateway
		if response.StatusCode == http.StatusNotFound {
			status = http.StatusNotFound
		}
		observerError(w, status)
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, observerMaxResponse+1))
	if err != nil || len(body) > observerMaxResponse || !json.Valid(body) || bytes.Contains(body, []byte(token)) {
		observerError(w, http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// RunObserver serves until ctx is cancelled. It has no queue, database, access
// log, command execution, or persistent event data. Shutdown closes connections
// immediately so the enclosing systemd/Pod process can terminate promptly.
func RunObserver(ctx context.Context, path string) error {
	cfg, err := LoadObserverConfig(path)
	if err != nil {
		return err
	}
	key, err := readObserverKey(cfg.JWTKeyFile)
	if err != nil {
		return err
	}
	tlsConfig, err := serverTLS(cfg.ServerTLS)
	if err != nil {
		return errors.New("observer TLS configuration failed")
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("observer listener unavailable")
	}
	timeout, _ := time.ParseDuration(cfg.RequestTimeout)
	handler := newObserverHandler(cfg, key)
	defer handler.client.CloseIdleConnections()
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout: 3 * time.Second, WriteTimeout: timeout + 2*time.Second,
		IdleTimeout: 15 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-stop:
		}
	}()
	err = server.Serve(tls.NewListener(listener, tlsConfig))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return errors.New("observer listener failed")
}
