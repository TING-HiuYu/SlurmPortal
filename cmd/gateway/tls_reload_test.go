package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type reloadTestLog chan string

func (log reloadTestLog) Write(raw []byte) (int, error) { log <- string(raw); return len(raw), nil }

func (log reloadTestLog) next(t *testing.T, wanted string) {
	t.Helper()
	select {
	case line := <-log:
		if !strings.Contains(line, wanted) || strings.Contains(line, "PRIVATE KEY") {
			t.Fatalf("unexpected TLS reload log: %s", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("TLS reload did not finish")
	}
}

func replacementTLSPair(t *testing.T, serial int64, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "gateway.example.invalid"}, DNSNames: []string{"gateway.example.invalid"}, NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, public, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
}

func replaceTLSFiles(t *testing.T, certificatePath, keyPath string, certificate, key []byte) {
	t.Helper()
	for _, file := range []struct {
		path string
		raw  []byte
		mode os.FileMode
	}{{certificatePath, certificate, 0644}, {keyPath, key, 0600}} {
		if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(file.path, file.raw, file.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHTTPSReloadChangesRealTLSAndPreservesExistingStreams(t *testing.T) {
	cfg, _ := testConfiguration(t)
	initial, err := keyPair(cfg.TLSCertificate, cfg.TLSKey)
	if err != nil {
		t.Fatal(err)
	}
	oldPEM, err := os.ReadFile(cfg.TLSCertificate)
	if err != nil {
		t.Fatal(err)
	}
	nextPEM, nextKey := replacementTLSPair(t, 2, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(oldPEM) || !roots.AppendCertsFromPEM(nextPEM) {
		t.Fatal("fixture trust certificates are invalid")
	}
	identity := newHTTPSIdentity(cfg.TLSCertificate, cfg.TLSKey, initial)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reloadSignals := make(chan os.Signal, 1)
	log := make(reloadTestLog, 16)
	reloadsStopped := make(chan struct{})
	go func() { defer close(reloadsStopped); identity.reloadOnSignals(ctx, reloadSignals, log) }()
	defer func() { cancel(); <-reloadsStopped }()

	streamStarted, releaseStream := make(chan struct{}), make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(releaseStream) }) }
	defer unblock()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/terminal":
			conn, e := websocket.Accept(w, r, nil)
			if e != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			for {
				kind, raw, e := conn.Read(ctx)
				if e != nil || conn.Write(ctx, kind, raw) != nil {
					return
				}
			}
		case "/download":
			_, _ = fmt.Fprintln(w, "download-before-reload")
			w.(http.Flusher).Flush()
			close(streamStarted)
			select {
			case <-releaseStream:
				_, _ = fmt.Fprintln(w, "download-after-reload")
			case <-ctx.Done():
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: identity.getCertificate}
	server.StartTLS()
	defer func() { unblock(); server.Close() }()
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: "gateway.example.invalid", RootCAs: roots}, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	readIdentity := func(t *testing.T, serial int64) [32]byte {
		t.Helper()
		response, e := client.Get(server.URL)
		if e != nil {
			t.Fatal(e)
		}
		defer func() { _ = response.Body.Close() }()
		leaf := response.TLS.PeerCertificates[0]
		if leaf.SerialNumber.Int64() != serial {
			t.Fatalf("TLS handshake served serial %s, wanted %d", leaf.SerialNumber, serial)
		}
		return sha256.Sum256(leaf.Raw)
	}
	before := readIdentity(t, 1)
	terminal, _, err := websocket.Dial(ctx, strings.Replace(server.URL, "https:", "wss:", 1)+"/terminal", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = terminal.CloseNow() }()
	echo := func(wanted string) {
		t.Helper()
		if e := terminal.Write(ctx, websocket.MessageBinary, []byte(wanted)); e != nil {
			t.Fatal(e)
		}
		_, raw, e := terminal.Read(ctx)
		if e != nil || string(raw) != wanted {
			t.Fatalf("existing terminal disconnected: %q %v", raw, e)
		}
	}
	echo("terminal-before-reload")
	download, err := client.Get(server.URL + "/download")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = download.Body.Close() }()
	<-streamStarted
	reader := bufio.NewReader(download.Body)
	if line, e := reader.ReadString('\n'); e != nil || line != "download-before-reload\n" {
		t.Fatalf("download did not begin: %q %v", line, e)
	}
	replaceTLSFiles(t, cfg.TLSCertificate, cfg.TLSKey, nextPEM, nextKey)
	reloadSignals <- syscall.SIGHUP
	log.next(t, "certificate reloaded")
	after := readIdentity(t, 2)
	if after == before {
		t.Fatal("new TLS handshakes still use the old certificate fingerprint")
	}
	echo("terminal-after-reload")

	_, wrongKey := replacementTLSPair(t, 3, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	expiredPEM, expiredKey := replacementTLSPair(t, 4, time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	futurePEM, futureKey := replacementTLSPair(t, 5, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	for name, corrupt := range map[string]func() error{
		"mismatched pair":             func() error { return os.WriteFile(cfg.TLSKey, wrongKey, 0600) },
		"malformed certificate":       func() error { return os.WriteFile(cfg.TLSCertificate, []byte("not a PEM certificate"), 0644) },
		"public private key":          func() error { return os.Chmod(cfg.TLSKey, 0644) },
		"writable public certificate": func() error { return os.Chmod(cfg.TLSCertificate, 0666) },
		"symlink private key": func() error {
			copy := filepath.Join(filepath.Dir(cfg.TLSKey), "key-copy")
			if e := os.WriteFile(copy, nextKey, 0600); e != nil {
				return e
			}
			if e := os.Remove(cfg.TLSKey); e != nil {
				return e
			}
			return os.Symlink(copy, cfg.TLSKey)
		},
		"symlink public certificate": func() error {
			copy := filepath.Join(filepath.Dir(cfg.TLSCertificate), "certificate-copy")
			if e := os.WriteFile(copy, nextPEM, 0644); e != nil {
				return e
			}
			if e := os.Remove(cfg.TLSCertificate); e != nil {
				return e
			}
			return os.Symlink(copy, cfg.TLSCertificate)
		},
		"expired certificate": func() error { replaceTLSFiles(t, cfg.TLSCertificate, cfg.TLSKey, expiredPEM, expiredKey); return nil },
		"future certificate":  func() error { replaceTLSFiles(t, cfg.TLSCertificate, cfg.TLSKey, futurePEM, futureKey); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			replaceTLSFiles(t, cfg.TLSCertificate, cfg.TLSKey, nextPEM, nextKey)
			if e := corrupt(); e != nil {
				t.Fatal(e)
			}
			reloadSignals <- syscall.SIGHUP
			log.next(t, "reload rejected")
			if readIdentity(t, 2) != after {
				t.Fatal("failed reload replaced the working HTTPS identity")
			}
			echo("terminal-survives-rejected-reload")
		})
	}
	unblock()
	if line, e := reader.ReadString('\n'); e != nil || line != "download-after-reload\n" {
		t.Fatalf("in-progress download interrupted by TLS reload: %q %v", line, e)
	}
}
