package gateway

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const gatewayTestOrigin = "https://console.fixture.invalid"

type gatewayTestDimensions struct{ cols, rows uint32 }
type gatewayTestSSH struct {
	listener         net.Listener
	home             string
	homes            map[string]string
	host, ca         ssh.Signer
	mu               sync.Mutex
	connections      map[net.Conn]bool
	wg               sync.WaitGroup
	dimensions       chan gatewayTestDimensions
	accepted         atomic.Int64
	acceptedTCP      atomic.Int64
	userAuthAttempts atomic.Int64
	authGate         <-chan struct{}
	authEntered      chan struct{}
	stopped          chan struct{}
	ignoreSFTPEOF    bool
	ignoredSFTPEOF   chan struct{}
	stallNextSFTP    *gatewayTestSFTPFault
	allowRunnerExec  bool
	runnerCommands   []string
}

type gatewayTestSFTPFault struct {
	blocked chan struct{}
	closed  chan struct{}
	notify  sync.Once
}

// Preserve the real INIT/OPEN exchange, but intentionally withhold DATA so a
// log ReadAt blocks on a genuine SSH/SFTP channel until that channel is closed.
type gatewayStalledSFTP struct {
	ssh.Channel
	mu      sync.Mutex
	pending []byte
	fault   *gatewayTestSFTPFault
}

func (w *gatewayStalledSFTP) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, data...)
	if len(w.pending) > 1<<20 {
		return 0, io.ErrShortBuffer
	}
	for len(w.pending) >= 4 {
		length := int(binary.BigEndian.Uint32(w.pending[:4])) + 4
		if length < 5 || length > 1<<20 {
			return 0, io.ErrUnexpectedEOF
		}
		if len(w.pending) < length {
			break
		}
		packet := w.pending[:length]
		if packet[4] == 103 { // SSH_FXP_DATA (SFTP v3).
			w.fault.notify.Do(func() { close(w.fault.blocked) })
		} else if _, err := w.Channel.Write(packet); err != nil {
			return 0, err
		}
		w.pending = w.pending[length:]
	}
	return len(data), nil
}

func gatewayTestSigner(t *testing.T) (ssh.Signer, ed25519.PrivateKey) {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer, private
}

// The fixture is a genuine local SSH server. Authentication verifies an SSH
// user certificate against the fixture CA, SFTP uses the real filesystem, and
// the only shell implementation echoes bytes; no OS shell or real SSH host runs.
func newGatewayTestSSH(t *testing.T) *gatewayTestSSH {
	t.Helper()
	storage, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(storage, "homes", "user")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	host, _ := gatewayTestSigner(t)
	ca, _ := gatewayTestSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &gatewayTestSSH{listener: listener, home: home, homes: map[string]string{"user": home}, host: host, ca: ca, connections: map[net.Conn]bool{}, dimensions: make(chan gatewayTestDimensions, 32), stopped: make(chan struct{}), ignoredSFTPEOF: make(chan struct{}, 8)}
	checker := &ssh.CertChecker{IsUserAuthority: func(key ssh.PublicKey) bool { return bytes.Equal(key.Marshal(), ca.PublicKey().Marshal()) }}
	config := &ssh.ServerConfig{PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		permissions, err := checker.Authenticate(metadata, key)
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		gate, entered := s.authGate, s.authEntered
		s.mu.Unlock()
		if gate != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-gate:
			case <-s.stopped:
				return nil, io.EOF
			}
		}
		return permissions, nil
	}}
	// x/crypto invokes this after receiving the first user-auth request,
	// including "none". Diagnostics must stop earlier, during pinned KEX.
	config.BannerCallback = func(ssh.ConnMetadata) string { s.userAuthAttempts.Add(1); return "" }
	config.AddHostKey(host)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			s.acceptedTCP.Add(1)
			s.mu.Lock()
			s.connections[connection] = true
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer func() { _ = connection.Close(); s.mu.Lock(); delete(s.connections, connection); s.mu.Unlock() }()
				conn, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer func() { _ = conn.Close() }()
				s.mu.Lock()
				userHome := s.homes[conn.User()]
				s.mu.Unlock()
				if userHome == "" {
					userHome = s.home
				}
				transportClosed := make(chan struct{})
				defer close(transportClosed)
				s.accepted.Add(1)
				s.wg.Add(1)
				go func() { defer s.wg.Done(); ssh.DiscardRequests(requests) }()
				for channel := range channels {
					if channel.ChannelType() != "session" {
						_ = channel.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, requests, err := channel.Accept()
					if err != nil {
						continue
					}
					s.wg.Add(1)
					go s.serveChannel(ch, requests, transportClosed, userHome)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		close(s.stopped)
		_ = listener.Close()
		s.mu.Lock()
		for connection := range s.connections {
			_ = connection.Close()
		}
		s.mu.Unlock()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("local SSH fixture leaked a connection")
		}
	})
	return s
}

func (s *gatewayTestSSH) serveChannel(ch ssh.Channel, requests <-chan *ssh.Request, transportClosed <-chan struct{}, home string) {
	defer s.wg.Done()
	defer func() { _ = ch.Close() }()
	for request := range requests {
		switch request.Type {
		case "subsystem":
			var value struct{ Name string }
			if ssh.Unmarshal(request.Payload, &value) != nil || value.Name != "sftp" {
				_ = request.Reply(false, nil)
				continue
			}
			_ = request.Reply(true, nil)
			s.mu.Lock()
			ignoreEOF, fault := s.ignoreSFTPEOF, s.stallNextSFTP
			s.stallNextSFTP = nil
			s.mu.Unlock()
			var stream io.ReadWriteCloser = ch
			if fault != nil {
				stream = &gatewayStalledSFTP{Channel: ch, fault: fault}
				defer close(fault.closed)
			}
			server, err := sftp.NewServer(stream, sftp.WithServerWorkingDirectory(home))
			if err != nil {
				return
			}
			_ = server.Serve()
			if ignoreEOF {
				select {
				case s.ignoredSFTPEOF <- struct{}{}:
				default:
				}
				select {
				case <-transportClosed:
				case <-s.stopped:
				}
			}
			_ = server.Close()
			return
		case "pty-req":
			var value struct {
				Term                      string
				Cols, Rows, Width, Height uint32
				Modes                     string
			}
			if ssh.Unmarshal(request.Payload, &value) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			select {
			case s.dimensions <- gatewayTestDimensions{value.Cols, value.Rows}:
			default:
			}
			_ = request.Reply(true, nil)
		case "window-change":
			var value struct{ Cols, Rows, Width, Height uint32 }
			if ssh.Unmarshal(request.Payload, &value) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			select {
			case s.dimensions <- gatewayTestDimensions{value.Cols, value.Rows}:
			default:
			}
			if request.WantReply {
				_ = request.Reply(true, nil)
			}
		case "shell":
			_ = request.Reply(true, nil)
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				_, _ = ch.Write([]byte("fixture-ready\r\n"))
				_, _ = io.Copy(ch, ch)
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				_ = ch.Close()
			}()
		case "exec":
			var value struct{ Command string }
			if ssh.Unmarshal(request.Payload, &value) != nil {
				_ = request.Reply(false, nil)
				continue
			}
			s.mu.Lock()
			allowed := s.allowRunnerExec && strings.Contains(value.Command, "/usr/bin/screen")
			if allowed {
				s.runnerCommands = append(s.runnerCommands, value.Command)
			}
			s.mu.Unlock()
			_ = request.Reply(allowed, nil)
			if allowed {
				_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		default:
			_ = request.Reply(false, nil)
		}
	}
}

type gatewayIntegration struct {
	ssh        *gatewayTestSSH
	gateway    *Gateway
	server     *httptest.Server
	client     *http.Client
	ticketKey  ed25519.PrivateKey
	credential credential
	userKey    ed25519.PrivateKey
}

func newGatewayIntegration(t *testing.T, edit func(*Config)) *gatewayIntegration {
	t.Helper()
	remote := newGatewayTestSSH(t)
	_, ticketKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	config := Config{GatewayID: "gateway-fixture", ClusterID: "source", Listen: "127.0.0.1:0", CallbackListen: "127.0.0.1:9444", StateDirectory: state, FileRoot: remote.home,
		AllowedOrigins: []string{gatewayTestOrigin}, TicketPublicKeys: map[string]string{"fixture": base64.RawURLEncoding.EncodeToString(ticketKey.Public().(ed25519.PublicKey))}, UserCAKeys: []string{string(ssh.MarshalAuthorizedKey(remote.ca.PublicKey()))},
		Clusters: []Cluster{{ID: "source", SSHAddress: remote.listener.Addr().String(), HostKeys: []string{string(ssh.MarshalAuthorizedKey(remote.host.PublicKey()))}}, {ID: "target", SSHAddress: remote.listener.Addr().String(), HostKeys: []string{string(ssh.MarshalAuthorizedKey(remote.host.PublicKey()))}}}}
	if edit != nil {
		edit(&config)
	}
	gateway, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gateway.Close)
	server := httptest.NewTLSServer(gateway)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 5 * time.Second
	_, userKey := gatewayTestSigner(t)
	f := &gatewayIntegration{ssh: remote, gateway: gateway, server: server, client: client, ticketKey: ticketKey, userKey: userKey}
	f.credential = f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(2*time.Minute), nil)
	return f
}

func (f *gatewayIntegration) certificate(t *testing.T, principal string, start, end time.Time, edit func(*ssh.Certificate)) credential {
	t.Helper()
	signer, err := ssh.NewSignerFromKey(f.userKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: signer.PublicKey(), Serial: 1, CertType: ssh.UserCert, KeyId: "web-access/source/access/user:1/1", ValidPrincipals: []string{principal}, ValidAfter: uint64(start.Unix()), ValidBefore: uint64(end.Unix()), Permissions: ssh.Permissions{Extensions: map[string]string{"permit-pty": ""}}}
	if edit != nil {
		edit(cert)
	}
	if err := cert.SignCert(rand.Reader, f.ssh.ca); err != nil {
		t.Fatal(err)
	}
	private, err := ssh.MarshalPrivateKey(f.userKey, "ephemeral fixture key")
	if err != nil {
		t.Fatal(err)
	}
	return credential{PrivateKey: string(pem.EncodeToMemory(private)), Certificate: string(ssh.MarshalAuthorizedKey(cert))}
}

