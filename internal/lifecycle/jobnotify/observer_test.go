package jobnotify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func observerFixtureConfig(material tlsMaterial) ObserverConfig {
	files := material.files
	files.ClientFingerprints = []string{material.fingerprint}
	return ObserverConfig{Listen: "127.0.0.1:9445", ServerTLS: files, JWTKeyFile: "unused", JWTUsername: "slurm", UpstreamURL: "http://127.0.0.1:6820", RequestTimeout: "1s"}
}

func observerAuthorizedRequest(t *testing.T, material tlsMaterial, method, path string, body io.Reader) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, body)
	leaf, err := x509.ParseCertificate(material.certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	return r
}

func TestObserverJWTMatchesSlurmShortLivedRawKeyFormat(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef\n")
	now := time.Unix(1900000000, 0)
	parts := strings.Split(observerJWT(key, "slurm", now), ".")
	if len(parts) != 3 {
		t.Fatal("JWT segments")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(header) != `{"alg":"HS256","typ":"JWT"}` {
		t.Fatal("JWT header")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	if err != nil || json.Unmarshal(payload, &claims) != nil || len(claims) != 3 || claims["sun"] != "slurm" || claims["iat"] != float64(now.Unix()) || claims["exp"] != float64(now.Unix()+60) {
		t.Fatal("JWT claims")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if parts[2] != base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("JWT raw-byte signature")
	}
}

func TestObserverConfigurationAndPrivateKeyBoundaries(t *testing.T) {
	material := testTLS(t)
	valid := observerFixtureConfig(material)
	for _, endpoint := range []string{"https://127.0.0.1:6820", "http://localhost:6820", "http://192.0.2.1:6820", "http://127.0.0.1:6820/a", "http://u:p@127.0.0.1:6820", "http://127.0.0.1:6820?", "http://127.0.0.1:6820#fragment", "http://127.0.0.1:0"} {
		cfg := valid
		cfg.UpstreamURL = endpoint
		if cfg.validate() == nil {
			t.Fatalf("unsafe upstream accepted: %s", endpoint)
		}
	}
	for _, listen := range []string{"localhost:9445", "127.0.0.1:0", ":9445", "127.0.0.1:70000"} {
		cfg := valid
		cfg.Listen = listen
		if cfg.validate() == nil {
			t.Fatal("invalid listener accepted")
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	for _, test := range []struct {
		mode os.FileMode
		size int
		ok   bool
	}{{0600, 32, true}, {0400, 32, true}, {0640, 32, false}, {0600, 31, false}, {0600, 4097, false}} {
		_ = os.Remove(path)
		if os.WriteFile(path, []byte(strings.Repeat("k", test.size)), test.mode) != nil {
			t.Fatal("key fixture")
		}
		_, err := readObserverKey(path)
		if (err == nil) != test.ok {
			t.Fatalf("private key boundary failed: mode %o size %d", test.mode, test.size)
		}
	}
	configPath := filepath.Join(dir, "observer.yaml")
	data, _ := yaml.Marshal(valid)
	if os.WriteFile(configPath, data, 0600) != nil {
		t.Fatal("config fixture")
	}
	if _, err := LoadObserverConfig(configPath); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(configPath, 0644)
	if _, err := LoadObserverConfig(configPath); err == nil {
		t.Fatal("public observer configuration accepted")
	}
}

func TestObserverOnlyAuthorizedGETJobAndNoHeaderForwarding(t *testing.T) {
	material := testTLS(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != "/slurm/v0.0.45/job/7" || r.Header.Get("X-SLURM-USER-NAME") != "slurm" || r.Header.Get("X-SLURM-USER-TOKEN") == "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Arbitrary") != "" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected forwarded request")
		}
		w.Header().Set("X-SLURM-USER-TOKEN", "must-not-leak")
		_, _ = io.WriteString(w, `{"jobs":[]}`)
	}))
	defer upstream.Close()
	cfg := observerFixtureConfig(material)
	cfg.UpstreamURL = upstream.URL
	h := newObserverHandler(cfg, []byte(strings.Repeat("k", 32)))
	defer h.client.CloseIdleConnections()
	unauthorized := observerAuthorizedRequest(t, material, http.MethodGet, "/slurm/v0.0.45/job/7", nil)
	unauthorized.TLS = nil
	w := httptest.NewRecorder()
	h.ServeHTTP(w, unauthorized)
	if w.Code != http.StatusForbidden {
		t.Fatal("non-mTLS request accepted")
	}
	foreign := observerAuthorizedRequest(t, testTLS(t), http.MethodGet, "/slurm/v0.0.45/job/7", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, foreign)
	if w.Code != http.StatusForbidden {
		t.Fatal("verified but unpinned peer accepted")
	}
	for _, path := range []string{"/slurm/v0.0.45/jobs", "/slurm/v0.0.45/job/0", "/slurm/v0.0.45/job/01", "/slurm/v0.0.45/job/4294967294", "/slurm/v0.0.45/job/7/", "/slurm/v0.0.45/job/%37", "/slurm/v0.0.45/job/7?", "/slurm/v0.0.45/job/7?token=bad", "/slurm/v0.0.45/job/7/../8", "/slurm/v0.0.44/job/7"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, observerAuthorizedRequest(t, material, http.MethodGet, path, nil))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unsafe path accepted: %s status %d", path, w.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodHead, http.MethodOptions} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, observerAuthorizedRequest(t, material, method, "/slurm/v0.0.45/job/7", nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatal("write method accepted")
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, observerAuthorizedRequest(t, material, http.MethodGet, "/slurm/v0.0.45/job/7", strings.NewReader("body")))
	if w.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatal("rejected request reached upstream")
	}
	r := observerAuthorizedRequest(t, material, http.MethodGet, "/slurm/v0.0.45/job/7", nil)
	r.Header.Set("X-SLURM-USER-NAME", "root")
	r.Header.Set("X-SLURM-USER-TOKEN", "attacker")
	r.Header.Set("Authorization", "Bearer attacker")
	r.Header.Set("X-Arbitrary", "attacker")
	r.Header.Set("Cookie", "attacker")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != `{"jobs":[]}` || w.Header().Get("X-SLURM-USER-TOKEN") != "" || w.Header().Get("Cache-Control") != "no-store" || calls.Load() != 1 {
		t.Fatal("valid response or header isolation failed")
	}
}

