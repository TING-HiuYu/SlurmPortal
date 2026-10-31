package joblifecycle

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/eventbus"
	"github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/jobnotify"
	"gopkg.in/yaml.v3"
)

type hookTestTLS struct {
	files       jobnotify.TLSFiles
	certificate tls.Certificate
	pool        *x509.CertPool
}

func newHookTestTLS(t *testing.T) hookTestTLS {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "lifecycle hook isolated CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "lifecycle hook isolated peer"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	files := jobnotify.TLSFiles{CertFile: filepath.Join(directory, "peer.crt"), KeyFile: filepath.Join(directory, "peer.key"), CAFile: filepath.Join(directory, "ca.crt"), ServerName: "127.0.0.1"}
	for path, body := range map[string][]byte{files.CertFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), files.KeyFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), files.CAFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})} {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	certificate, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})) {
		t.Fatal("append isolated CA")
	}
	return hookTestTLS{files: files, certificate: certificate, pool: pool}
}

func hookTestServer(t *testing.T, material hookTestTLS, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{material.certificate}, ClientCAs: material.pool, ClientAuth: tls.RequireAndVerifyClientCert}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func hookTestConfig(t *testing.T, material hookTestTLS, url string) HookConfig {
	t.Helper()
	tokens, err := eventbus.NewLocalTokens()
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(t.TempDir(), "write-token")
	if err := os.WriteFile(tokenPath, []byte(tokens.ReadWrite+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return HookConfig{URL: url, TLS: material.files, WriteTokenFile: tokenPath, Timeout: "200ms"}
}

func hookConfigPath(t *testing.T, cfg HookConfig, mode os.FileMode) string {
	t.Helper()
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hook.yaml")
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLifecycleHookSuccessfulMutualTLSHandoff(t *testing.T) {
	material := newHookTestTLS(t)
	received := make(chan string, 2)
	server := hookTestServer(t, material, func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			t.Error("hook did not present verified client certificate")
		}
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("invalid handoff headers")
		}
		var c jobnotify.Callback
		if json.NewDecoder(r.Body).Decode(&c) != nil || c.JobID != 42 || c.SLUID != "s0123456789ABC" {
			t.Errorf("invalid callback %+v", c)
		}
		received <- r.URL.Path
		w.WriteHeader(http.StatusAccepted)
	})
	cfg := hookTestConfig(t, material, server.URL)
	for _, kind := range []string{StartingType, EndingType} {
		if err := SendHook(context.Background(), cfg, kind, jobnotify.Callback{JobID: 42, SLUID: "S0123456789abc"}); err != nil {
			t.Fatal(err)
		}
	}
	if a, b := <-received, <-received; a != StartingPath || b != EndingPath {
		t.Fatalf("wrong lifecycle paths %s %s", a, b)
	}
}

func TestLifecycleHookWaitForAcceptanceIsBounded(t *testing.T) {
	material := newHookTestTLS(t)
	server := hookTestServer(t, material, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() })
	cfg := hookTestConfig(t, material, server.URL)
	cfg.Timeout = "40ms"
	started := time.Now()
	if err := SendHook(context.Background(), cfg, StartingType, jobnotify.Callback{JobID: 42, SLUID: "s0123456789ABC"}); err == nil {
		t.Fatal("unconfirmed handoff reported success")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("hook exceeded scheduling budget: %v", elapsed)
	}
}

func TestLifecycleHookRejectsResponseOtherThanAccepted(t *testing.T) {
	material := newHookTestTLS(t)
	server := hookTestServer(t, material, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	cfg := hookTestConfig(t, material, server.URL)
	if SendHook(context.Background(), cfg, EndingType, jobnotify.Callback{JobID: 42, SLUID: "s0123456789ABC"}) == nil {
		t.Fatal("200 was mistaken for queue acceptance")
	}
}

func TestLifecycleHookWriteTokenMustBePrivateRegularFile(t *testing.T) {
	material := newHookTestTLS(t)
	var received atomic.Int64
	server := hookTestServer(t, material, func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(202)
	})
	for _, mode := range []os.FileMode{0644, 0640, 0440} {
		cfg := hookTestConfig(t, material, server.URL)
		if err := os.Chmod(cfg.WriteTokenFile, mode); err != nil {
			t.Fatal(err)
		}
		if SendHook(context.Background(), cfg, StartingType, jobnotify.Callback{JobID: 42, SLUID: "s0123456789ABC"}) == nil {
			t.Fatalf("credential mode %o accepted", mode)
		}
	}
	cfg := hookTestConfig(t, material, server.URL)
	target := cfg.WriteTokenFile
	cfg.WriteTokenFile = filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(target, cfg.WriteTokenFile); err != nil {
		t.Fatal(err)
	}
	if SendHook(context.Background(), cfg, StartingType, jobnotify.Callback{JobID: 42, SLUID: "s0123456789ABC"}) == nil {
		t.Fatal("symlink write credential accepted")
	}
	if received.Load() != 0 {
		t.Fatal("invalid credentials reached network")
	}
}

func TestLifecycleHookConfigAndEndpointValidation(t *testing.T) {
	material := newHookTestTLS(t)
	base := hookTestConfig(t, material, "https://127.0.0.1")
	for _, endpoint := range []string{"http://127.0.0.1", "https://user:secret@127.0.0.1", "https://127.0.0.1/other", "https://127.0.0.1?query=value", "https://127.0.0.1?", "https://127.0.0.1/#fragment", "https:opaque"} {
		cfg := base
		cfg.URL = endpoint
		if _, err := LoadHookConfig(hookConfigPath(t, cfg, 0600)); err == nil {
			t.Errorf("invalid hook origin accepted %q", endpoint)
		}
	}
	for _, timeout := range []string{"0s", "-1s", "3s", "invalid"} {
		cfg := base
		cfg.Timeout = timeout
		if _, err := LoadHookConfig(hookConfigPath(t, cfg, 0600)); err == nil {
			t.Errorf("invalid timeout accepted %q", timeout)
		}
	}
	base.Timeout = ""
	loaded, err := LoadHookConfig(hookConfigPath(t, base, 0600))
	if err != nil || loaded.Timeout != "1s" {
		t.Fatalf("missing default timeout %+v %v", loaded, err)
	}
	if _, err := LoadHookConfig(hookConfigPath(t, base, 0644)); err == nil {
		t.Fatal("public hook configuration accepted")
	}
}

func TestLifecycleHookRejectsUntrustedTLSAndInvalidIdentity(t *testing.T) {
	material := newHookTestTLS(t)
	var requests atomic.Int64
	server := hookTestServer(t, material, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(202) })
	cfg := hookTestConfig(t, material, server.URL)
	cfg.TLS.ServerName = "wrong.invalid"
	if SendHook(context.Background(), cfg, StartingType, jobnotify.Callback{JobID: 42, SLUID: "s0123456789ABC"}) == nil {
		t.Fatal("server name mismatch accepted")
	}
	for _, c := range []jobnotify.Callback{{JobID: 0, SLUID: "s0123456789ABC"}, {JobID: 42, SLUID: "bad"}} {
		if SendHook(context.Background(), cfg, StartingType, c) == nil {
			t.Fatal("invalid execution identity accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("untrusted TLS or invalid identity reached application")
	}
}
