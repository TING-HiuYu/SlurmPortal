package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/gateway"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func testConfiguration(t *testing.T) (gateway.Config, string) {
	t.Helper()
	dir := t.TempDir()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "gateway.example.invalid"}, DNSNames: []string{"gateway.example.invalid"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, publicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	certificatePath, keyPath := filepath.Join(dir, "gateway.crt.pem"), filepath.Join(dir, "gateway.key.pem")
	if err = os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}
	sshKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	userCA := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshKey)))
	userCAPath := filepath.Join(dir, "user-ca-list.pub")
	if err = os.WriteFile(userCAPath, []byte(userCA+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := gateway.Config{GatewayID: "test-gateway", ClusterID: "source", Listen: "127.0.0.1:0", CallbackListen: "127.0.0.1:0", TLSCertificate: certificatePath, TLSKey: keyPath, AllowedOrigins: []string{"https://console.example.invalid"}, TicketPublicKeys: map[string]string{"ticket-key": base64.RawURLEncoding.EncodeToString(publicKey)}, UserCAKeysFile: userCAPath, Clusters: []gateway.Cluster{{ID: "source", SSHAddress: "127.0.0.1:22"}}, StateDirectory: filepath.Join(dir, "state-must-not-be-created")}
	configPath := filepath.Join(dir, "gateway.yaml")
	writeConfiguration(t, configPath, cfg)
	return cfg, configPath
}

func writeConfiguration(t *testing.T, path string, cfg gateway.Config) {
	t.Helper()
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckConfigurationDoesNotStartOrCreateState(t *testing.T) {
	cfg, path := testConfiguration(t)
	var output bytes.Buffer
	if err := run(context.Background(), []string{"-config", path, "-check-config"}, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.StateDirectory); !os.IsNotExist(err) {
		t.Fatalf("configuration check touched the state directory: %v", err)
	}
	if !strings.Contains(output.String(), "no listeners started") {
		t.Fatalf("unexpected check output: %s", output.String())
	}
}

func TestCheckConfigurationAcceptsAuthoritativePublicCABundleSymlink(t *testing.T) {
	cfg, path := testConfiguration(t)
	bundleDirectory := filepath.Join(filepath.Dir(path), "published-cas")
	versionDirectory := filepath.Join(bundleDirectory, "v1")
	if err := os.MkdirAll(versionDirectory, 0755); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(cfg.UserCAKeysFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDirectory, "trusted-user-ca-keys"), bundle, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("v1", filepath.Join(bundleDirectory, "current")); err != nil {
		t.Fatal(err)
	}
	cfg.UserCAKeysFile = filepath.Join(bundleDirectory, "current", "trusted-user-ca-keys")
	cfg.UserCAKeys = []string{"invalid legacy inline key must be ignored"}
	writeConfiguration(t, path, cfg)
	if err := run(context.Background(), []string{"-config", path, "-check-config"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("0644 authoritative CA bundle through current/version symlink rejected: %v", err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil || loaded.FileRoot != "/HOME" {
		t.Fatalf("default shared file root changed: root=%q err=%v", loaded.FileRoot, err)
	}
	if _, err := os.Stat(cfg.StateDirectory); !os.IsNotExist(err) {
		t.Fatalf("public CA configuration check touched state: %v", err)
	}
}

func TestPrivateKeyPermissionsAndSymlinksAreRejected(t *testing.T) {
	cfg, _ := testConfiguration(t)
	if _, err := gatewayTLS(cfg); err != nil {
		t.Fatalf("0644 public certificate and 0600 private key rejected: %v", err)
	}
	if err := os.Chmod(cfg.TLSKey, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayTLS(cfg); err == nil || !strings.Contains(err.Error(), "0600 or 0400") {
		t.Fatalf("public private key accepted: %v", err)
	}
	if err := os.Chmod(cfg.TLSKey, 0400); err != nil {
		t.Fatal(err)
	}
	if _, err := gatewayTLS(cfg); err != nil {
		t.Fatalf("0400 private key rejected: %v", err)
	}
	link := filepath.Join(filepath.Dir(cfg.TLSKey), "linked.key")
	if err := os.Symlink(cfg.TLSKey, link); err != nil {
		t.Fatal(err)
	}
	cfg.TLSKey = link
	if _, err := gatewayTLS(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink private key accepted: %v", err)
	}
}

func TestRequiredTLSAndMasterMTLSAreCheckedBeforeStartup(t *testing.T) {
	cfg, path := testConfiguration(t)
	missing := cfg
	missing.TLSCertificate = ""
	writeConfiguration(t, path, missing)
	if _, err := gatewayTLS(missing); err == nil || !strings.Contains(err.Error(), "public HTTPS TLS") {
		t.Fatalf("missing public certificate accepted: %v", err)
	}
	missing = cfg
	missing.MasterOrigin = "https://sync.example.invalid"
	if _, err := gatewayTLS(missing); err == nil || !strings.Contains(err.Error(), "master mTLS") {
		t.Fatalf("master without mTLS credentials accepted: %v", err)
	}
	missing.MasterClientCertificate = cfg.TLSCertificate
	missing.MasterClientKey = cfg.TLSKey
	if _, err := gatewayTLS(missing); err != nil {
		t.Fatalf("valid master mTLS pair rejected: %v", err)
	}
	missing.MasterOrigin = ""
	if _, err := gatewayTLS(missing); err == nil || !strings.Contains(err.Error(), "require master_origin") {
		t.Fatalf("orphan master credentials accepted: %v", err)
	}
}

func TestConfigurationMustBePrivateAndRejectUnknownArguments(t *testing.T) {
	_, path := testConfiguration(t)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-config", path, "-check-config"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "0600 or 0400") {
		t.Fatalf("nonprivate configuration accepted: %v", err)
	}
	if err := run(context.Background(), []string{"-config", ""}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("empty configuration path accepted: %v", err)
	}
	if err := run(context.Background(), []string{"-check-config", "unexpected"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unexpected positional argument accepted: %v", err)
	}
}

func TestCheckConfigurationRejectsUnknownYAMLFields(t *testing.T) {
	_, path := testConfiguration(t)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("unknown_gateway_setting: true\n"); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"-config", path, "-check-config"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unknown_gateway_setting") {
		t.Fatalf("unknown YAML setting accepted: %v", err)
	}
}

func TestServerLeavesStreamDeadlinesToHandlers(t *testing.T) {
	server := gatewayServer(context.Background(), http.NotFoundHandler())
	if server.ReadHeaderTimeout != 10*time.Second || server.IdleTimeout != 60*time.Second || server.MaxHeaderBytes != 32<<10 {
		t.Fatalf("header/idle safeguards changed: %+v", server)
	}
	if server.ReadTimeout != 0 || server.WriteTimeout != 0 {
		t.Fatalf("global deadlines would interrupt terminal/download streams: read=%v write=%v", server.ReadTimeout, server.WriteTimeout)
	}
}
