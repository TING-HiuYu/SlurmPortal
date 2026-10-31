package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type session struct {
	id, owner, username, cluster, home, root, purpose string
	client                                            *ssh.Client
	files                                             *sftp.Client
	last                                              time.Time
	refs                                              atomic.Int32
}
type download struct {
	sessionID string
	owner     string
	paths     []string
	expires   time.Time
}

// Gateway serves the browser terminal (WebSocket to SSH), file browsing (SFTP) and cross-cluster
// transfers for one login node. Every request carries a short-lived ticket signed by master.
type Gateway struct {
	cfg               Config
	mu                sync.Mutex
	sessions          map[string]*session
	used              map[string]time.Time
	downloads         map[string]download
	transfers         map[string]*Transfer
	reservations      int
	pendingOwners     map[string]int
	metadataSlots     chan struct{}
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	masterClient      *http.Client
	bootID            string
	registrationError string
	lastRegistered    time.Time
}

// New starts a gateway and registers it with master.
func New(parent context.Context, cfg Config) (*Gateway, error) {
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	if e := privateDirectory(cfg.StateDirectory); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	g := &Gateway{cfg: cfg, sessions: map[string]*session{}, used: map[string]time.Time{}, downloads: map[string]download{}, transfers: map[string]*Transfer{}, ctx: ctx, cancel: cancel, bootID: opaque(), pendingOwners: map[string]int{}, metadataSlots: make(chan struct{}, 4)}
	if e := g.loadTransfers(); e != nil {
		cancel()
		return nil, e
	}
	var e error
	g.masterClient, e = masterHTTPClient(cfg)
	if e != nil {
		cancel()
		return nil, e
	}
	g.wg.Add(1)
	go g.background()
	if cfg.MasterOrigin != "" {
		g.registrationError = "registration_pending"
		g.wg.Add(1)
		go g.registrationLoop()
	}
	return g, nil
}
func privateDirectory(path string) error {
	if e := os.MkdirAll(path, 0700); e != nil {
		return e
	}
	st, e := os.Lstat(path)
	if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0700 {
		return errors.New("directory must be private 0700")
	}
	return nil
}
func opaque() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic("system random unavailable")
	}
	return hex.EncodeToString(b[:])
}

// Close stops background work and closes open sessions.
func (g *Gateway) Close() {
	g.cancel()
	g.mu.Lock()
	sessions := g.sessions
	g.sessions = map[string]*session{}
	g.mu.Unlock()
	for _, s := range sessions {
		_ = s.client.Close()
		_ = s.files.Close()
	}
	g.wg.Wait()
	if tr, ok := g.masterClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}
func (g *Gateway) background() {
	defer g.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			g.prune()
			g.flushOutbox()
		}
	}
}
func (g *Gateway) prune() {
	now := time.Now()
	var closeList []*session
	g.mu.Lock()
	for id, s := range g.sessions {
		if s.refs.Load() == 0 && now.Sub(s.last) > time.Duration(g.cfg.SessionIdleSeconds)*time.Second {
			delete(g.sessions, id)
			closeList = append(closeList, s)
		}
	}
	for id, at := range g.used {
		if !at.After(now) {
			delete(g.used, id)
		}
	}
	for id, v := range g.downloads {
		if !v.expires.After(now) {
			delete(g.downloads, id)
		}
	}
	g.mu.Unlock()
	for _, s := range closeList {
		_ = s.client.Close()
		_ = s.files.Close()
	}
}
func (g *Gateway) origin(r *http.Request) bool {
	value := r.Header.Get("Origin")
	for _, o := range g.cfg.AllowedOrigins {
		if value == o {
			return true
		}
	}
	return false
}
func (g *Gateway) cors(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if g.origin(r) {
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization,Content-Type")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Disposition,Content-Length")
	}
}
func reply(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data})
}
func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "error": code, "message": code})
}
func body(r *http.Request, v any) error {
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		return errors.New("JSON required")
	}
	d := json.NewDecoder(io.LimitReader(r.Body, 131073))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("invalid body")
	}
	return nil
}
func (g *Gateway) auth(r *http.Request, scope string) (Claims, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		if c, e := r.Cookie(gatewayCookieName); e == nil {
			return g.cfg.VerifyTicket(c.Value, scope, time.Now())
		}
	}
	if !strings.HasPrefix(h, "Bearer ") {
		return Claims{}, errors.New("ticket required")
	}
	return g.cfg.VerifyTicket(strings.TrimPrefix(h, "Bearer "), scope, time.Now())
}
func (g *Gateway) getSession(id string, c Claims) (*session, func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[id]
	if s == nil || s.owner != c.Subject || s.username != c.Username || s.cluster != c.ClusterID {
		return nil, nil, errors.New("session unavailable")
	}
	s.last = time.Now()
	s.refs.Add(1)
	return s, func() { s.refs.Add(-1) }, nil
}

