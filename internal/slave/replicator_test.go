package slave_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/slave"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/testutil"
)

type pki struct {
	pool   *x509.CertPool
	server tls.Certificate
	client tls.Certificate
	rogue  tls.Certificate
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, tmpl *x509.Certificate) tls.Certificate {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &k.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k, Leaf: leaf}
}

func newPKI(t *testing.T) pki {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := func(n int64, cn string, usage x509.ExtKeyUsage) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(n), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature}
	}
	return pki{pool: pool,
		server: issue(t, ca, caKey, leaf(2, "master.test", x509.ExtKeyUsageServerAuth)),
		client: issue(t, ca, caKey, leaf(3, "cluster1-slave", x509.ExtKeyUsageClientAuth)),
		rogue:  issue(t, ca, caKey, leaf(4, "rogue", x509.ExtKeyUsageClientAuth))}
}

type resolver struct {
	mu  sync.Mutex
	txt string
}

func (r *resolver) set(h string) {
	r.mu.Lock()
	r.txt = `"` + replication.TXTPrefix + h + `"`
	r.mu.Unlock()
}
func (r *resolver) LookupTXT(context.Context, string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return []string{"unrelated", r.txt}, nil
}

type env struct {
	db      store.Store
	srv     *httptest.Server
	res     *resolver
	fetches atomic.Int64
	dir     *fakeDirectory
	pki     pki
}

func setup(t *testing.T) *env {
	t.Helper()
	db := testutil.DB(t)
	e := &env{db: db, res: &resolver{}, pki: newPKI(t), dir: &fakeDirectory{entries: map[string]bool{}}}
	sum := sha256.Sum256(e.pki.client.Certificate[0])
	cfg := &config.Config{Role: "master", HTTP: config.HTTP{RatePerSecond: 1e6, RateBurst: 1e6},
		Auth:      config.Auth{Issuer: "i", Audience: "a", TokenTTL: time.Hour},
		JWTKeys:   authn.Keyring{ActiveKID: "k", Keys: map[string][]byte{"k": []byte("0123456789abcdef0123456789abcdef")}},
		Directory: config.Directory{BaseDN: "dc=test", UserDN: "ou=people,dc=test"},
		Sync:      config.Sync{PeerFingerprints: []string{hex.EncodeToString(sum[:])}}}
	a, err := app.New(context.Background(), cfg, db)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.fetches.Add(1)
		a.Handler.ServeHTTP(w, r)
	}))
	e.srv.TLS = &tls.Config{Certificates: []tls.Certificate{e.pki.server}, ClientCAs: e.pki.pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	e.srv.StartTLS()
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) publish(t *testing.T) {
	t.Helper()
	_, h, err := replication.Load(context.Background(), e.db, "dc=test")
	if err != nil {
		t.Fatal(err)
	}
	e.res.set(h)
}

