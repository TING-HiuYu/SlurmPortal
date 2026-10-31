package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	joblifecycle "github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/runtime"
	"gopkg.in/yaml.v3"
)

func cliConfig(t *testing.T, cfg joblifecycle.HookConfig) string {
	t.Helper()
	body, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hook.yaml")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLifecycleCLIUsageAndOperationalErrorsFailOpen(t *testing.T) {
	getenv := func(string) string { return "private-job-identity" }
	for _, args := range [][]string{nil, {"-event", "unknown"}, {"-event", "starting", "unexpected"}, {"-unknown"}} {
		var out bytes.Buffer
		if code := run(args, getenv, &out); code != 2 {
			t.Fatalf("bad usage should return 2: args=%v code=%d", args, code)
		}
	}
	for _, kind := range []string{"starting", "ending"} {
		var out bytes.Buffer
		code := run([]string{"-event", kind, "-config", "/nonexistent/lifecycle-config"}, getenv, &out)
		if code != 0 {
			t.Fatalf("operational configuration failure should not block job: %d", code)
		}
		if strings.Contains(out.String(), "private-job-identity") || strings.Contains(out.String(), "/nonexistent") {
			t.Fatal("operational output leaked identity or configuration path")
		}
	}
	tokens, err := eventbus.NewLocalTokens()
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(t.TempDir(), "write-token")
	if err = os.WriteFile(tokenPath, []byte(tokens.ReadWrite), 0600); err != nil {
		t.Fatal(err)
	}
	path := cliConfig(t, joblifecycle.HookConfig{URL: "https://127.0.0.1:1", WriteTokenFile: tokenPath, TLS: jobnotify.TLSFiles{CertFile: "missing-certificate", KeyFile: "missing-key"}, Timeout: "10ms"})
	for _, kind := range []string{"starting", "ending"} {
		var out bytes.Buffer
		lookup := func(name string) string {
			if name == "SLURM_JOB_ID" {
				return "42"
			}
			if name == "SLURM_JOB_SLUID" {
				return "s0123456789ABC"
			}
			return ""
		}
		if code := run([]string{"-event", kind, "-config", path}, lookup, &out); code != 0 {
			t.Fatalf("network/TLS failure blocked job: %d", code)
		}
		if strings.Contains(out.String(), tokens.ReadWrite) || strings.Contains(out.String(), "missing-certificate") {
			t.Fatal("credential detail leaked")
		}
	}
}

func TestLifecycleCLISendsEveryStartAndEndWithoutRuntimeThreshold(t *testing.T) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated CLI CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
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
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})) {
		t.Fatal("append isolated CA")
	}
	var starts, ends atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if len(r.TLS.VerifiedChains) == 0 {
			t.Error("CLI handoff missing verified client")
		}
		switch r.URL.Path {
		case joblifecycle.StartingPath:
			starts.Add(1)
		case joblifecycle.EndingPath:
			ends.Add(1)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(202)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	tokens, err := eventbus.NewLocalTokens()
	if err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(directory, "write-token")
	if err := os.WriteFile(tokenPath, []byte(tokens.ReadWrite), 0600); err != nil {
		t.Fatal(err)
	}
	path := cliConfig(t, joblifecycle.HookConfig{URL: server.URL, TLS: files, WriteTokenFile: tokenPath, Timeout: "1s"})
	for _, duration := range []string{"0", "1", "59", "60", "61"} {
		for _, kind := range []string{"starting", "ending"} {
			var out bytes.Buffer
			getenv := func(name string) string {
				switch name {
				case "SLURM_JOB_ID":
					return "42"
				case "SLURM_JOB_SLUID":
					return "s0123456789ABC"
				case "SLURM_JOB_START_TIME":
					return "1000"
				case "SLURM_JOB_END_TIME":
					return duration
				}
				return ""
			}
			if code := run([]string{"-event", kind, "-config", path}, getenv, &out); code != 0 || out.Len() != 0 {
				t.Fatalf("duration %s kind %s was filtered or failed: code=%d output=%q", duration, kind, code, out.String())
			}
		}
	}
	if starts.Load() != 5 || ends.Load() != 5 {
		t.Fatalf("short-job lifecycle callbacks missing: starting=%d ending=%d", starts.Load(), ends.Load())
	}
}