// ServeHTTP routes terminal, file and transfer requests after verifying the ticket.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.cors(w, r)
	if r.TLS == nil {
		fail(w, 400, "https_required")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, 400, "query_not_allowed")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/healthz" {
		reply(w, 200, map[string]string{"status": "ok"})
		return
	}
	if r.Method == "GET" && r.URL.Path == "/readyz" {
		if code := g.configurationError(); code != "" {
			fail(w, 503, code)
		} else {
			reply(w, 200, map[string]string{"status": "ready"})
		}
		return
	}
	if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/downloads/") {
		g.downloadOnce(w, r)
		return
	}
	if !g.origin(r) {
		fail(w, 403, "origin_denied")
		return
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/v1/auth/session" {
		g.authSession(w, r)
		return
	}
	if r.Method == "POST" && r.URL.Path == "/v1/diagnostics" {
		g.diagnostics(w, r)
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/diagnostics/socket" {
		g.diagnosticSocket(w, r)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 2 && parts[0] == "v1" && parts[1] == "sessions" && r.Method == "POST" {
		g.open(w, r)
		return
	}
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "sessions" {
		id := parts[2]
		if len(parts) == 4 && parts[3] == "terminal" && r.Method == "GET" {
			g.terminal(w, r, id)
			return
		}
		scope := "sftp"
		if len(parts) == 4 && parts[3] == "transfers" {
			scope = "transfer"
		}
		if len(parts) == 3 {
			scope = "session:create"
		}
		c, e := g.auth(r, scope)
		if e != nil {
			fail(w, 401, "ticket_invalid")
			return
		}
		s, done, e := g.getSession(id, c)
		if e != nil {
			fail(w, 404, "session_unavailable")
			return
		}
		defer done()
		switch {
		case len(parts) == 3 && r.Method == "DELETE":
			g.mu.Lock()
			delete(g.sessions, id)
			g.mu.Unlock()
			_ = s.client.Close()
			_ = s.files.Close()
			reply(w, 200, map[string]bool{"closed": true})
		case len(parts) == 5 && parts[3] == "files" && parts[4] == "list" && r.Method == "POST":
			g.list(w, r, s)
		case len(parts) == 5 && parts[3] == "files" && parts[4] == "download-ticket" && r.Method == "POST":
			g.downloadTicket(w, r, s)
		case len(parts) == 4 && parts[3] == "download" && r.Method == "POST":
			g.downloadDirect(w, r, s)
		case len(parts) == 4 && parts[3] == "transfers" && r.Method == "POST":
			g.startTransfer(w, r, s, c)
		default:
			fail(w, 404, "not_found")
		}
		return
	}
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "transfers" && r.Method == "GET" {
		if len(parts) == 4 && parts[3] == "logs" {
			g.logs(w, r, parts[2])
			return
		}
		if len(parts) == 3 {
			c, e := g.auth(r, "transfer:read")
			if e != nil {
				fail(w, 401, "ticket_invalid")
				return
			}
			g.mu.Lock()
			t := g.transfers[parts[2]]
			if t == nil || t.UserID != c.Subject || t.Username != c.Username {
				g.mu.Unlock()
				fail(w, 404, "transfer_unavailable")
				return
			}
			v := t.public()
			g.mu.Unlock()
			reply(w, 200, v)
			return
		}
	}
	fail(w, 404, "not_found")
}

type credential struct {
	PrivateKey  string `json:"private_key"`
	Certificate string `json:"certificate"`
}

