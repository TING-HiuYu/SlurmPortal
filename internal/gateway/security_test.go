package gateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

func securityConfig(t *testing.T) (Config, ed25519.PrivateKey) {
	t.Helper()
	public, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	ca, _, _ := ed25519.GenerateKey(rand.Reader)
	caSSH, _ := ssh.NewPublicKey(ca)
	pin := string(ssh.MarshalAuthorizedKey(caSSH))
	dir := t.TempDir()
	if e = os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	cfg := Config{GatewayID: "fixture-gateway", ClusterID: "source", AllowedOrigins: []string{"https://console.fixture.invalid"}, TicketPublicKeys: map[string]string{"current": base64.RawURLEncoding.EncodeToString(public)}, UserCAKeys: []string{pin}, Clusters: []Cluster{{ID: "source", SSHAddress: "127.0.0.1:22", HostKeys: []string{pin}}, {ID: "destination", SSHAddress: "target.fixture.invalid:22", HostKeys: []string{pin}}}, StateDirectory: dir}
	if e = cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	return cfg, private
}
func securityToken(t *testing.T, key ed25519.PrivateKey, c Claims, kid string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	token.Header["kid"] = kid
	s, e := token.SignedString(key)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func securityClaims(now time.Time) Claims {
	return Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "go-ldap-admin", Audience: jwt.ClaimStrings{"fixture-gateway"}, Subject: "42", ID: uuid.NewString(), IssuedAt: jwt.NewNumericDate(now.Add(-time.Second)), NotBefore: jwt.NewNumericDate(now.Add(-time.Second)), ExpiresAt: jwt.NewNumericDate(now.Add(299 * time.Second))}, Username: "user", ClusterID: "source", Scopes: []string{"session:create", "terminal", "sftp", "transfer", "transfer:read", "diagnostics"}, DestinationClusterIDs: []string{"destination"}}
}
func TestDedicatedTicketsRejectWrongIdentityAlgorithmAndEntireInterval(t *testing.T) {
	cfg, key := securityConfig(t)
	now := time.Now().Truncate(time.Second)
	valid := securityClaims(now)
	if _, e := cfg.VerifyTicket(securityToken(t, key, valid, "current"), "sftp", now); e != nil {
		t.Fatal(e)
	}
	cases := map[string]func(*Claims){"issuer": func(c *Claims) { c.Issuer = "app" }, "audience": func(c *Claims) { c.Audience = jwt.ClaimStrings{"other"} }, "multi-aud": func(c *Claims) { c.Audience = jwt.ClaimStrings{"fixture-gateway", "other"} }, "subject": func(c *Claims) { c.Subject = "../root" }, "username": func(c *Claims) { c.Username = "root;id" }, "cluster": func(c *Claims) { c.ClusterID = "destination" }, "missing-issued": func(c *Claims) { c.IssuedAt = nil }, "missing-nbf": func(c *Claims) { c.NotBefore = nil }, "expired": func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(now) }, "future-nbf": func(c *Claims) { c.NotBefore = jwt.NewNumericDate(now.Add(time.Second)) }, "wide-issued": func(c *Claims) { c.IssuedAt = jwt.NewNumericDate(now.Add(-2 * time.Second)) }, "wide-nbf": func(c *Claims) { c.NotBefore = jwt.NewNumericDate(now.Add(-2 * time.Second)) }, "missing-scope": func(c *Claims) { c.Scopes = []string{"terminal"} }, "duplicate-scope": func(c *Claims) { c.Scopes = []string{"sftp", "sftp"} }, "unknown-scope": func(c *Claims) { c.Scopes = []string{"sftp", "admin"} }, "empty-jti": func(c *Claims) { c.ID = "" }}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid
			c.Scopes = append([]string{}, c.Scopes...)
			change(&c)
			if _, e := cfg.VerifyTicket(securityToken(t, key, c, "current"), "sftp", now); e == nil {
				t.Fatal("accepted invalid ticket")
			}
		})
	}
	if _, e := cfg.VerifyTicket(securityToken(t, key, valid, "unknown"), "sftp", now); e == nil {
		t.Fatal("accepted unknown kid")
	}
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, valid).SignedString([]byte("application-jwt-key"))
	if _, e := cfg.VerifyTicket(hs, "sftp", now); e == nil {
		t.Fatal("accepted ordinary app JWT")
	}
	pub2, key2, _ := ed25519.GenerateKey(rand.Reader)
	cfg.TicketPublicKeys["next"] = base64.RawURLEncoding.EncodeToString(pub2)
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	if _, e := cfg.VerifyTicket(securityToken(t, key2, valid, "next"), "sftp", now); e != nil {
		t.Fatal(e)
	}
}
func TestRunnerShellPreservesLiteralPathsAndWritesExactPartialStatistics(t *testing.T) {
	cfg, _ := securityConfig(t)
	dir := filepath.Join(t.TempDir(), "runner ' space")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	bin := t.TempDir()
	fake := filepath.Join(bin, "rsync")
	argsFile := filepath.Join(bin, "arguments")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %s\nprintf 'Total bytes sent: 1,000,000\\nTotal bytes received: 250\\nTotal transferred file size: 2,000,000 bytes\\n'\nexit 23\n", quote(argsFile))
	if e := os.WriteFile(fake, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(bin, "curl"), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	danger := filepath.Join(dir, `data'; touch INJECTED; echo '`)
	task := &Transfer{TransferID: uuid.NewString(), Username: "user", RemoteDirectory: dir, SourcePaths: []string{danger}, DestinationPath: `/home/user/dest'; touch BAD; echo '`}
	files := runnerFiles(cfg, task, cfg.Clusters[1], credential{"temporary-private-key", "temporary-cert"}, "synthetic-local-capability")
	if _, exists := files["known_hosts"]; exists {
		t.Fatal("unexpected host identity file")
	}
	for _, setting := range []string{"StrictHostKeyChecking no", "CheckHostIP no", "UserKnownHostsFile /dev/null", "GlobalKnownHostsFile /dev/null"} {
		if !strings.Contains(files["ssh_config"], setting) {
			t.Fatalf("missing explicitly selected SSH setting %s", setting)
		}
	}
	for name, value := range files {
		if name == "run.sh" {
			value = strings.ReplaceAll(value, "'/usr/bin/rsync'", quote(fake))
			value = strings.ReplaceAll(value, "if /usr/bin/curl --config", "if "+quote(filepath.Join(bin, "curl"))+" --config")
		}
		if e := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); e != nil {
			t.Fatal(e)
		}
	}
	cmd := exec.Command("/bin/sh", filepath.Join(dir, "run.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	if output, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("runner: %v %s", e, output)
	}
	raw, e := os.ReadFile(filepath.Join(dir, "exit.json"))
	if e != nil {
		t.Fatal(e)
	}
	var result completionInput
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatalf("invalid exit JSON %q: %v", raw, e)
	}
	if result.ExitCode != 23 || result.BytesSent == nil || *result.BytesSent != 1000000 || result.TotalBytes == nil || *result.TotalBytes != 2000000 {
		t.Fatalf("wrong statistics: %s", raw)
	}
	for _, name := range []string{"identity", "identity-cert.pub", "known_hosts", "ssh_config", "callback.conf", "run.sh"} {
		if _, e = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(e) {
			t.Fatalf("credential/script retained: %s", name)
		}
	}
	for _, name := range []string{"INJECTED", "BAD"} {
		if _, e = os.Stat(filepath.Join(dir, name)); !os.IsNotExist(e) {
			t.Fatal("shell injected")
		}
	}
	args, e := os.ReadFile(argsFile)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(args), danger) || !strings.Contains(string(args), "destination:"+task.DestinationPath) {
		t.Fatal("path changed through shell")
	}
	for _, option := range []string{"--checksum", "--append", "TCPRcvBuf", "TCPSndBuf", "--delete"} {
		if strings.Contains(string(args), option) {
			t.Fatalf("forbidden option %s", option)
		}
	}
	if !strings.Contains(string(args), "--partial-dir=.rsync-partial") || strings.Contains(string(args), "--secluded-args") || strings.Contains(string(args), "--old-args") {
		t.Fatal("missing safe transfer flags")
	}
}