func TestObserverResourceLayoutUsesAuthenticatedReadOnlyPath(t *testing.T) {
	material := testTLS(t)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != "/slurm/v0.0.45/resources/7" || r.Header.Get("X-SLURM-USER-TOKEN") == "" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected resource request")
		}
		_, _ = io.WriteString(w, `{"nodes":[]}`)
	}))
	defer upstream.Close()
	cfg := observerFixtureConfig(material)
	cfg.UpstreamURL = upstream.URL
	h := newObserverHandler(cfg, []byte(strings.Repeat("k", 32)))
	defer h.client.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, observerAuthorizedRequest(t, material, http.MethodGet, "/slurm/v0.0.45/resources/7", nil))
	if w.Code != http.StatusOK || w.Body.String() != `{"nodes":[]}` || calls.Load() != 1 {
		t.Fatal("resource layout was not forwarded")
	}
	for _, path := range []string{"/slurm/v0.0.45/resources/0", "/slurm/v0.0.45/resources/07", "/slurm/v0.0.45/resources/7?extra=1", "/slurm/v0.0.45/resources/7/extra"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, observerAuthorizedRequest(t, material, http.MethodGet, path, nil))
		if w.Code != http.StatusBadRequest || calls.Load() != 1 {
			t.Fatalf("unsafe resource path accepted: %s", path)
		}
	}
}

func TestObserverRedirectSizeJSONAndTimeoutFailures(t *testing.T) {
	material := testTLS(t)
	for _, scenario := range []string{"redirect", "size", "invalid-json", "token-echo", "timeout", "not-found"} {
		t.Run(scenario, func(t *testing.T) {
			var redirected atomic.Int64
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
			defer target.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch scenario {
				case "redirect":
					http.Redirect(w, r, target.URL, http.StatusFound)
				case "size":
					_, _ = io.WriteString(w, `{"data":"`+strings.Repeat("x", observerMaxResponse)+`"}`)
				case "invalid-json":
					_, _ = io.WriteString(w, "not JSON")
				case "token-echo":
					_ = json.NewEncoder(w).Encode(map[string]string{"token": r.Header.Get("X-SLURM-USER-TOKEN")})
				case "timeout":
					<-r.Context().Done()
				case "not-found":
					w.WriteHeader(404)
					_, _ = io.WriteString(w, "internal diagnostic must not leak")
				}
			}))
			defer upstream.Close()
			cfg := observerFixtureConfig(material)
			cfg.UpstreamURL = upstream.URL
			cfg.RequestTimeout = "100ms"
			h := newObserverHandler(cfg, []byte(strings.Repeat("k", 32)))
			defer h.client.CloseIdleConnections()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, observerAuthorizedRequest(t, material, http.MethodGet, "/slurm/v0.0.45/job/7", nil))
			expected := 502
			if scenario == "not-found" {
				expected = 404
			}
			if w.Code != expected || w.Body.String() != `{"error":"observer request rejected"}` || redirected.Load() != 0 {
				t.Fatalf("scenario escaped boundary: status %d", w.Code)
			}
		})
	}
}

func TestObserverActualMTLSListenerRejectsMissingAndWrongCertificate(t *testing.T) {
	material := testTLS(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"jobs":[]}`) }))
	defer upstream.Close()
	port, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := port.Addr().String()
	_ = port.Close()
	cfg := observerFixtureConfig(material)
	cfg.Listen = address
	cfg.UpstreamURL = upstream.URL
	dir := t.TempDir()
	cfg.JWTKeyFile = filepath.Join(dir, "jwt.key")
	_ = os.WriteFile(cfg.JWTKeyFile, []byte(strings.Repeat("k", 32)), 0600)
	data, _ := yaml.Marshal(cfg)
	configPath := filepath.Join(dir, "observer.yaml")
	_ = os.WriteFile(configPath, data, 0600)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunObserver(ctx, configPath) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	clientTLSConfig, _ := clientTLS(material.files)
	client := newHTTPClient(clientTLSConfig, time.Second)
	defer client.CloseIdleConnections()
	eventually(t, func() bool {
		response, err := client.Get("https://" + address + "/slurm/v0.0.45/job/7")
		if err != nil {
			return false
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == 200
	})
	for _, certificates := range [][]tls.Certificate{nil, {testTLS(t).certificate}} {
		badClient := newHTTPClient(&tls.Config{RootCAs: material.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12, Certificates: certificates}, time.Second)
		response, err := badClient.Get("https://" + address + "/slurm/v0.0.45/job/7")
		if response != nil {
			_ = response.Body.Close()
		}
		badClient.CloseIdleConnections()
		if err == nil {
			t.Fatal("missing or foreign client certificate accepted")
		}
	}
}