func (f *gatewayIntegration) ticket(t *testing.T, edit func(*Claims)) string {
	t.Helper()
	now := time.Now()
	claims := Claims{RegisteredClaims: jwt.RegisteredClaims{Issuer: "go-ldap-admin", Subject: "1", Audience: jwt.ClaimStrings{"gateway-fixture"}, ID: opaque(), IssuedAt: jwt.NewNumericDate(now.Add(-time.Second)), NotBefore: jwt.NewNumericDate(now.Add(-time.Second)), ExpiresAt: jwt.NewNumericDate(now.Add(2 * time.Minute))}, Username: "user", ClusterID: "source", Scopes: []string{"session:create", "terminal", "sftp", "transfer", "transfer:read"}, DestinationClusterIDs: []string{"target"}}
	if edit != nil {
		edit(&claims)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = "fixture"
	signed, err := token.SignedString(f.ticketKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type gatewayHTTPResult struct {
	status int
	body   []byte
	header http.Header
}

func (f *gatewayIntegration) request(t *testing.T, method, path, ticket, origin string, input any, cookies ...*http.Cookie) gatewayHTTPResult {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, f.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if ticket != "" {
		req.Header.Set("Authorization", "Bearer "+ticket)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		t.Fatal(err)
	}
	return gatewayHTTPResult{response.StatusCode, encoded, response.Header.Clone()}
}

func (f *gatewayIntegration) authCookie(t *testing.T, edit func(*Claims)) *http.Cookie {
	t.Helper()
	value := f.ticket(t, func(c *Claims) {
		c.Scopes = []string{"session:create"}
		if edit != nil {
			edit(c)
		}
	})
	return &http.Cookie{Name: "__Host-hpc_gateway_jwt", Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode}
}
func gatewayData(t *testing.T, result gatewayHTTPResult) map[string]json.RawMessage {
	t.Helper()
	var envelope struct {
		Code int
		Data map[string]json.RawMessage
	}
	if err := json.Unmarshal(result.body, &envelope); err != nil || envelope.Code != 0 {
		t.Fatalf("invalid success response %d: %s", result.status, result.body)
	}
	return envelope.Data
}
func gatewayString(t *testing.T, data map[string]json.RawMessage, key string) string {
	t.Helper()
	var value string
	if json.Unmarshal(data[key], &value) != nil || value == "" {
		t.Fatalf("missing %s", key)
	}
	return value
}
func (f *gatewayIntegration) open(t *testing.T) (string, string) {
	t.Helper()
	ticket := f.ticket(t, nil)
	result := f.request(t, "POST", "/v1/sessions", ticket, gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": f.credential.PrivateKey, "certificate": f.credential.Certificate})
	if result.status != 201 {
		t.Fatalf("opening genuine local SSH/SFTP session failed %d: %s", result.status, result.body)
	}
	data := gatewayData(t, result)
	if home := gatewayString(t, data, "home"); home != f.ssh.home {
		t.Fatalf("wrong authenticated HOME %q", home)
	}
	if root := gatewayString(t, data, "file_root"); root != f.gateway.cfg.FileRoot {
		t.Fatalf("wrong configured file root %q", root)
	}
	return gatewayString(t, data, "session_id"), ticket
}

func TestNativeGatewaySSHSessionFilesAndOwnerIsolation(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	if err := os.WriteFile(filepath.Join(f.ssh.home, "hello.txt"), []byte("hello fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, ticket := f.open(t)
	listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", ticket, gatewayTestOrigin, map[string]string{"path": "."})
	if listed.status != 200 {
		t.Fatalf("listing failed %d: %s", listed.status, listed.body)
	}
	var entries []struct {
		Name, Path string
		Size       int64
	}
	if json.Unmarshal(gatewayData(t, listed)["entries"], &entries) != nil || len(entries) != 1 || entries[0].Name != "hello.txt" || entries[0].Path != filepath.Join(f.ssh.home, "hello.txt") || entries[0].Size != 14 {
		t.Fatalf("incorrect SFTP metadata %s", listed.body)
	}
	if listed.header.Get("Access-Control-Allow-Origin") != gatewayTestOrigin || listed.header.Get("Cache-Control") != "no-store" {
		t.Fatal("missing exact-origin/no-store headers")
	}
	for _, edit := range []func(*Claims){func(c *Claims) { c.Subject = "2" }, func(c *Claims) { c.Username = "other" }, func(c *Claims) { c.Scopes = []string{"terminal"} }} {
		got := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", f.ticket(t, edit), gatewayTestOrigin, map[string]string{"path": "."})
		if got.status != 404 && got.status != 401 {
			t.Fatalf("another actor/scope accessed session: %d %s", got.status, got.body)
		}
	}
	if got := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", ticket, "https://attacker.invalid", map[string]string{"path": "."}); got.status != 403 {
		t.Fatal("wrong browser origin accepted")
	}
	if got := f.request(t, "POST", "/v1/sessions", ticket, gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": f.credential.PrivateKey, "certificate": f.credential.Certificate}); got.status != 409 {
		t.Fatalf("session-create ticket replay accepted %d", got.status)
	}
	closed := f.request(t, "DELETE", "/v1/sessions/"+id, ticket, gatewayTestOrigin, nil)
	if closed.status != 200 {
		t.Fatalf("close failed %d", closed.status)
	}
	if got := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", ticket, gatewayTestOrigin, map[string]string{"path": "."}); got.status != 404 {
		t.Fatal("closed session remained accessible")
	}
}

func TestNativeGatewayOneTimeSingleFileAndZIPDownloads(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	contents := map[string]string{"single.bin": strings.Repeat("streamed\x00fixture\n", 8192), "nested/a.txt": "nested contents\n", "notes.txt": "notes\n"}
	for name, value := range contents {
		filename := filepath.Join(f.ssh.home, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id, ticket := f.open(t)
	once := func(paths []string) string {
		t.Helper()
		result := f.request(t, "POST", "/v1/sessions/"+id+"/files/download-ticket", ticket, gatewayTestOrigin, map[string]any{"paths": paths})
		if result.status != 201 {
			t.Fatalf("download capability failed: %d %s", result.status, result.body)
		}
		return gatewayString(t, gatewayData(t, result), "url")
	}
	singleURL := once([]string{"single.bin"})
	cookie := f.authCookie(t, nil)
	single := f.request(t, "GET", singleURL, "", "", nil, cookie)
	if single.status != 200 || string(single.body) != contents["single.bin"] || !strings.Contains(single.header.Get("Content-Disposition"), "single.bin") || single.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("native file download invalid: %d %s", single.status, single.header)
	}
	if got := f.request(t, "GET", singleURL, "", "", nil, cookie); got.status != 404 {
		t.Fatalf("single-use capability replayed: %d", got.status)
	}
	zipURL := once([]string{"nested", "notes.txt"})
	archive := f.request(t, "GET", zipURL, "", "", nil, cookie)
	if archive.status != 200 || archive.header.Get("Content-Type") != "application/zip" {
		t.Fatalf("ZIP download failed: %d %s", archive.status, archive.body)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive.body), int64(len(archive.body)))
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for _, file := range reader.File {
		input, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		value, err := io.ReadAll(input)
		_ = input.Close()
		if err != nil {
			t.Fatal(err)
		}
		actual[file.Name] = string(value)
	}
	if !reflect.DeepEqual(actual, map[string]string{"nested/a.txt": contents["nested/a.txt"], "notes.txt": contents["notes.txt"]}) {
		t.Fatalf("unsafe or invalid archive contents: %+v", actual)
	}
	if got := f.request(t, "GET", zipURL, "", "", nil, cookie); got.status != 404 {
		t.Fatal("ZIP capability replayed")
	}
	// The capability must be consumed atomically even if a browser retries
	// while the first native download is still streaming.
	concurrentURL := once([]string{"single.bin"})
	type result struct {
		status int
		body   []byte
		err    error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			request, err := http.NewRequest("GET", f.server.URL+concurrentURL, nil)
			if err != nil {
				results <- result{err: err}
				return
			}
			request.AddCookie(cookie)
			response, err := f.client.Do(request)
			if err != nil {
				results <- result{err: err}
				return
			}
			defer func() { _ = response.Body.Close() }()
			body, err := io.ReadAll(response.Body)
			results <- result{response.StatusCode, body, err}
		}()
	}
	statuses := map[int]int{}
	for range 2 {
		value := <-results
		if value.err != nil {
			t.Fatal(value.err)
		}
		statuses[value.status]++
		if value.status == 200 && string(value.body) != contents["single.bin"] {
			t.Fatal("concurrent download corrupted bytes")
		}
	}
	if statuses[200] != 1 || statuses[404] != 1 {
		t.Fatalf("one-time capability admitted concurrent replay: %+v", statuses)
	}
}

func TestNativeGatewayCanceledDownloadPreservesFileSessionAndNextZIP(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	large, err := os.Create(filepath.Join(f.ssh.home, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	err = large.Truncate(64 << 20)
	_ = large.Close()
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{"a.txt": strings.Repeat("a", 64<<10), "b.txt": strings.Repeat("b", 32<<10)}
	for name, value := range contents {
		if err := os.WriteFile(filepath.Join(f.ssh.home, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id, ticket := f.open(t)
	capability := func(paths []string) string {
		t.Helper()
		result := f.request(t, "POST", "/v1/sessions/"+id+"/files/download-ticket", ticket, gatewayTestOrigin, map[string]any{"paths": paths})
		if result.status != 201 {
			t.Fatalf("native download capability failed: %d %s", result.status, result.body)
		}
		return gatewayString(t, gatewayData(t, result), "url")
	}
	cookie := f.authCookie(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "GET", f.server.URL+capability([]string{"large.bin"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(cookie)
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != 200 {
		t.Fatalf("download did not start: %d", response.StatusCode)
	}
	if _, err := io.ReadFull(response.Body, make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.gateway.mu.Lock()
		refs := f.gateway.sessions[id].refs.Load()
		f.gateway.mu.Unlock()
		if refs == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled download did not release its session reference")
		}
		time.Sleep(5 * time.Millisecond)
	}
	listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", ticket, gatewayTestOrigin, map[string]string{"path": "."})
	if listed.status != 200 {
		t.Fatalf("canceled download broke the shared file browser: %d %s", listed.status, listed.body)
	}
	archive := f.request(t, "GET", capability([]string{"a.txt", "b.txt"}), "", "", nil, cookie)
	if archive.status != 200 {
		t.Fatalf("ZIP after canceled download failed: %d %s", archive.status, archive.body)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive.body), int64(len(archive.body)))
	if err != nil {
		t.Fatal(err)
	}
	actual := map[string]string{}
	for _, file := range reader.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		actual[file.Name] = string(raw)
	}
	if !reflect.DeepEqual(actual, contents) {
		t.Fatal("ZIP contents after cancellation do not match the two source files")
	}
	if f.ssh.acceptedTCP.Load() != 1 || f.ssh.accepted.Load() != 1 {
		t.Fatal("file browser recovered by reauthenticating instead of preserving its SSH transport")
	}
}

func TestNativeGatewayRejectsFileRootTraversalAndOutsideSymlinks(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "private.txt"), []byte("outside-secret-must-not-leak"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.ssh.home, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "private.txt"), filepath.Join(f.ssh.home, "file-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.ssh.home, "safe.txt"), []byte("inside-file-via-outside-path"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.ssh.home, filepath.Join(outside, "back")); err != nil {
		t.Fatal(err)
	}
	id, ticket := f.open(t)
	for _, value := range []string{outside, "escape", "escape/private.txt", "file-link", "../" + filepath.Base(outside)} {
		listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", ticket, gatewayTestOrigin, map[string]string{"path": value})
		if listed.status != 403 {
			t.Errorf("unsafe listing path %q returned %d: %s", value, listed.status, listed.body)
		}
		download := f.request(t, "POST", "/v1/sessions/"+id+"/download", ticket, gatewayTestOrigin, map[string]any{"paths": []string{value}})
		if download.status != 403 {
			t.Errorf("unsafe download path %q returned %d", value, download.status)
		}
		if bytes.Contains(download.body, []byte("outside-secret")) || bytes.Contains(listed.body, []byte("private.txt")) {
			t.Error("outside HOME data disclosed")
		}
	}
	// Resolution can cross an outside symlink while returning to a final
	// readable target inside the configured root; only final file scope is
	// constrained. No outside file contents are returned along that path.
	returned := f.request(t, "POST", "/v1/sessions/"+id+"/download", ticket, gatewayTestOrigin, map[string]any{"paths": []string{"escape/back/safe.txt"}})
	if returned.status != 200 || string(returned.body) != "inside-file-via-outside-path" {
		t.Fatalf("inside-root final symlink target was denied: %d %s", returned.status, returned.body)
	}
}

func TestNativeGatewaySharedFileRootDoesNotImposeUsernamePrefixes(t *testing.T) {
	f := newGatewayIntegration(t, func(c *Config) { c.FileRoot = filepath.Dir(c.FileRoot) })
	root := f.gateway.cfg.FileRoot
	otherHome := filepath.Join(root, "other-account")
	shared := filepath.Join(root, "shared-data")
	for _, directory := range []string{otherHome, shared} {
		if err := os.MkdirAll(directory, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range map[string]string{filepath.Join(f.ssh.home, "alice.txt"): "alice-readable", filepath.Join(otherHome, "bob.txt"): "bob-readable", filepath.Join(shared, "shared.txt"): "shared-readable"} {
		if err := os.WriteFile(name, []byte(value), 0644); err != nil {
			t.Fatal(err)
		}
	}
	f.ssh.mu.Lock()
	f.ssh.homes["other"] = otherHome
	f.ssh.mu.Unlock()
	// Each connection has a separately authenticated SSH username and HOME.
	// The fixture does not impersonate OS UIDs; it verifies that gateway scope
	// is the configured root, leaving actual access decisions to the SSH server.
	aliceID, aliceTicket := f.open(t)
	bobCredential := f.certificate(t, "other", time.Now().Add(-time.Second), time.Now().Add(time.Minute), func(c *ssh.Certificate) { c.KeyId = "web-access/source/access/user:2/1" })
	bobBootstrap := f.ticket(t, func(c *Claims) { c.Subject = "2"; c.Username = "other"; c.Scopes = []string{"session:create"} })
	opened := f.request(t, "POST", "/v1/sessions", bobBootstrap, gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": bobCredential.PrivateKey, "certificate": bobCredential.Certificate})
	if opened.status != 201 {
		t.Fatalf("second SSH user session failed: %d %s", opened.status, opened.body)
	}
	bobData := gatewayData(t, opened)
	bobID := gatewayString(t, bobData, "session_id")
	if gatewayString(t, bobData, "home") != otherHome || gatewayString(t, bobData, "file_root") != root {
		t.Fatal("shared root was confused with the second user's HOME")
	}
	bobTicket := f.ticket(t, func(c *Claims) { c.Subject = "2"; c.Username = "other"; c.Scopes = []string{"sftp"} })
	for _, actor := range []struct{ id, ticket, directory, file, want string }{{aliceID, aliceTicket, otherHome, filepath.Join(otherHome, "bob.txt"), "bob-readable"}, {bobID, bobTicket, f.ssh.home, filepath.Join(f.ssh.home, "alice.txt"), "alice-readable"}, {aliceID, aliceTicket, "../shared-data", "../shared-data/shared.txt", "shared-readable"}} {
		listed := f.request(t, "POST", "/v1/sessions/"+actor.id+"/files/list", actor.ticket, gatewayTestOrigin, map[string]string{"path": actor.directory})
		if listed.status != 200 {
			t.Fatalf("gateway imposed a username/HOME prefix inside shared root: %d %s", listed.status, listed.body)
		}
		if gatewayString(t, gatewayData(t, listed), "file_root") != root {
			t.Fatal("shared root changed between users")
		}
		download := f.request(t, "POST", "/v1/sessions/"+actor.id+"/download", actor.ticket, gatewayTestOrigin, map[string]any{"paths": []string{actor.file}})
		if download.status != 200 || string(download.body) != actor.want {
			t.Fatalf("readable shared-root file denied/corrupted: %d %s", download.status, download.body)
		}
	}
	if err := os.Symlink("../shared-data", filepath.Join(f.ssh.home, "shared-link")); err != nil {
		t.Fatal(err)
	}
	linked := f.request(t, "POST", "/v1/sessions/"+aliceID+"/download", aliceTicket, gatewayTestOrigin, map[string]any{"paths": []string{"shared-link/shared.txt"}})
	if linked.status != 200 || string(linked.body) != "shared-readable" {
		t.Fatalf("relative link to another shared-root directory denied: %d %s", linked.status, linked.body)
	}
	// File sharing does not grant another actor control of an SSH session.
	for _, actor := range []struct{ id, ticket string }{{aliceID, bobTicket}, {bobID, aliceTicket}} {
		if got := f.request(t, "POST", "/v1/sessions/"+actor.id+"/files/list", actor.ticket, gatewayTestOrigin, map[string]string{"path": "."}); got.status != 404 {
			t.Fatal("shared root disabled session owner isolation")
		}
	}
	for _, outside := range []string{filepath.Join(filepath.Dir(root), "outside-root"), root + "-other"} {
		if err := os.MkdirAll(outside, 0700); err != nil {
			t.Fatal(err)
		}
		filename := filepath.Join(outside, "secret.txt")
		if err := os.WriteFile(filename, []byte("outside-shared-root-secret"), 0600); err != nil {
			t.Fatal(err)
		}
		relative, err := filepath.Rel(f.ssh.home, filename)
		if err != nil {
			t.Fatal(err)
		}
		for _, input := range []string{filename, relative} {
			got := f.request(t, "POST", "/v1/sessions/"+aliceID+"/download", aliceTicket, gatewayTestOrigin, map[string]any{"paths": []string{input}})
			if got.status != 403 || bytes.Contains(got.body, []byte("outside-shared-root-secret")) {
				t.Fatalf("configured root boundary/prefix bypass: %q status%d", input, got.status)
			}
		}
	}
	if f.ssh.acceptedTCP.Load() != 2 || f.ssh.accepted.Load() != 2 {
		t.Fatal("shared-root operations changed the authenticated SSH transports")
	}
}

func TestNativeGatewayResolvesInsideRootLinksAndBoundsLinkLoops(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	directory := filepath.Join(f.ssh.home, "real-directory")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, "file.txt")
	if err := os.WriteFile(filename, []byte("inside-root-link-content"), 0600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"absolute-directory": directory, "relative-directory": "real-directory", "absolute-file": filename, "relative-file": "real-directory/file.txt", "loop": "loop", "loop-a": "loop-b", "loop-b": "loop-a"} {
		if err := os.Symlink(target, filepath.Join(f.ssh.home, link)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 41; i++ {
		target := fmt.Sprintf("chain-%02d", i+1)
		if i == 40 {
			target = "real-directory/file.txt"
		}
		if err := os.Symlink(target, filepath.Join(f.ssh.home, fmt.Sprintf("chain-%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	id, ticket := f.open(t)
	for _, link := range []string{"absolute-directory", "relative-directory"} {
		listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", ticket, gatewayTestOrigin, map[string]string{"path": link})
		if listed.status != 200 || gatewayString(t, gatewayData(t, listed), "path") != directory {
			t.Fatalf("inside-root directory link was not resolved: %d %s", listed.status, listed.body)
		}
	}
	for _, link := range []string{"absolute-directory/file.txt", "relative-directory/file.txt", "absolute-file", "relative-file", "chain-01"} {
		got := f.request(t, "POST", "/v1/sessions/"+id+"/download", ticket, gatewayTestOrigin, map[string]any{"paths": []string{link}})
		if got.status != 200 || string(got.body) != "inside-root-link-content" {
			t.Fatalf("valid inside-root link denied or corrupted: %q %d %s", link, got.status, got.body)
		}
	}
	for _, link := range []string{"loop", "loop-a", "chain-00"} {
		got := f.request(t, "POST", "/v1/sessions/"+id+"/download", ticket, gatewayTestOrigin, map[string]any{"paths": []string{link}})
		if got.status != 403 {
			t.Fatalf("unbounded symbolic link expansion was not denied: %q %d", link, got.status)
		}
	}
}

func TestNativeGatewayAcceptsHostKeyRotationAndRejectsInvalidUserCertificates(t *testing.T) {
	t.Run("rotated host key", func(t *testing.T) {
		wrong, _ := gatewayTestSigner(t)
		f := newGatewayIntegration(t, func(c *Config) {
			c.Clusters[0].HostKeys = []string{string(ssh.MarshalAuthorizedKey(wrong.PublicKey()))}
		})
		f.open(t)
		if f.ssh.accepted.Load() != 1 {
			t.Fatal("changed SSH host key blocked a valid user-certificate session")
		}
	})
	t.Run("host keys optional", func(t *testing.T) {
		f := newGatewayIntegration(t, func(c *Config) {
			for i := range c.Clusters {
				c.Clusters[i].HostKeys = nil
			}
		})
		f.open(t)
	})
	f := newGatewayIntegration(t, nil)
	for name, makeCredential := range map[string]func() credential{
		"expired": func() credential {
			return f.certificate(t, "user", time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute), nil)
		},
		"over five minutes": func() credential {
			return f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(301*time.Second), nil)
		},
		"principal mismatch": func() credential {
			return f.certificate(t, "someone-else", time.Now().Add(-time.Second), time.Now().Add(time.Minute), nil)
		},
		"untrusted CA": func() credential {
			return f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(time.Minute), nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := makeCredential()
			if name == "untrusted CA" { // Re-sign with a different authority rather than editing an authenticated field.
				key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(value.Certificate))
				if err != nil {
					t.Fatal(err)
				}
				cert := key.(*ssh.Certificate)
				ca, _ := gatewayTestSigner(t)
				if err := cert.SignCert(rand.Reader, ca); err != nil {
					t.Fatal(err)
				}
				value.Certificate = string(ssh.MarshalAuthorizedKey(cert))
			}
			got := f.request(t, "POST", "/v1/sessions", f.ticket(t, nil), gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": value.PrivateKey, "certificate": value.Certificate})
			if got.status != 400 || f.ssh.accepted.Load() != 0 {
				t.Fatalf("invalid ephemeral credential accepted: %d %s", got.status, got.body)
			}
		})
	}
}

func TestNativeGatewayRejectsNormalLoginJWTAndInvalidTicketIntervals(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	normal := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "go-ldap-admin", "sub": "1", "username": "user", "cluster_id": "source", "aud": "gateway-fixture", "exp": time.Now().Add(time.Minute).Unix()})
	normal.Header["kid"] = "fixture"
	signed, err := normal.SignedString([]byte("ordinary-login-secret"))
	if err != nil {
		t.Fatal(err)
	}
	got := f.request(t, "POST", "/v1/sessions", signed, gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": f.credential.PrivateKey, "certificate": f.credential.Certificate})
	if got.status != 401 || f.ssh.accepted.Load() != 0 {
		t.Fatal("application login JWT became a gateway ticket")
	}
	for name, edit := range map[string]func(*Claims){
		"expired": func(c *Claims) {
			c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
			c.NotBefore = c.IssuedAt
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		},
		"overlong":       func(c *Claims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(301 * time.Second)) },
		"other cluster":  func(c *Claims) { c.ClusterID = "target" },
		"other audience": func(c *Claims) { c.Audience = jwt.ClaimStrings{"other-gateway"} },
		"missing scope":  func(c *Claims) { c.Scopes = []string{"sftp"} },
	} {
		t.Run(name, func(t *testing.T) {
			ticket := f.ticket(t, edit)
			if _, err := f.gateway.cfg.VerifyTicket(ticket, "session:create", time.Now()); err == nil {
				t.Fatal("invalid scoped ticket verified")
			}
		})
	}
}

func (f *gatewayIntegration) websocket(t *testing.T, id, origin string) *websocket.Conn {
	t.Helper()
	return f.websocketPath(t, "/v1/sessions/"+id+"/terminal", origin)
}

func (f *gatewayIntegration) websocketPath(t *testing.T, path, origin string, cookies ...*http.Cookie) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cookie := f.authCookie(t, nil)
	if len(cookies) > 0 {
		cookie = cookies[0]
	}
	conn, response, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "https:", "wss:", 1)+path, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{origin}, "Cookie": []string{cookie.Name + "=" + cookie.Value}}})
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("websocket handshake failed %d: %v", status, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func TestNativeGatewayTerminalFirstMessageAuthPTYResizeAndBinaryEcho(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	id, ticket := f.open(t)
	conn := f.websocket(t, id, gatewayTestOrigin)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	auth, _ := json.Marshal(map[string]any{"ticket": ticket, "cols": 80, "rows": 24})
	if err := conn.Write(ctx, websocket.MessageText, auth); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-f.ssh.dimensions:
		if size != (gatewayTestDimensions{80, 24}) {
			t.Fatalf("wrong PTY size %+v", size)
		}
	case <-ctx.Done():
		t.Fatal("no native PTY request")
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":44}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case size := <-f.ssh.dimensions:
		if size != (gatewayTestDimensions{132, 44}) {
			t.Fatalf("wrong native resize %+v", size)
		}
	case <-ctx.Done():
		t.Fatal("no native window-change request")
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("binary-terminal-echo\n")); err != nil {
		t.Fatal(err)
	}
	var output []byte
	for !bytes.Contains(output, []byte("binary-terminal-echo\n")) {
		kind, value, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.MessageBinary {
			t.Fatal("terminal output was not a binary websocket frame")
		}
		output = append(output, value...)
	}
	if !bytes.Contains(output, []byte("fixture-ready")) {
		t.Fatalf("native shell was not opened: %q", output)
	}
	_ = conn.CloseNow()
}

func TestNativeGatewayTerminalRejectsMissingAuthAndOtherOwners(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	id, _ := f.open(t)
	for name, first := range map[string][]byte{"missing": []byte(`{"cols":80,"rows":24}`), "another owner": []byte(`{"ticket":"` + f.ticket(t, func(c *Claims) { c.Subject = "2" }) + `"}`), "binary first": []byte(`{"ticket":"` + f.ticket(t, nil) + `"}`)} {
		t.Run(name, func(t *testing.T) {
			conn := f.websocket(t, id, gatewayTestOrigin)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			kind := websocket.MessageText
			if name == "binary first" {
				kind = websocket.MessageBinary
			}
			if err := conn.Write(ctx, kind, first); err != nil {
				t.Fatal(err)
			}
			_, _, err := conn.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("unauthorized terminal was not closed with policy violation: %v", err)
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "https:", "wss:", 1)+"/v1/sessions/"+id+"/terminal", &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{"https://attacker.invalid"}}})
	if conn != nil {
		_ = conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatal("wrong-origin terminal handshake accepted")
	}
	select {
	case size := <-f.ssh.dimensions:
		t.Fatalf("unauthorized terminal created native PTY %+v", size)
	default:
	}
}

func TestNativeGatewayPerUserQuotaIncludesInFlightSSHAuthentication(t *testing.T) {
	f := newGatewayIntegration(t, func(c *Config) { c.MaxSessionsPerUser = 1 })
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.ssh.mu.Lock()
	f.ssh.authGate = gate
	f.ssh.authEntered = entered
	f.ssh.mu.Unlock()
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate) }) }
	t.Cleanup(unblock)
	firstTicket, secondTicket := f.ticket(t, nil), f.ticket(t, nil)
	input := map[string]string{"cluster_id": "source", "private_key": f.credential.PrivateKey, "certificate": f.credential.Certificate}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest, err := http.NewRequest("POST", f.server.URL+"/v1/sessions", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	firstRequest.Header.Set("Authorization", "Bearer "+firstTicket)
	firstRequest.Header.Set("Origin", gatewayTestOrigin)
	firstRequest.Header.Set("Content-Type", "application/json")
	type asynchronousResult struct {
		response gatewayHTTPResult
		err      error
	}
	first := make(chan asynchronousResult, 1)
	go func() {
		response, err := f.client.Do(firstRequest)
		if err != nil {
			first <- asynchronousResult{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		first <- asynchronousResult{gatewayHTTPResult{response.StatusCode, body, response.Header.Clone()}, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first SSH authentication did not reach the deterministic barrier")
	}
	f.gateway.mu.Lock()
	pending := f.gateway.pendingOwners["1"]
	sessions := len(f.gateway.sessions)
	f.gateway.mu.Unlock()
	if pending != 1 || sessions != 0 {
		t.Fatalf("first SSH request was not reserved: pending%d sessions%d", pending, sessions)
	}
	second := f.request(t, "POST", "/v1/sessions", secondTicket, gatewayTestOrigin, input)
	if second.status != 429 {
		t.Fatalf("same actor bypassed in-flight session limit: %d %s", second.status, second.body)
	}
	select {
	case result := <-first:
		t.Fatalf("first authentication completed before release: %+v", result)
	default:
	}
	if f.ssh.acceptedTCP.Load() != 1 {
		t.Fatal("rejected second request still opened another SSH transport")
	}
	unblock()
	var result asynchronousResult
	select {
	case result = <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("first SSH session did not complete after release")
	}
	if result.err != nil || result.response.status != 201 {
		t.Fatalf("first request failed after release: %v %d %s", result.err, result.response.status, result.response.body)
	}
	id := gatewayString(t, gatewayData(t, result.response), "session_id")
	f.gateway.mu.Lock()
	pending = f.gateway.pendingOwners["1"]
	reservations := f.gateway.reservations
	f.gateway.mu.Unlock()
	if pending != 0 || reservations != 0 {
		t.Fatal("successful SSH session retained an in-flight reservation")
	}
	if closed := f.request(t, "DELETE", "/v1/sessions/"+id, firstTicket, gatewayTestOrigin, nil); closed.status != 200 {
		t.Fatal("could not release the user's session quota")
	}
	// A quota rejection does not burn a fresh one-time session ticket.
	if retry := f.request(t, "POST", "/v1/sessions", secondTicket, gatewayTestOrigin, input); retry.status != 201 {
		t.Fatalf("released quota or rejected ticket was not reusable: %d %s", retry.status, retry.body)
	}
}

func TestNativeGatewayDiagnosticsOnlyProbeConfiguredKeyExchange(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	ticket := f.ticket(t, func(c *Claims) { c.Scopes = []string{"diagnostics"} })
	for _, input := range []map[string]any{{"url": "https://unconfigured.invalid/"}, {"host": "127.0.0.1", "port": 22}, {"cluster_id": "target"}} {
		got := f.request(t, "POST", "/v1/diagnostics", ticket, gatewayTestOrigin, input)
		if got.status != 400 {
			t.Fatalf("diagnostic caller supplied its own network target: %d %s", got.status, got.body)
		}
	}
	if f.ssh.acceptedTCP.Load() != 0 {
		t.Fatal("rejected diagnostic input still opened a network connection")
	}
	missingScope := f.request(t, "POST", "/v1/diagnostics", f.ticket(t, nil), gatewayTestOrigin, map[string]any{})
	if missingScope.status != 401 {
		t.Fatal("terminal/file ticket authorized diagnostics without its scope")
	}
	got := f.request(t, "POST", "/v1/diagnostics", ticket, gatewayTestOrigin, map[string]any{})
	if got.status != 200 {
		t.Fatalf("native configured-host probe failed %d: %s", got.status, got.body)
	}
	data := gatewayData(t, got)
	if gatewayString(t, data, "cluster_id") != "source" || gatewayString(t, data, "gateway_id") != "gateway-fixture" {
		t.Fatal("diagnostics probed an unconfigured identity")
	}
	var results []struct {
		ProbeID                  string `json:"probe_id"`
		DestinationClusterID     string `json:"destination_cluster_id"`
		Status, Detail, Coverage string
		LatencyMS                int64 `json:"latency_ms"`
	}
	const coverage = "SSH transport/key exchange; host identity is not pinned; user auth not tested"
	if json.Unmarshal(data["results"], &results) != nil || len(results) != 3 || results[0].ProbeID != "ssh-handshake" || results[0].Status != "ok" || results[0].Detail != "ssh_key_exchange_completed" || results[0].Coverage != coverage || results[0].LatencyMS < 0 {
		t.Fatalf("misleading diagnostic result: %s", got.body)
	}
	local := results[1]
	if local.ProbeID != "transfer-runtime" || local.Detail == "" || local.Coverage == "" || local.LatencyMS < 0 {
		t.Fatalf("local transfer runtime result is incomplete: %s", got.body)
	}
	if local.Status != "ok" && local.Status != "error" && local.Status != "timeout" {
		t.Fatalf("runtime reported an unsupported status: %s", got.body)
	}
	// Verify the actual fixed local binary independently. A missing, old or
	// unrecognizable rsync cannot honestly produce a successful runtime probe;
	// modern rsync may still report another missing prerequisite, so do not
	// assume this developer machine is a provisioned transfer login node.
	versionCtx, stopVersion := context.WithTimeout(context.Background(), 2*time.Second)
	version, versionErr := exec.CommandContext(versionCtx, "/usr/bin/rsync", "--version").Output()
	stopVersion()
	parts := regexp.MustCompile(`rsync\s+version\s+(\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(string(version))
	compatible := false
	if versionErr == nil && len(parts) == 4 {
		major, _ := strconv.Atoi(parts[1])
		minor, _ := strconv.Atoi(parts[2])
		patch, _ := strconv.Atoi(parts[3])
		compatible = major > 3 || (major == 3 && (minor > 2 || (minor == 2 && patch >= 4)))
	}
	if !compatible && local.Status == "ok" {
		t.Fatalf("runtime claimed success with unavailable/unsupported fixed rsync: %q; result%s", version, got.body)
	}
	remote := results[2]
	if remote.ProbeID != "transfer-ssh" || remote.DestinationClusterID != "target" || remote.Status != "ok" || remote.Detail != "ssh_key_exchange_completed" || remote.Coverage != coverage || remote.LatencyMS < 0 {
		t.Fatalf("configured transfer destination was not honestly probed: %s", got.body)
	}
	if f.ssh.acceptedTCP.Load() != 2 || f.ssh.accepted.Load() != 0 || f.ssh.userAuthAttempts.Load() != 0 {
		t.Fatal("source/destination probes did not stop before user authentication")
	}
}

func TestNativeGatewayDiagnosticWebsocketRequiresScopedFirstFrame(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	for _, allowed := range []bool{true, false} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		cookie := f.authCookie(t, nil)
		conn, _, err := websocket.Dial(ctx, strings.Replace(f.server.URL, "https:", "wss:", 1)+"/v1/diagnostics/socket", &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{gatewayTestOrigin}, "Cookie": []string{cookie.Name + "=" + cookie.Value}}})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ticket := f.ticket(t, func(c *Claims) {
			if allowed {
				c.Scopes = []string{"diagnostics"}
			}
		})
		first, _ := json.Marshal(map[string]string{"ticket": ticket})
		if err := conn.Write(ctx, websocket.MessageText, first); err != nil {
			_ = conn.CloseNow()
			cancel()
			t.Fatal(err)
		}
		kind, response, err := conn.Read(ctx)
		if allowed {
			if err != nil || kind != websocket.MessageText {
				_ = conn.CloseNow()
				cancel()
				t.Fatalf("authenticated diagnostic frame missing: %v", err)
			}
			var result struct {
				ProbeID string `json:"probe_id"`
				Status  string
			}
			if json.Unmarshal(response, &result) != nil || result.ProbeID != "gateway-websocket" || result.Status != "ok" {
				t.Errorf("invalid diagnostic acknowledgement %s", response)
			}
			_, _, err = conn.Read(ctx)
			if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Errorf("successful diagnostic socket did not close cleanly: %v", err)
			}
		} else if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
			t.Errorf("missing diagnostics scope accepted: %v", err)
		}
		_ = conn.CloseNow()
		cancel()
	}
	if f.ssh.acceptedTCP.Load() != 0 {
		t.Fatal("WebSocket connectivity probe unexpectedly opened SSH")
	}
}

func TestNativeGatewaySourceCertificateBindsClusterActorSerialAndPurpose(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	for name, edit := range map[string]func(*ssh.Certificate){
		"another cluster":       func(c *ssh.Certificate) { c.KeyId = "web-access/target/access/user:1/1" },
		"another actor":         func(c *ssh.Certificate) { c.KeyId = "web-access/source/access/user:2/1" },
		"wrong serial":          func(c *ssh.Certificate) { c.Serial = 2 },
		"uppercase serial":      func(c *ssh.Certificate) { c.Serial = 0xaf; c.KeyId = "web-access/source/access/user:1/AF" },
		"zero serial":           func(c *ssh.Certificate) { c.Serial = 0; c.KeyId = "web-access/source/access/user:1/0" },
		"terminal purpose":      func(c *ssh.Certificate) { c.KeyId = "web-access/source/terminal/user:1/1" },
		"files purpose":         func(c *ssh.Certificate) { c.KeyId = "web-access/source/files/user:1/1" },
		"missing PTY extension": func(c *ssh.Certificate) { c.Extensions = map[string]string{} },
		"transfer purpose": func(c *ssh.Certificate) {
			c.KeyId = "web-access/source/transfer/user:1/1"
			c.CriticalOptions = map[string]string{"force-command": "/usr/bin/rsync --server --sender . /fixture"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			connectionsBefore := f.ssh.acceptedTCP.Load()
			value := f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(time.Minute), edit)
			result := f.request(t, "POST", "/v1/sessions", f.ticket(t, nil), gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": value.PrivateKey, "certificate": value.Certificate})
			if result.status != 400 {
				t.Fatalf("wrong signed source binding was accepted: %d %s", result.status, result.body)
			}
			if f.ssh.acceptedTCP.Load() != connectionsBefore {
				t.Fatal("invalid source binding reached SSH instead of being rejected locally")
			}
		})
	}
	t.Run("missing bootstrap scope", func(t *testing.T) {
		connectionsBefore := f.ssh.acceptedTCP.Load()
		ticket := f.ticket(t, func(c *Claims) { c.Scopes = []string{"terminal", "sftp"} })
		result := f.request(t, "POST", "/v1/sessions", ticket, gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": f.credential.PrivateKey, "certificate": f.credential.Certificate})
		if result.status != 401 || f.ssh.acceptedTCP.Load() != connectionsBefore {
			t.Fatalf("per-route scopes implicitly authorized session creation: %d %s", result.status, result.body)
		}
	})
}

func TestNativeGatewayAccessCredentialSupportsSeparatelyScopedFilesAndTerminal(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	bootstrap := f.ticket(t, func(c *Claims) { c.Scopes = []string{"session:create"} })
	opened := f.request(t, "POST", "/v1/sessions", bootstrap, gatewayTestOrigin, map[string]string{"cluster_id": "source", "private_key": f.credential.PrivateKey, "certificate": f.credential.Certificate})
	if opened.status != 201 {
		t.Fatalf("access credential required unrelated per-route bootstrap scopes: %d %s", opened.status, opened.body)
	}
	data := gatewayData(t, opened)
	id := gatewayString(t, data, "session_id")
	if gatewayString(t, data, "home") != f.ssh.home || gatewayString(t, data, "file_root") != f.gateway.cfg.FileRoot {
		t.Fatal("bootstrap did not distinguish SSH HOME and configured file root")
	}
	if denied := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", bootstrap, gatewayTestOrigin, map[string]string{"path": "."}); denied.status != 401 {
		t.Fatal("bootstrap-only ticket authorized file operations")
	}
	filesTicket := f.ticket(t, func(c *Claims) { c.Scopes = []string{"sftp"} })
	listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", filesTicket, gatewayTestOrigin, map[string]string{"path": "."})
	if listed.status != 200 {
		t.Fatalf("access credential could not access SFTP: %d %s", listed.status, listed.body)
	}
	if gatewayString(t, gatewayData(t, listed), "file_root") != f.gateway.cfg.FileRoot {
		t.Fatal("list response omitted the common file root")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	deniedSocket := f.websocket(t, id, gatewayTestOrigin)
	gatewayWriteControl(t, ctx, deniedSocket, map[string]any{"ticket": filesTicket, "cols": 80, "rows": 24})
	gatewayPolicyClose(t, ctx, deniedSocket)
	terminalTicket := f.ticket(t, func(c *Claims) { c.Scopes = []string{"terminal"} })
	if denied := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", terminalTicket, gatewayTestOrigin, map[string]string{"path": "."}); denied.status != 401 {
		t.Fatal("terminal-only ticket authorized files")
	}
	conn := f.websocket(t, id, gatewayTestOrigin)
	gatewayWriteControl(t, ctx, conn, map[string]any{"ticket": terminalTicket, "cols": 80, "rows": 24})
	gatewayReadBinaryUntil(t, ctx, conn, "fixture-ready")
	if err := conn.Write(ctx, websocket.MessageBinary, []byte("same-access-credential-both-routes\n")); err != nil {
		t.Fatal(err)
	}
	gatewayReadBinaryUntil(t, ctx, conn, "same-access-credential-both-routes\n")
	if f.ssh.acceptedTCP.Load() != 1 || f.ssh.accepted.Load() != 1 {
		t.Fatal("shared access credential unexpectedly created another SSH transport")
	}
}

func gatewayReadBinaryUntil(t *testing.T, ctx context.Context, conn *websocket.Conn, wanted string) {
	t.Helper()
	var result []byte
	for !bytes.Contains(result, []byte(wanted)) {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("native binary stream closed before %q: %v", wanted, err)
		}
		if kind == websocket.MessageBinary {
			result = append(result, data...)
		}
		if len(result) > 1<<20 {
			t.Fatal("fixture received unbounded output")
		}
	}
}

func gatewayWriteControl(t *testing.T, ctx context.Context, conn *websocket.Conn, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatal(err)
	}
}

func gatewayPolicyClose(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Fatalf("expired/invalid lease did not close1008: %v", err)
			}
			return
		}
	}
}

func gatewayWaitPast(t *testing.T, ctx context.Context, expiry time.Time) {
	t.Helper()
	timer := time.NewTimer(time.Until(expiry.Add(100 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("test context expired before original ticket deadline")
	}
	if !time.Now().After(expiry) {
		t.Fatal("fixture did not pass the original ticket expiration")
	}
}

func TestNativeGatewayTerminalTicketLeaseExpiryAndSameConnectionRenewal(t *testing.T) {
	for _, renew := range []bool{false, true} {
		name := "expires"
		if renew {
			name = "renewed same SSH connection"
		}
		t.Run(name, func(t *testing.T) {
			f := newGatewayIntegration(t, nil)
			// This SSH authentication credential expires while the connection
			// is already established. Renewal must concern the gateway ticket,
			// not require a replacement certificate or another SSH login.
			certificateExpiry := time.Now().Add(2 * time.Second)
			f.credential = f.certificate(t, "user", time.Now().Add(-time.Second), certificateExpiry, nil)
			id, _ := f.open(t)
			expiry := time.Unix(time.Now().Unix()+3, 0)
			if !renew {
				expiry = time.Unix(time.Now().Unix()+2, 0)
			}
			short := f.ticket(t, func(c *Claims) { c.Scopes = []string{"terminal"}; c.ExpiresAt = jwt.NewNumericDate(expiry) })
			conn := f.websocket(t, id, gatewayTestOrigin)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			gatewayWriteControl(t, ctx, conn, map[string]any{"ticket": short, "cols": 80, "rows": 24})
			gatewayReadBinaryUntil(t, ctx, conn, "fixture-ready")
			if !renew {
				gatewayPolicyClose(t, ctx, conn)
				return
			}
			fresh := f.ticket(t, func(c *Claims) {
				c.Scopes = []string{"terminal"}
				c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(6 * time.Second))
			})
			gatewayWriteControl(t, ctx, conn, map[string]string{"type": "auth", "ticket": fresh})
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("renewal-control-accepted\n")); err != nil {
				t.Fatal(err)
			}
			gatewayReadBinaryUntil(t, ctx, conn, "renewal-control-accepted\n")
			gatewayWaitPast(t, ctx, expiry)
			if time.Now().Unix() < certificateExpiry.Unix() {
				t.Fatal("SSH certificate has not expired in renewal fixture")
			}
			if err := conn.Write(ctx, websocket.MessageBinary, []byte("echo-after-original-expiry\n")); err != nil {
				t.Fatal(err)
			}
			gatewayReadBinaryUntil(t, ctx, conn, "echo-after-original-expiry\n")
			if f.ssh.acceptedTCP.Load() != 1 || f.ssh.accepted.Load() != 1 {
				t.Fatal("gateway renewal created another SSH transport/authentication")
			}
		})
	}
}

func TestNativeGatewayTerminalRenewalCannotChangeActorOrRequiredScope(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	id, ticket := f.open(t)
	for name, edit := range map[string]func(*Claims){"another actor": func(c *Claims) { c.Subject = "2" }, "another username": func(c *Claims) { c.Username = "other" }, "another cluster": func(c *Claims) { c.ClusterID = "target" }, "missing terminal scope": func(c *Claims) { c.Scopes = []string{"sftp"} }} {
		t.Run(name, func(t *testing.T) {
			conn := f.websocket(t, id, gatewayTestOrigin)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			gatewayWriteControl(t, ctx, conn, map[string]string{"ticket": ticket})
			gatewayReadBinaryUntil(t, ctx, conn, "fixture-ready")
			gatewayWriteControl(t, ctx, conn, map[string]string{"type": "auth", "ticket": f.ticket(t, edit)})
			gatewayPolicyClose(t, ctx, conn)
		})
	}
	if f.ssh.acceptedTCP.Load() != 1 {
		t.Fatal("invalid ticket renewal tried another SSH identity")
	}
}

func TestNativeGatewayRawLogTicketLeaseExpiryRenewalAndOwnership(t *testing.T) {
	for _, scenario := range []string{"expires", "renews", "another actor", "missing log scope"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGatewayIntegration(t, nil)
			id, _ := f.open(t)
			transferID := opaque()
			directory := filepath.Join(f.ssh.home, ".go-ldap-admin-transfers", transferID)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			filename := filepath.Join(directory, "raw.log")
			if err := os.WriteFile(filename, []byte("raw-log-before-renewal\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.gateway.mu.Lock()
			f.gateway.transfers[transferID] = &Transfer{Version: 1, EventID: transferID, TransferID: transferID, GatewayID: f.gateway.cfg.GatewayID, ClusterID: "source", UserID: "1", NumericUserID: 1, Username: "user", SessionID: id, RemoteDirectory: directory, Status: "running", StartedAt: time.Now().UTC()}
			f.gateway.mu.Unlock()
			expiry := time.Unix(time.Now().Unix()+3, 0)
			ticket := f.ticket(t, func(c *Claims) { c.Scopes = []string{"transfer:read"}; c.ExpiresAt = jwt.NewNumericDate(expiry) })
			conn := f.websocketPath(t, "/v1/transfers/"+transferID+"/logs", gatewayTestOrigin)
			ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
			defer cancel()
			gatewayWriteControl(t, ctx, conn, map[string]string{"ticket": ticket})
			gatewayReadBinaryUntil(t, ctx, conn, "raw-log-before-renewal\n")
			if scenario == "expires" {
				gatewayPolicyClose(t, ctx, conn)
				return
			}
			fresh := f.ticket(t, func(c *Claims) {
				c.Scopes = []string{"transfer:read"}
				c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(6 * time.Second))
				if scenario == "another actor" {
					c.Subject = "2"
				}
				if scenario == "missing log scope" {
					c.Scopes = []string{"terminal"}
				}
			})
			gatewayWriteControl(t, ctx, conn, map[string]string{"type": "auth", "ticket": fresh})
			if scenario == "another actor" || scenario == "missing log scope" {
				gatewayPolicyClose(t, ctx, conn)
				return
			}
			gatewayWaitPast(t, ctx, expiry)
			file, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = file.WriteString("raw-log-after-original-expiry\n")
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			gatewayReadBinaryUntil(t, ctx, conn, "raw-log-after-original-expiry\n")
			if f.ssh.acceptedTCP.Load() != 1 || f.ssh.accepted.Load() != 1 {
				t.Fatal("log renewal redialed SSH instead of retaining the source session")
			}
		})
	}
}

func (s *gatewayTestSSH) closeTransports() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for connection := range s.connections {
		_ = connection.Close()
	}
}

func TestNativeGatewayCloseReleasesSFTPIgnoringEOF(t *testing.T) {
	for _, operation := range []string{"gateway close", "delete", "prune"} {
		t.Run(operation, func(t *testing.T) {
			f := newGatewayIntegration(t, nil)
			f.ssh.mu.Lock()
			f.ssh.ignoreSFTPEOF = true
			f.ssh.mu.Unlock()
			// If a regression blocks the operation, force the fixture transport
			// closed before the ordinary HTTP/gateway cleanup can itself hang.
			t.Cleanup(f.ssh.closeTransports)
			id, ticket := f.open(t)
			f.gateway.mu.Lock()
			session := f.gateway.sessions[id]
			f.gateway.mu.Unlock()
			baseClose := make(chan struct{})
			go func() { _ = session.files.Close(); close(baseClose) }()
			select {
			case <-f.ssh.ignoredSFTPEOF:
			case <-time.After(time.Second):
				t.Fatal("fixture did not observe the native SFTP client's EOF")
			}
			select {
			case <-baseClose:
				t.Fatal("fixture closed stdout instead of simulating an EOF-ignoring remote")
			default:
			}
			type result struct {
				status int
				err    error
			}
			done := make(chan result, 1)
			if operation == "prune" {
				f.gateway.mu.Lock()
				session.last = time.Now().Add(-time.Duration(f.gateway.cfg.SessionIdleSeconds+1) * time.Second)
				f.gateway.mu.Unlock()
			}
			go func() {
				if operation == "gateway close" {
					f.gateway.Close()
					done <- result{status: 200}
					return
				}
				if operation == "prune" {
					f.gateway.prune()
					done <- result{status: 200}
					return
				}
				request, err := http.NewRequest("DELETE", f.server.URL+"/v1/sessions/"+id, nil)
				if err != nil {
					done <- result{err: err}
					return
				}
				request.Header.Set("Origin", gatewayTestOrigin)
				request.Header.Set("Authorization", "Bearer "+ticket)
				response, err := f.client.Do(request)
				if err != nil {
					done <- result{err: err}
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				done <- result{status: response.StatusCode}
			}()
			select {
			case value := <-done:
				if value.err != nil || value.status != 200 {
					t.Fatalf("close operation failed: status%d err%v", value.status, value.err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("close waited for a remote SFTP EOF instead of closing its SSH transport")
			}
			select {
			case <-baseClose:
			case <-time.After(time.Second):
				t.Fatal("closing SSH transport did not release the SFTP receive loop")
			}
			f.gateway.mu.Lock()
			remaining := len(f.gateway.sessions)
			f.gateway.mu.Unlock()
			if remaining != 0 {
				t.Fatal("closed session remained in the gateway")
			}
		})
	}
}

func TestNativeGatewayBlockedLogChannelCancellationPreservesFileSession(t *testing.T) {
	for _, scenario := range []string{"client cancellation", "ticket expiry"} {
		t.Run(scenario, func(t *testing.T) {
			f := newGatewayIntegration(t, nil)
			t.Cleanup(f.ssh.closeTransports)
			if err := os.WriteFile(filepath.Join(f.ssh.home, "browser.txt"), []byte("base-file-session-still-readable"), 0600); err != nil {
				t.Fatal(err)
			}
			id, bootstrap := f.open(t)
			transferID := opaque()
			directory := filepath.Join(f.ssh.home, ".go-ldap-admin-transfers", transferID)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(directory, "raw.log"), []byte("this SFTP DATA response will be withheld\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.gateway.mu.Lock()
			f.gateway.transfers[transferID] = &Transfer{Version: 1, EventID: transferID, TransferID: transferID, GatewayID: f.gateway.cfg.GatewayID, ClusterID: "source", UserID: "1", NumericUserID: 1, Username: "user", SessionID: id, RemoteDirectory: directory, Status: "running", StartedAt: time.Now().UTC()}
			f.gateway.mu.Unlock()
			fault := &gatewayTestSFTPFault{blocked: make(chan struct{}), closed: make(chan struct{})}
			f.ssh.mu.Lock()
			f.ssh.stallNextSFTP = fault
			f.ssh.mu.Unlock()
			ticket := f.ticket(t, func(c *Claims) {
				c.Scopes = []string{"transfer:read"}
				if scenario == "ticket expiry" {
					c.ExpiresAt = jwt.NewNumericDate(time.Unix(time.Now().Unix()+3, 0))
				}
			})
			conn := f.websocketPath(t, "/v1/transfers/"+transferID+"/logs", gatewayTestOrigin)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			gatewayWriteControl(t, ctx, conn, map[string]string{"ticket": ticket})
			select {
			case <-fault.blocked:
			case <-ctx.Done():
				t.Fatal("log did not read from the independent, intentionally blocked native SFTP channel")
			}
			if scenario == "client cancellation" {
				_ = conn.CloseNow()
			} else {
				gatewayPolicyClose(t, ctx, conn)
			}
			select {
			case <-fault.closed:
			case <-time.After(2 * time.Second):
				t.Fatal("log cancellation did not release the blocked SFTP read/channel")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				f.gateway.mu.Lock()
				session := f.gateway.sessions[id]
				refs := int32(-1)
				if session != nil {
					refs = session.refs.Load()
				}
				f.gateway.mu.Unlock()
				if refs == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("canceled log handler retained its session reference")
				}
				time.Sleep(5 * time.Millisecond)
			}
			listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", bootstrap, gatewayTestOrigin, map[string]string{"path": "."})
			if listed.status != 200 {
				t.Fatalf("canceled log broke file browsing: %d %s", listed.status, listed.body)
			}
			// Check the base subsystem directly: native downloads now use their
			// own cancelable channel instead of borrowing this shared client.
			f.gateway.mu.Lock()
			base := f.gateway.sessions[id].files
			f.gateway.mu.Unlock()
			file, err := base.Open(filepath.Join(f.ssh.home, "browser.txt"))
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil || string(contents) != "base-file-session-still-readable" {
				t.Fatalf("canceled log broke the base SFTP client: %q %v", contents, err)
			}
			download := f.request(t, "POST", "/v1/sessions/"+id+"/download", bootstrap, gatewayTestOrigin, map[string]any{"paths": []string{"browser.txt"}})
			if download.status != 200 || string(download.body) != "base-file-session-still-readable" {
				t.Fatalf("canceled log broke file download: %d %s", download.status, download.body)
			}
			if f.ssh.acceptedTCP.Load() != 1 || f.ssh.accepted.Load() != 1 {
				t.Fatal("log/file operations reauthenticated or lost the original SSH transport")
			}
			f.gateway.mu.Lock()
			transfer := f.gateway.transfers[transferID]
			unchanged := transfer.Status == "running" && transfer.FinishedAt == nil
			f.gateway.mu.Unlock()
			if !unchanged {
				t.Fatal("closing a log stream changed the detached transfer lifecycle")
			}
		})
	}
}

func gatewayResponseCookie(t *testing.T, result gatewayHTTPResult) *http.Cookie {
	t.Helper()
	for _, cookie := range (&http.Response{Header: result.header}).Cookies() {
		if cookie.Name == "__Host-hpc_gateway_jwt" {
			return cookie
		}
	}
	t.Fatalf("gateway did not set its host-only auth cookie: %d %s", result.status, result.header)
	return nil
}

func (f *gatewayIntegration) applicationLoginJWT(t *testing.T) string {
	t.Helper()
	claims, err := f.gateway.cfg.VerifyTicket(f.ticket(t, nil), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = "fixture"
	value, err := token.SignedString([]byte("ordinary-application-login-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestNativeGatewayCookieFlagsExpiryAndLogoutPreserveBackendSessions(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	id, backendTicket := f.open(t)
	transferID := opaque()
	f.gateway.mu.Lock()
	f.gateway.transfers[transferID] = &Transfer{TransferID: transferID, UserID: "1", Username: "user", SessionID: id, Status: "running"}
	f.gateway.mu.Unlock()
	expiry := time.Unix(time.Now().Unix()+90, 0)
	ticket := f.ticket(t, func(c *Claims) { c.Scopes = []string{"sftp"}; c.ExpiresAt = jwt.NewNumericDate(expiry) })
	result := f.request(t, "POST", "/v1/auth/session", ticket, gatewayTestOrigin, map[string]any{})
	if result.status != 200 {
		t.Fatalf("valid scoped JWT could not bootstrap cookie: %d %s", result.status, result.body)
	}
	cookie := gatewayResponseCookie(t, result)
	if cookie.Value != ticket || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" || !cookie.Expires.Equal(expiry) || cookie.MaxAge < 1 || cookie.MaxAge > 90 {
		t.Fatalf("unsafe auth cookie attributes: %+v", cookie)
	}
	if result.header.Get("Access-Control-Allow-Origin") != gatewayTestOrigin || result.header.Get("Access-Control-Allow-Credentials") != "true" || result.header.Get("Cache-Control") != "no-store" {
		t.Fatal("cookie bootstrap omitted exact credentialed CORS or no-store")
	}
	if f.client.Jar != nil {
		t.Fatal("tests must not implicitly attach cookies to unrelated auth-negative requests")
	}
	if listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", "", gatewayTestOrigin, map[string]string{"path": "."}, cookie); listed.status != 200 {
		t.Fatalf("scoped cookie fallback did not authorize files: %d %s", listed.status, listed.body)
	}
	if wrongHeader := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", f.applicationLoginJWT(t), gatewayTestOrigin, map[string]string{"path": "."}, cookie); wrongHeader.status != 401 {
		t.Fatal("invalid explicit Authorization silently fell back to a valid cookie")
	}
	if denied := f.request(t, "POST", "/v1/auth/session", ticket, "https://attacker.invalid", map[string]any{}); denied.status != 403 || denied.header.Get("Set-Cookie") != "" {
		t.Fatal("wrong-origin request changed the auth cookie")
	}
	if denied := f.request(t, "POST", "/v1/auth/session", f.applicationLoginJWT(t), gatewayTestOrigin, map[string]any{}); denied.status != 403 || denied.header.Get("Set-Cookie") != "" {
		t.Fatal("ordinary application HS JWT became a gateway cookie")
	}
	expired := f.ticket(t, func(c *Claims) {
		c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
		c.NotBefore = c.IssuedAt
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	})
	if denied := f.request(t, "POST", "/v1/auth/session", expired, gatewayTestOrigin, map[string]any{}); denied.status != 403 || denied.header.Get("Set-Cookie") != "" {
		t.Fatal("expired scoped JWT became a cookie")
	}
	for _, mode := range []string{"bearer", "cookie"} {
		authorization := ""
		var cookies []*http.Cookie
		if mode == "bearer" {
			authorization = ticket
		} else {
			cookies = []*http.Cookie{cookie}
		}
		cleared := f.request(t, "DELETE", "/v1/auth/session", authorization, gatewayTestOrigin, nil, cookies...)
		if cleared.status != 200 {
			t.Fatalf("cookie logout via%s failed: %d %s", mode, cleared.status, cleared.body)
		}
		deleted := gatewayResponseCookie(t, cleared)
		if deleted.Value != "" || deleted.MaxAge >= 0 || !deleted.Expires.Before(time.Now()) || !deleted.Secure || !deleted.HttpOnly || deleted.SameSite != http.SameSiteStrictMode || deleted.Path != "/" || deleted.Domain != "" {
			t.Fatalf("logout cookie was not securely expired: %+v", deleted)
		}
		if listed := f.request(t, "POST", "/v1/sessions/"+id+"/files/list", backendTicket, gatewayTestOrigin, map[string]string{"path": "."}); listed.status != 200 {
			t.Fatalf("cookie logout destroyed the backend SSH session: %d %s", listed.status, listed.body)
		}
		f.gateway.mu.Lock()
		transfer := f.gateway.transfers[transferID]
		intact := transfer != nil && transfer.Status == "running" && transfer.FinishedAt == nil
		f.gateway.mu.Unlock()
		if !intact {
			t.Fatal("cookie logout changed a detached transfer")
		}
	}
	if denied := f.request(t, "DELETE", "/v1/auth/session", ticket, "https://attacker.invalid", nil, cookie); denied.status != 403 || denied.header.Get("Set-Cookie") != "" {
		t.Fatal("cross-origin request cleared the gateway cookie")
	}
	if f.ssh.acceptedTCP.Load() != 1 || f.ssh.accepted.Load() != 1 {
		t.Fatal("cookie login/logout reauthenticated or closed the existing SSH transport")
	}
}

func TestNativeGatewayCookiePreUpgradeGateAndFirstFrameIdentityBinding(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	id, _ := f.open(t)
	address := strings.Replace(f.server.URL, "https:", "wss:", 1) + "/v1/sessions/" + id + "/terminal"
	expired := f.authCookie(t, func(c *Claims) {
		c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
		c.NotBefore = c.IssuedAt
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	})
	wrongName := f.authCookie(t, nil)
	wrongName.Name = "gowebmini-Token"
	for name, cookie := range map[string]*http.Cookie{"missing": nil, "malformed": {Name: "__Host-hpc_gateway_jwt", Value: "invalid"}, "expired": expired, "application HS JWT": {Name: "__Host-hpc_gateway_jwt", Value: f.applicationLoginJWT(t)}, "application cookie name": wrongName} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			headers := http.Header{"Origin": []string{gatewayTestOrigin}}
			if cookie != nil {
				headers.Set("Cookie", cookie.Name+"="+cookie.Value)
			}
			conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: headers})
			if conn != nil {
				_ = conn.CloseNow()
			}
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if err == nil || response == nil || response.StatusCode != 403 {
				t.Fatalf("unauthorized WebSocket reached101upgrade: %v %+v", err, response)
			}
		})
	}
	validCookie := f.authCookie(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{"https://attacker.invalid"}, "Cookie": []string{validCookie.Name + "=" + validCookie.Value}}})
	if conn != nil {
		_ = conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatal("valid cookie bypassed exact WebSocket Origin")
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	otherCookie := f.authCookie(t, func(c *Claims) { c.Subject = "2" })
	mismatch := f.websocketPath(t, "/v1/sessions/"+id+"/terminal", gatewayTestOrigin, otherCookie)
	first := f.ticket(t, func(c *Claims) { c.Scopes = []string{"terminal"} })
	gatewayWriteControl(t, ctx, mismatch, map[string]string{"ticket": first})
	gatewayPolicyClose(t, ctx, mismatch)
	otherOwner := f.websocketPath(t, "/v1/sessions/"+id+"/terminal", gatewayTestOrigin, otherCookie)
	otherFrame := f.ticket(t, func(c *Claims) { c.Subject = "2"; c.Scopes = []string{"terminal"} })
	gatewayWriteControl(t, ctx, otherOwner, map[string]string{"ticket": otherFrame})
	gatewayPolicyClose(t, ctx, otherOwner)
	select {
	case size := <-f.ssh.dimensions:
		t.Fatalf("mismatched preliminary/first-frame actor requested a PTY: %+v", size)
	default:
	}
	// A generic valid Bearer is also an allowed pre-upgrade proof; the first
	// frame must still independently carry the terminal-scoped ticket.
	generic := f.ticket(t, func(c *Claims) { c.Scopes = []string{"diagnostics"} })
	compatible, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{gatewayTestOrigin}, "Authorization": []string{"Bearer " + generic}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = compatible.CloseNow() }()
	gatewayWriteControl(t, ctx, compatible, map[string]string{"ticket": first})
	gatewayReadBinaryUntil(t, ctx, compatible, "fixture-ready")
	// Neither URL parameters nor Sec-WebSocket-Protocol replace cookie/Bearer
	// authentication. The normal clients never place JWTs in either location.
	query, response, err := websocket.Dial(ctx, address+"?ticket="+generic, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{gatewayTestOrigin}, "Cookie": []string{validCookie.Name + "=" + validCookie.Value}}})
	if query != nil {
		_ = query.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 400 {
		t.Fatal("URL JWT was accepted")
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	protocol, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Origin": []string{gatewayTestOrigin}}, Subprotocols: []string{generic}})
	if protocol != nil {
		_ = protocol.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != 403 {
		t.Fatal("WebSocket subprotocol JWT became authentication")
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func TestNativeGatewayDownloadCookieOwnerFailuresDoNotConsumeCapability(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	if err := os.WriteFile(filepath.Join(f.ssh.home, "owned.txt"), []byte("owned-native-file"), 0600); err != nil {
		t.Fatal(err)
	}
	id, scoped := f.open(t)
	issue := func() string {
		t.Helper()
		result := f.request(t, "POST", "/v1/sessions/"+id+"/files/download-ticket", scoped, gatewayTestOrigin, map[string]any{"paths": []string{"owned.txt"}})
		if result.status != 201 {
			t.Fatalf("capability failed %d %s", result.status, result.body)
		}
		return gatewayString(t, gatewayData(t, result), "url")
	}
	address := issue()
	expired := f.authCookie(t, func(c *Claims) {
		c.IssuedAt = jwt.NewNumericDate(time.Now().Add(-2 * time.Minute))
		c.NotBefore = c.IssuedAt
		c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	})
	for name, cookie := range map[string]*http.Cookie{"missing": nil, "malformed": {Name: "__Host-hpc_gateway_jwt", Value: "invalid"}, "expired": expired, "application HS JWT": {Name: "__Host-hpc_gateway_jwt", Value: f.applicationLoginJWT(t)}, "another actor": f.authCookie(t, func(c *Claims) { c.Subject = "2" }), "another username": f.authCookie(t, func(c *Claims) { c.Username = "other" }), "another cluster": f.authCookie(t, func(c *Claims) { c.ClusterID = "target" })} {
		t.Run(name, func(t *testing.T) {
			var cookies []*http.Cookie
			if cookie != nil {
				cookies = []*http.Cookie{cookie}
			}
			got := f.request(t, "GET", address, "", "", nil, cookies...)
			if got.status != 403 || bytes.Contains(got.body, []byte("owned-native-file")) {
				t.Fatalf("download authorized without matching gateway actor: %d %s", got.status, got.body)
			}
		})
	}
	generic := f.ticket(t, func(c *Claims) { c.Scopes = []string{"session:create"} })
	authenticated := f.request(t, "POST", "/v1/auth/session", generic, gatewayTestOrigin, map[string]any{})
	if authenticated.status != 200 {
		t.Fatal("generic scoped JWT did not create a native-download cookie")
	}
	cookie := gatewayResponseCookie(t, authenticated)
	if badHeader := f.request(t, "GET", address, f.applicationLoginJWT(t), "", nil, cookie); badHeader.status != 403 {
		t.Fatal("invalid Authorization fell back to download cookie")
	}
	valid := f.request(t, "GET", address, "", "", nil, cookie)
	if valid.status != 200 || string(valid.body) != "owned-native-file" {
		t.Fatalf("bad-cookie probes consumed the legitimate one-time capability: %d %s", valid.status, valid.body)
	}
	if replay := f.request(t, "GET", address, "", "", nil, cookie); replay.status != 404 {
		t.Fatal("valid cookie made one-time capability reusable")
	}
	// Native capability consumption needs an actor proof, not another SFTP
	// scope: the capability was already minted under the SFTP-scoped action.
	valid = f.request(t, "GET", issue(), f.ticket(t, func(c *Claims) { c.Scopes = []string{"terminal"} }), "", nil)
	if valid.status != 200 || string(valid.body) != "owned-native-file" {
		t.Fatal("generic valid Bearer could not consume its owned native capability")
	}
}

func TestNativeGatewayTransferUsesOrdinaryTargetAccessAndPreservesAuthorization(t *testing.T) {
	f := newGatewayIntegration(t, nil)
	id, ticket := f.open(t)
	source := filepath.Join(f.ssh.home, "source with space.txt")
	if err := os.WriteFile(source, []byte("ordinary transfer fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	target := f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(time.Minute), func(c *ssh.Certificate) { c.KeyId = "web-access/target/access/user:1/1" })
	input := func(value credential) map[string]any {
		return map[string]any{"destination_cluster_id": "target", "source_paths": []string{source}, "destination_path": "/HOME/user/results", "private_key": value.PrivateKey, "certificate": value.Certificate}
	}
	for _, keyID := range []string{"web-access/source/access/user:1/1", "web-access/target/access/user:2/1", "web-access/target/transfer/user:1/1"} {
		invalid := f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(time.Minute), func(c *ssh.Certificate) { c.KeyId = keyID })
		got := f.request(t, "POST", "/v1/sessions/"+id+"/transfers", ticket, gatewayTestOrigin, input(invalid))
		if got.status != 400 {
			t.Fatalf("wrong target identity %s accepted: %d %s", keyID, got.status, got.body)
		}
	}
	for name, edit := range map[string]func(*Claims){"no transfer scope": func(c *Claims) { c.Scopes = []string{"sftp"} }, "unapproved destination": func(c *Claims) { c.DestinationClusterIDs = nil }} {
		got := f.request(t, "POST", "/v1/sessions/"+id+"/transfers", f.ticket(t, edit), gatewayTestOrigin, input(target))
		if got.status < 400 {
			t.Fatalf("%s bypassed authorization", name)
		}
	}
	f.ssh.mu.Lock()
	f.ssh.allowRunnerExec = true
	f.ssh.mu.Unlock()
	got := f.request(t, "POST", "/v1/sessions/"+id+"/transfers", ticket, gatewayTestOrigin, input(target))
	if got.status != 201 {
		t.Fatalf("ordinary access target needs special credentials: %d %s", got.status, got.body)
	}
	transferID := gatewayString(t, gatewayData(t, got), "transfer_id")
	dir := filepath.Join(f.ssh.home, ".go-ldap-admin-transfers", transferID)
	for name, want := range map[string]string{"identity": target.PrivateKey, "identity-cert.pub": target.Certificate} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "identity" {
			original, originalErr := ssh.ParsePrivateKey([]byte(want))
			encoded, encodedErr := ssh.ParsePrivateKey(content)
			if originalErr != nil || encodedErr != nil || !bytes.Equal(original.PublicKey().Marshal(), encoded.PublicKey().Marshal()) {
				t.Fatal("runner private-key encoding changed the ordinary key pair")
			}
		} else if string(content) != want {
			t.Fatal("runner changed the issued ordinary certificate")
		}
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("temporary transfer credentials are not private")
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("temporary transfer directory is not private")
	}
	f.ssh.mu.Lock()
	commands := append([]string{}, f.ssh.runnerCommands...)
	f.ssh.mu.Unlock()
	if len(commands) != 1 || !strings.Contains(commands[0], "web-rsync-"+transferID) {
		t.Fatal("ordinary credential did not reach the screen runner")
	}
}

func TestNativeGatewayBrowserPKCS8TransferKeyAuthenticatesWithOpenSSH(t *testing.T) {
	cli, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("OpenSSH CLI is required for private-key format interoperability")
	}
	f := newGatewayIntegration(t, nil)
	id, ticket := f.open(t)
	source := filepath.Join(f.ssh.home, "source.txt")
	if err := os.WriteFile(source, []byte("CLI authentication fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	target := f.certificate(t, "user", time.Now().Add(-time.Second), time.Now().Add(time.Minute), func(c *ssh.Certificate) { c.KeyId = "web-access/target/access/user:1/1" })
	encoded, err := x509.MarshalPKCS8PrivateKey(f.userKey)
	if err != nil {
		t.Fatal(err)
	}
	// Match WebCrypto's actual Ed25519 export, which Go SSH can parse but
	// OpenSSH CLI does not load as an Ed25519 identity file.
	target.PrivateKey = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}))
	f.ssh.mu.Lock()
	f.ssh.allowRunnerExec = true
	f.ssh.mu.Unlock()
	created := f.request(t, "POST", "/v1/sessions/"+id+"/transfers", ticket, gatewayTestOrigin, map[string]any{"destination_cluster_id": "target", "source_paths": []string{source}, "destination_path": "/HOME/user/results", "private_key": target.PrivateKey, "certificate": target.Certificate})
	if created.status != 201 {
		t.Fatalf("browser PKCS8 credential was rejected: %d %s", created.status, created.body)
	}
	dir := filepath.Join(f.ssh.home, ".go-ldap-admin-transfers", gatewayString(t, gatewayData(t, created), "transfer_id"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, cli, "-F", filepath.Join(dir, "ssh_config"), "-T", "destination")
	command.Env = append(os.Environ(), "SSH_AUTH_SOCK=")
	const marker = "OPENSSH_CERTIFICATE_AUTH_OK\n"
	command.Stdin = strings.NewReader(marker)
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(marker)) {
		t.Fatalf("OpenSSH could not authenticate with the runner's ordinary certificate: %v %s", err, output)
	}
	stored, err := os.ReadFile(filepath.Join(dir, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ssh.ParsePrivateKey(stored)
	original, originalErr := ssh.NewPublicKey(f.userKey.Public())
	if err != nil || originalErr != nil || !bytes.Equal(identity.PublicKey().Marshal(), original.Marshal()) {
		t.Fatal("private-key re-encoding changed the authenticated public key")
	}
	certificate, err := os.ReadFile(filepath.Join(dir, "identity-cert.pub"))
	if err != nil || string(certificate) != target.Certificate {
		t.Fatal("runner changed the issued ordinary certificate")
	}
}