func TestRunnerLargeLogPreservesRsyncExitAndTrailingStatistics(t *testing.T) {
	cfg, _ := securityConfig(t)
	dir, bin := t.TempDir(), t.TempDir()
	fakeRsync, fakeCurl := filepath.Join(bin, "rsync"), filepath.Join(bin, "curl")
	// Exceed both the 512-byte and 1024-byte interpretations of the former
	// ulimit -f 65536, then emit the trailer needed by completion reporting.
	output := "#!/bin/sh\nawk 'BEGIN {for (i=0; i<66560; i++) printf \"%01024d\\n\", 0}' || exit $?\nprintf 'Total bytes sent: 1,000,000\\nTotal bytes received: 250\\nTotal transferred file size: 2,000,000 bytes\\n'\nexit 23\n"
	if err := os.WriteFile(fakeRsync, []byte(output), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakeCurl, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	task := &Transfer{TransferID: uuid.NewString(), Username: "user", RemoteDirectory: dir, SourcePaths: []string{"/HOME/user/source"}, DestinationPath: "/HOME/user/destination"}
	files := runnerFiles(cfg, task, cfg.Clusters[1], credential{"temporary-private-key", "temporary-cert"}, "synthetic-local-capability")
	for name, value := range files {
		if name == "run.sh" {
			value = strings.ReplaceAll(value, "'/usr/bin/rsync'", quote(fakeRsync))
			value = strings.ReplaceAll(value, "if /usr/bin/curl --config", "if "+quote(fakeCurl)+" --config")
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "/bin/sh", filepath.Join(dir, "run.sh")).CombinedOutput(); err != nil {
		t.Fatalf("runner: %v %s", err, output)
	}
	info, err := os.Stat(filepath.Join(dir, "raw.log"))
	if err != nil || info.Size() <= 64<<20 {
		t.Fatalf("large raw log not retained: info=%v err=%v", info, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "exit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result completionInput
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("invalid completion JSON %q: %v", raw, err)
	}
	if result.ExitCode != 23 || result.BytesSent == nil || *result.BytesSent != 1000000 || result.BytesReceived == nil || *result.BytesReceived != 250 || result.TotalBytes == nil || *result.TotalBytes != 2000000 {
		t.Fatalf("large log lost rsync exit or trailing statistics: %s", raw)
	}
}

func insertSecurityTransfer(t *testing.T, g *Gateway) (*Transfer, string) {
	t.Helper()
	id := uuid.NewString()
	cap := opaque() + opaque()
	task := &Transfer{Version: 1, EventID: id, TransferID: id, GatewayID: g.cfg.GatewayID, ClusterID: g.cfg.ClusterID, UserID: "42", NumericUserID: 42, Username: "user", DestinationClusterID: "destination", SourcePaths: []string{"/home/user/data"}, DestinationPath: "/home/user/destination", Status: "running", StartedAt: time.Now().UTC().Add(-10 * time.Second), CallbackHash: hashCapability(cap), CallbackExpires: time.Now().Add(time.Hour)}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.transfers[id] = task
	if e := g.persist(task); e != nil {
		t.Fatal(e)
	}
	return task, cap
}
func callbackTest(g *Gateway, id, cap, remote string, value any) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(value)
	r := httptest.NewRequest("POST", "http://127.0.0.1:9444/v1/completions/"+id, bytes.NewReader(raw))
	r.RemoteAddr = remote
	r.Header.Set("Authorization", "Bearer "+cap)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.CallbackHandler().ServeHTTP(w, r)
	return w
}
func TestLocalCompletionCapabilitySurvivesRestartAndFreezesOutbox(t *testing.T) {
	cfg, _ := securityConfig(t)
	g, e := New(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	task, cap := insertSecurityTransfer(t, g)
	id := task.TransferID
	g.Close()
	g, e = New(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	sent, received, total := int64(1000000), int64(50), int64(3000000)
	input := completionInput{ExitCode: 23, StartedUnix: task.StartedAt.Unix(), FinishedUnix: task.StartedAt.Add(10 * time.Second).Unix(), BytesSent: &sent, BytesReceived: &received, TotalBytes: &total}
	for _, test := range []struct {
		cap, remote string
		status      int
	}{{cap, "203.0.113.1:123", 403}, {"wrong", "127.0.0.1:123", 403}, {cap, "127.0.0.1:123", 200}} {
		if w := callbackTest(g, id, test.cap, test.remote, input); w.Code != test.status {
			t.Fatalf("callback %d %s", w.Code, w.Body.String())
		}
	}
	g.mu.Lock()
	stored := *g.transfers[id]
	g.mu.Unlock()
	if stored.Status != "partial" || stored.AverageMBPerSecond == nil || *stored.AverageMBPerSecond != 0.100005 {
		t.Fatalf("bad result %+v", stored)
	}
	if callbackTest(g, id, cap, "127.0.0.1:123", input).Code != 200 {
		t.Fatal("idempotent callback failed")
	}
	input.ExitCode = 0
	if callbackTest(g, id, cap, "127.0.0.1:123", input).Code != 409 {
		t.Fatal("late conflicting completion changed result")
	}
	raw, e := os.ReadFile(filepath.Join(cfg.StateDirectory, id+".json"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(raw, []byte(cap)) {
		t.Fatal("completion secret persisted plaintext")
	}
	g.cancel()
	g.wg.Wait()
	g.ctx = context.Background()
	g.cfg.MasterOrigin = "https://sync.fixture.invalid"
	calls := 0
	g.masterClient = &http.Client{Transport: securityRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://sync.fixture.invalid/sync/v2/web-access/events" {
			t.Fatal("arbitrary outgoing target")
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(cap)) || bytes.Contains(body, []byte("callback_hash")) {
			t.Fatal("secret leaked into master event")
		}
		if calls == 1 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"code":503}`)), Header: http.Header{}}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"outcome":"smtp_accepted"}}`)), Header: http.Header{}}, nil
	})}
	g.flushOutbox()
	if g.transfers[id].Delivered {
		t.Fatal("503 consumed outbox")
	}
	g.flushOutbox()
	if !g.transfers[id].Delivered {
		t.Fatal("accepted event not consumed")
	}
	g.flushOutbox()
	if calls != 2 {
		t.Fatal("duplicate SMTP dispatch after durable acceptance")
	}
}