func (g *Gateway) signer(value credential, username string) (ssh.Signer, error) {
	if len(value.PrivateKey) > 32768 || len(value.Certificate) > 32768 {
		return nil, errors.New("credential too large")
	}
	key, e := ssh.ParsePrivateKey([]byte(value.PrivateKey))
	if e != nil {
		return nil, errors.New("invalid ephemeral key")
	}
	pub, _, _, rest, e := ssh.ParseAuthorizedKey([]byte(value.Certificate))
	if e != nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid certificate")
	}
	cert, ok := pub.(*ssh.Certificate)
	if !ok || cert.CertType != ssh.UserCert || !bytes.Equal(cert.Key.Marshal(), key.PublicKey().Marshal()) || cert.ValidBefore <= cert.ValidAfter || cert.ValidBefore-cert.ValidAfter > 300 {
		return nil, errors.New("invalid ephemeral certificate")
	}
	trusted := false
	userCAs, err := g.cfg.trustedUserCAs()
	if err != nil {
		return nil, err
	}
	for _, ca := range userCAs {
		if bytes.Equal(ca.Marshal(), cert.SignatureKey.Marshal()) {
			trusted = true
		}
	}
	if !trusted {
		return nil, errors.New("untrusted CA")
	}
	check := ssh.CertChecker{Clock: time.Now}
	if check.CheckCert(username, cert) != nil {
		return nil, errors.New("certificate rejected")
	}
	return ssh.NewCertSigner(cert, key)
}
func certificatePurpose(signer ssh.Signer, cluster, subject string) (string, error) {
	cert, ok := signer.PublicKey().(*ssh.Certificate)
	if !ok || cert.Serial == 0 {
		return "", errors.New("certificate identity invalid")
	}
	parts := strings.Split(cert.KeyId, "/")
	if len(parts) != 5 || parts[0] != "web-access" || parts[1] != cluster || parts[3] != "user:"+subject || parts[4] != strconv.FormatUint(cert.Serial, 16) {
		return "", errors.New("certificate identity invalid")
	}
	switch parts[2] {
	case "access":
		return parts[2], nil
	}
	return "", errors.New("certificate purpose invalid")
}
func (g *Gateway) dial(ctx context.Context, cluster Cluster, username string, signer ssh.Signer) (*ssh.Client, error) {
	config := &ssh.ClientConfig{User: username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: sshTimeout}
	raw, e := (&net.Dialer{Timeout: sshTimeout}).DialContext(ctx, "tcp", cluster.SSHAddress)
	if e != nil {
		return nil, e
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stopCancel()
	_ = raw.SetDeadline(time.Now().Add(sshTimeout))
	conn, ch, req, e := ssh.NewClientConn(raw, cluster.SSHAddress, config)
	if e != nil {
		_ = raw.Close()
		return nil, e
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(conn, ch, req), nil
}
func (g *Gateway) open(w http.ResponseWriter, r *http.Request) {
	if code := g.configurationError(); code != "" {
		fail(w, 503, code)
		return
	}
	c, e := g.auth(r, "session:create")
	if e != nil {
		fail(w, 401, "ticket_invalid")
		return
	}
	var input struct {
		credential
		ClusterID string `json:"cluster_id"`
	}
	if body(r, &input) != nil || input.ClusterID != c.ClusterID {
		fail(w, 400, "request_invalid")
		return
	}
	signer, e := g.signer(input.credential, c.Username)
	input.PrivateKey = ""
	if e != nil {
		fail(w, 400, "credential_invalid")
		return
	}
	purpose, e := certificatePurpose(signer, c.ClusterID, c.Subject)
	if e != nil || purpose != "access" {
		fail(w, 400, "credential_identity_invalid")
		return
	}
	cert := signer.PublicKey().(*ssh.Certificate)
	if _, ok := cert.Extensions["permit-pty"]; !ok {
		fail(w, 400, "credential_identity_invalid")
		return
	}
	g.prune()
	g.mu.Lock()
	count := 0
	for _, s := range g.sessions {
		if s.owner == c.Subject {
			count++
		}
	}
	if len(g.sessions)+g.reservations >= g.cfg.MaxSessions || count+g.pendingOwners[c.Subject] >= g.cfg.MaxSessionsPerUser || len(g.used) >= 8192 {
		g.mu.Unlock()
		fail(w, 429, "session_limit")
		return
	}
	if _, found := g.used[c.ID]; found {
		g.mu.Unlock()
		fail(w, 409, "ticket_used")
		return
	}
	g.used[c.ID] = c.ExpiresAt.Time
	g.reservations++
	g.pendingOwners[c.Subject]++
	g.mu.Unlock()
	defer func() { g.mu.Lock(); g.reservations--; g.pendingOwners[c.Subject]--; g.mu.Unlock() }()
	cluster, _ := g.cfg.cluster(c.ClusterID)
	client, e := g.dial(r.Context(), cluster, c.Username, signer)
	if e != nil {
		fail(w, 502, "ssh_connection_failed")
		return
	}
	setupTimeout := time.AfterFunc(sshTimeout, func() { _ = client.Close() })
	defer setupTimeout.Stop()
	stopCancel := context.AfterFunc(r.Context(), func() { _ = client.Close() })
	defer stopCancel()
	files, e := sftp.NewClient(client)
	if e != nil {
		_ = client.Close()
		fail(w, 502, "sftp_unavailable")
		return
	}
	home, e := files.RealPath(".")
	if e != nil || !strings.HasPrefix(home, "/") {
		_ = client.Close()
		_ = files.Close()
		fail(w, 502, "home_unavailable")
		return
	}
	s := &session{id: opaque(), owner: c.Subject, username: c.Username, cluster: c.ClusterID, home: home, root: g.cfg.FileRoot, purpose: purpose, client: client, files: files, last: time.Now()}
	g.mu.Lock()
	g.sessions[s.id] = s
	g.mu.Unlock()
	reply(w, 201, map[string]string{"session_id": s.id, "home": home, "file_root": g.cfg.FileRoot})
}