func (e *env) replicator(t *testing.T, stateDir string, cert tls.Certificate) *slave.Replicator {
	t.Helper()
	r := &slave.Replicator{MasterURL: e.srv.URL, DNSName: "_state.test", Resolver: e.res, StateDir: stateDir, LocalBaseDN: "dc=test",
		Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.pki.pool, ServerName: "master.test",
			Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}},
		Dial: func() (slave.DirectoryCloser, error) { return e.dir, nil }}
	if err := r.Open(); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestReplicatorEndToEnd(t *testing.T) {
	ctx := context.Background()
	e := setup(t)
	alice := testutil.User(t, e.db, "alice", 1)
	bob := testutil.User(t, e.db, "bob", 1)
	_ = bob
	e.publish(t)
	state := t.TempDir()
	r := e.replicator(t, state, e.pki.client)
	if r.Status().Ready {
		t.Fatal("ready before first sync")
	}
	if err := r.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if !r.Status().Ready || !e.dir.has("uid=alice,ou=people,dc=test") || !e.dir.has("uid=bob,ou=people,dc=test") {
		t.Fatalf("first sync incomplete: %+v %v", r.Status(), e.dir.entries)
	}
	if fi, err := os.Stat(filepath.Join(state, "applied.json")); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state file missing or too permissive: %v", err)
	}

	before := e.fetches.Load()
	if err := r.Check(ctx); err != nil || e.fetches.Load() != before {
		t.Fatalf("unchanged hash still fetched (err %v)", err)
	}

	if _, err := e.db.Exec(ctx, "UPDATE users SET status = 2 WHERE id = ?", alice); err != nil {
		t.Fatal(err)
	}
	e.publish(t)
	e.dir.failOn = "uid=bob"
	if _, err := e.db.Exec(ctx, "UPDATE users SET mail = 'b@example.org' WHERE username = 'bob'"); err != nil {
		t.Fatal(err)
	}
	e.publish(t)
	if err := r.Check(ctx); err == nil || r.Status().Ready {
		t.Fatal("failed reconcile reported ready")
	}
	if _, err := os.Stat(filepath.Join(state, "dirty")); err != nil {
		t.Fatal("dirty marker missing after failure")
	}

	restarted := e.replicator(t, state, e.pki.client)
	if restarted.Status().Ready {
		t.Fatal("restart ignored dirty marker")
	}
	if err := restarted.Check(ctx); err != nil || !restarted.Status().Ready {
		t.Fatalf("recovery failed: %v", err)
	}
	if e.dir.has("uid=alice,ou=people,dc=test") {
		t.Fatal("disabled user still in LDAP")
	}

	rogue := e.replicator(t, t.TempDir(), e.pki.rogue)
	if err := rogue.Check(ctx); err == nil {
		t.Fatal("unpinned client certificate fetched the snapshot")
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+replication.SnapshotPath, nil)
	tok := "Bearer invalid"
	req.Header.Set("Authorization", tok)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: e.pki.pool, ServerName: "master.test", Certificates: []tls.Certificate{e.pki.rogue}}}}
	if resp, err := client.Do(req); err == nil && resp.StatusCode == http.StatusOK {
		t.Fatal("snapshot served to non-pinned peer")
	}
}

// fakeDirectory records presence only; the reconcile logic is covered in package tests.
type fakeDirectory struct {
	mu      sync.Mutex
	entries map[string]bool
	failOn  string
}

func (f *fakeDirectory) has(dn string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.entries[dn] }
func (f *fakeDirectory) fail(dn string) error {
	if f.failOn != "" && len(dn) >= len(f.failOn) && dn[:len(f.failOn)] == f.failOn {
		f.failOn = ""
		return os.ErrDeadlineExceeded
	}
	return nil
}
func (f *fakeDirectory) Close() error { return nil }

// SearchEntries reports an empty pre-existing directory: the test starts from a fresh LDAP.
func (f *fakeDirectory) SearchEntries(string) ([]slave.Entry, error) { return nil, nil }
func (f *fakeDirectory) AddGroup(_ context.Context, g replication.Group) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[g.DN] = true
	return nil
}
func (f *fakeDirectory) UpdateGroup(context.Context, replication.Group) error { return nil }
func (f *fakeDirectory) Delete(_ context.Context, dn string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, dn)
	return nil
}
func (f *fakeDirectory) AddUser(_ context.Context, u replication.User, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail(u.DN); err != nil {
		return err
	}
	if f.entries[u.DN] {
		return slave.ErrExists
	}
	f.entries[u.DN] = true
	return nil
}
func (f *fakeDirectory) UpdateUser(_ context.Context, u replication.User) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail(u.DN)
}
func (f *fakeDirectory) SetPassword(context.Context, string, string) error    { return nil }
func (f *fakeDirectory) SetCertificate(context.Context, string, []byte) error { return nil }
func (f *fakeDirectory) ClearCertificate(context.Context, string) error       { return nil }
func (f *fakeDirectory) AddMember(context.Context, replication.Group, replication.User) error {
	return nil
}
func (f *fakeDirectory) RemoveMember(context.Context, replication.Group, replication.User) error {
	return nil
}