type securityRoundTripper func(*http.Request) (*http.Response, error)

func (f securityRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRegistrationEndpointAndPEMKeyComparison(t *testing.T) {
	cfg, _ := securityConfig(t)
	raw, e := x509.MarshalPKIXPublicKey(cfg.parsedKeys["current"])
	if e != nil {
		t.Fatal(e)
	}
	keyX := base64.RawURLEncoding.EncodeToString(cfg.parsedKeys["current"])
	cfg.TicketPublicKeys["current"] = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: raw}))
	if e = cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	cfg.MasterOrigin = "https://sync.fixture.invalid"
	sourceHost := "127.0.0.1"
	g := &Gateway{cfg: cfg, ctx: context.Background(), bootID: opaque()}
	g.masterClient = &http.Client{Transport: securityRoundTripper(func(req *http.Request) (*http.Response, error) {
		var input map[string]any
		if e := json.NewDecoder(req.Body).Decode(&input); e != nil {
			t.Fatal(e)
		}
		if input["gateway_id"] != cfg.GatewayID || input["instance_id"] != cfg.InstanceID || input["boot_id"] != g.bootID {
			t.Fatal("identity missing")
		}
		body, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"clusters": []map[string]any{{"id": "source", "enabled": true, "gateway_id": cfg.GatewayID, "ssh_host": sourceHost, "ssh_port": 22, "ssh_host_keys": cfg.Clusters[0].HostKeys}, {"id": "destination", "enabled": true, "gateway_id": "other-gateway", "address": "target.fixture.invalid", "ssh_host": "127.0.0.1", "ssh_port": 22, "ssh_host_keys": cfg.Clusters[1].HostKeys}}, "verification_keys": []map[string]string{{"kid": "current", "kty": "OKP", "crv": "Ed25519", "x": keyX}}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}, nil
	})}
	g.register()
	if v := g.configurationError(); v != "" {
		t.Fatalf("valid PEM key or publicdestination rejected %s", v)
	}
	sourceHost = "10.0.0.8"
	g.register()
	if v := g.configurationError(); v != "config_mismatch" {
		t.Fatalf("changed master SSH endpoint was silently ignored: %s", v)
	}
}

func TestCompletionMissingExitCannotBecomeSuccessAndMetadataBudgetStops(t *testing.T) {
	cfg, _ := securityConfig(t)
	g, e := New(context.Background(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer g.Close()
	task, cap := insertSecurityTransfer(t, g)
	value := map[string]any{"started_unix": task.StartedAt.Unix(), "finished_unix": time.Now().Unix(), "bytes_sent": nil, "bytes_received": nil, "total_bytes": nil}
	if response := callbackTest(g, task.TransferID, cap, "127.0.0.1:123", value); response.Code != 400 {
		t.Fatalf("missing exit became success: %d", response.Code)
	}
	value["exit_code"] = nil
	if response := callbackTest(g, task.TransferID, cap, "127.0.0.1:123", value); response.Code != 400 {
		t.Fatal("null exit accepted")
	}
	reader := &metadataReader{source: strings.NewReader("metadata-too-large"), remaining: 8}
	result, e := io.ReadAll(reader)
	if e == nil || len(result) != 8 {
		t.Fatalf("metadata budget not enforced: %q %v", result, e)
	}
}

func TestUserCABundleAuthoritativePublicSymlinkAndLiveRotation(t *testing.T) {
	cfg, _ := securityConfig(t)
	dir := t.TempDir()
	public1, private1, _ := ed25519.GenerateKey(rand.Reader)
	public2, private2, _ := ed25519.GenerateKey(rand.Reader)
	sshPub1, _ := ssh.NewPublicKey(public1)
	sshPub2, _ := ssh.NewPublicKey(public2)
	for name, key := range map[string]ssh.PublicKey{"v1": sshPub1, "v2": sshPub2} {
		if e := os.WriteFile(filepath.Join(dir, name), append([]byte("# slave public CA bundle\n\n"), ssh.MarshalAuthorizedKey(key)...), 0644); e != nil {
			t.Fatal(e)
		}
	}
	current := filepath.Join(dir, "current")
	if e := os.Symlink("v1", current); e != nil {
		t.Fatal(e)
	}
	cfg.UserCAKeysFile = current
	cfg.UserCAKeys = []string{"invalid legacy pin must not be used"}
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	g := &Gateway{cfg: cfg}
	credentialFor := func(private ed25519.PrivateKey) credential {
		_, key, _ := ed25519.GenerateKey(rand.Reader)
		signer, _ := ssh.NewSignerFromKey(key)
		ca, _ := ssh.NewSignerFromKey(private)
		now := time.Now()
		cert := &ssh.Certificate{Key: signer.PublicKey(), CertType: ssh.UserCert, Serial: 7, KeyId: "web-access/source/access/user:42/7", ValidPrincipals: []string{"user"}, ValidAfter: uint64(now.Add(-time.Second).Unix()), ValidBefore: uint64(now.Add(4 * time.Minute).Unix()), Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}}}
		if e := cert.SignCert(rand.Reader, ca); e != nil {
			t.Fatal(e)
		}
		encoded, e := x509.MarshalPKCS8PrivateKey(key)
		if e != nil {
			t.Fatal(e)
		}
		return credential{PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})), Certificate: string(ssh.MarshalAuthorizedKey(cert))}
	}
	old, newer := credentialFor(private1), credentialFor(private2)
	if _, e := g.signer(old, "user"); e != nil {
		t.Fatal(e)
	}
	if _, e := g.signer(newer, "user"); e == nil {
		t.Fatal("unpublished CA accepted")
	}
	next := filepath.Join(dir, "next")
	if e := os.Symlink("v2", next); e != nil {
		t.Fatal(e)
	}
	if e := os.Rename(next, current); e != nil {
		t.Fatal(e)
	}
	if _, e := g.signer(newer, "user"); e != nil {
		t.Fatalf("published rotation not reloaded: %v", e)
	}
	if _, e := g.signer(old, "user"); e == nil {
		t.Fatal("removed CA remained trusted")
	}
	if e := os.Chmod(filepath.Join(dir, "v2"), 0666); e != nil {
		t.Fatal(e)
	}
	if _, e := g.signer(newer, "user"); e == nil {
		t.Fatal("writable CA bundle accepted")
	}
	if e := os.Remove(current); e != nil {
		t.Fatal(e)
	}
	cfg.UserCAKeys = []string{string(ssh.MarshalAuthorizedKey(sshPub2))}
	if _, e := cfg.trustedUserCAs(); e == nil {
		t.Fatal("missing authoritative file fell back to inline")
	}
}
