package slave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

const maxSnapshotBytes = 64 << 20

var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// TXTResolver looks up DNS TXT records (net.Resolver in production).
type TXTResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// DirectoryCloser is a Directory bound to one LDAP session.
type DirectoryCloser interface {
	Directory
	Close() error
}

// Replicator keeps one cluster in step with master. It watches the snapshot hash in DNS, downloads a
// changed snapshot over mTLS, applies the difference to LDAP, publishes SSH trust files and queues
// trigger events.
type Replicator struct {
	MasterURL    string
	DNSName      string
	Resolver     TXTResolver
	Client       *http.Client
	StateDir     string
	SourceBaseDN string
	LocalBaseDN  string
	LocalGroupDN string
	AdminDN      string
	Dial         func() (DirectoryCloser, error)
	TrustDirs    []string
	Queue        *Queue
	CheckTimeout time.Duration
	DNSTimeout   time.Duration
	Now          func() time.Time
	Telemetry    telemetry.Provider
	// Limits guard every sync against mass deletion and dropping a trusted SSH CA.
	Limits Limits

	gate    chan struct{}
	mu      sync.RWMutex
	hash    string
	applied replication.Snapshot
	dirty   bool
	lastErr string
	lastOK  time.Time
}

type persisted struct {
	SourceHash string               `json:"sourceHash"`
	Snapshot   replication.Snapshot `json:"snapshot"`
}

func (r *Replicator) statePath() string { return filepath.Join(r.StateDir, "applied.json") }
func (r *Replicator) dirtyPath() string { return filepath.Join(r.StateDir, "dirty") }

// Open validates configuration and loads persisted state; a leftover dirty marker forces a reconcile.
func (r *Replicator) Open() error {
	u, err := url.Parse(r.MasterURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("master URL must be an HTTPS origin")
	}
	r.MasterURL = strings.TrimRight(r.MasterURL, "/")
	if r.StateDir == "" || r.Dial == nil || r.Client == nil || r.LocalBaseDN == "" {
		return errors.New("replicator needs state dir, LDAP dialer, mTLS client and local base DN")
	}
	if t, ok := r.Client.Transport.(*http.Transport); !ok || t.TLSClientConfig == nil || len(t.TLSClientConfig.Certificates) == 0 || t.TLSClientConfig.InsecureSkipVerify {
		return errors.New("verified mutual TLS client is required")
	}
	if r.Resolver == nil {
		r.Resolver = net.DefaultResolver
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.CheckTimeout == 0 {
		r.CheckTimeout = 10 * time.Minute
	}
	if r.DNSTimeout == 0 {
		r.DNSTimeout = 5 * time.Second
	}
	if r.Telemetry == nil {
		r.Telemetry = telemetry.Noop{}
	}
	r.gate = make(chan struct{}, 1)
	if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
		return err
	}
	raw, err := os.ReadFile(r.statePath())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		var p persisted
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("corrupt replication state: %w", err)
		}
		r.hash, r.applied = p.SourceHash, p.Snapshot
	}
	if _, err := os.Stat(r.dirtyPath()); err == nil {
		r.dirty = true
	}
	if r.applied.Version == 0 {
		r.applied = replication.Snapshot{Version: replication.Version}
	}
	return nil
}

// desiredHash reads master's current snapshot hash: from the DNS TXT record when DNSName is set,
// otherwise from master's peer-only hash endpoint.
func (r *Replicator) desiredHash(ctx context.Context) (string, error) {
	dctx, cancel := context.WithTimeout(ctx, r.DNSTimeout)
	defer cancel()
	var vals []string
	var err error
	if r.DNSName != "" {
		vals, err = r.Resolver.LookupTXT(dctx, r.DNSName)
	} else {
		vals, err = r.httpHash(dctx)
	}
	if err != nil {
		return "", fmt.Errorf("look up data hash: %w", err)
	}
	var want string
	for _, v := range vals {
		v = strings.Trim(strings.TrimSpace(v), `"`)
		h, ok := strings.CutPrefix(v, replication.TXTPrefix)
		if !ok || !hashPattern.MatchString(h) {
			continue
		}
		if want != "" && want != h {
			return "", errors.New("conflicting data hash TXT records")
		}
		want = h
	}
	if want == "" {
		return "", errors.New("data hash TXT record is missing")
	}
	return want, nil
}

// httpHash asks master for the hash over the same mutual-TLS client used for snapshots.
func (r *Replicator) httpHash(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.MasterURL+replication.HashPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return []string{string(body)}, nil
}

func (r *Replicator) fetch(ctx context.Context) (replication.Snapshot, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.MasterURL+replication.SnapshotPath, nil)
	if err != nil {
		return replication.Snapshot{}, "", err
	}
	telemetry.Inject(ctx, req)
	resp, err := r.Client.Do(req)
	if err != nil {
		return replication.Snapshot{}, "", fmt.Errorf("fetch snapshot: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return replication.Snapshot{}, "", fmt.Errorf("fetch snapshot: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return replication.Snapshot{}, "", err
	}
	if len(body) > maxSnapshotBytes {
		return replication.Snapshot{}, "", errors.New("snapshot exceeds size limit")
	}
	var s replication.Snapshot
	if err := json.Unmarshal(body, &s); err != nil {
		return replication.Snapshot{}, "", err
	}
	h, err := s.Hash()
	if err != nil {
		return replication.Snapshot{}, "", err
	}
	if resp.Header.Get("X-Data-Hash") != h {
		return replication.Snapshot{}, "", errors.New("snapshot hash mismatch")
	}
	return s, h, nil
}

// Check converges local state on master's current snapshot; concurrent callers are serialised.
func (r *Replicator) Check(ctx context.Context) (err error) {
	ctx, cancel := context.WithTimeout(ctx, r.CheckTimeout)
	defer cancel()
	select {
	case r.gate <- struct{}{}:
		defer func() { <-r.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	ctx, span := telemetry.Start(ctx, r.Telemetry, "slave.check", telemetry.SpanContext{})
	defer func() {
		if err != nil {
			span.RecordError(err)
		}
		span.End()
	}()
	defer func() {
		r.mu.Lock()
		if err != nil {
			r.lastErr = err.Error()
		} else {
			r.lastErr, r.lastOK = "", r.Now()
		}
		r.mu.Unlock()
	}()

	r.mu.RLock()
	cur, dirty, applied := r.hash, r.dirty, r.applied
	r.mu.RUnlock()

	want, err := r.desiredHash(ctx)
	if err != nil {
		return err
	}
	if want == cur && !dirty {
		return r.dispatch(ctx, applied)
	}
	next, h, err := r.fetch(ctx)
	if err != nil {
		return err
	}
	if h == cur && !dirty {
		return r.dispatch(ctx, applied)
	}
	source := r.SourceBaseDN
	if source == "" {
		source = next.BaseDN
	}
	if source != "" && (!strings.EqualFold(source, r.LocalBaseDN) || r.LocalGroupDN != "") {
		if next, err = RewriteDNs(next, source, r.LocalBaseDN, r.LocalGroupDN); err != nil {
			return fmt.Errorf("transform snapshot: %w", err)
		}
	} else {
		next.BaseDN = r.LocalBaseDN
	}
	if err := r.setDirty(true); err != nil {
		return err
	}
	dir, err := r.Dial()
	if err != nil {
		return fmt.Errorf("connect LDAP: %w", err)
	}
	if cur == "" {
		// First sync on an existing directory: diff against what LDAP and sshd hold now, not an
		// empty baseline, so the guard sees real deletions and unchanged entries are left alone.
		if applied, err = r.baseline(dir); err != nil {
			dir.Close()
			return fmt.Errorf("read existing directory: %w", err)
		}
		applied = Baseline(applied, next)
	}
	if err := Guard(applied, next, r.Limits); err != nil {
		dir.Close()
		return err
	}
	err = Reconcile(ctx, dir, applied, next, r.LocalBaseDN, r.AdminDN)
	dir.Close()
	if err != nil {
		return fmt.Errorf("reconcile LDAP: %w", err)
	}
	for _, d := range r.TrustDirs {
		if err := PublishTrust(d, next, r.Now()); err != nil {
			return fmt.Errorf("publish trust %s: %w", d, err)
		}
	}
	if r.Queue != nil {
		if err := r.Queue.Enqueue(applied, next); err != nil {
			return fmt.Errorf("enqueue triggers: %w", err)
		}
	}
	raw, err := json.Marshal(persisted{SourceHash: h, Snapshot: next})
	if err != nil {
		return err
	}
	if err := writeAtomic(r.statePath(), raw); err != nil {
		return err
	}
	if err := r.setDirty(false); err != nil {
		return err
	}
	r.mu.Lock()
	r.hash, r.applied = h, next
	r.mu.Unlock()
	return r.dispatch(ctx, next)
}

func (r *Replicator) setDirty(on bool) error {
	var err error
	if on {
		err = writeAtomic(r.dirtyPath(), []byte("reconcile in progress\n"))
	} else if err = os.Remove(r.dirtyPath()); errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		r.mu.Lock()
		r.dirty = on
		r.mu.Unlock()
	}
	return err
}

func (r *Replicator) dispatch(ctx context.Context, current replication.Snapshot) error {
	if r.Queue == nil {
		return nil
	}
	return r.Queue.Dispatch(ctx, current)
}

// Run checks immediately and then every interval after the previous check completes; report sees every outcome.
func (r *Replicator) Run(ctx context.Context, interval time.Duration, report func(error)) {
	for {
		err := r.Check(ctx)
		if report != nil {
			report(err)
		}
		if interval <= 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Status is what /readyz reports.
type Status struct {
	Ready     bool      `json:"ready"`
	Hash      string    `json:"hash,omitempty"`
	Dirty     bool      `json:"dirty"`
	LastError string    `json:"lastError,omitempty"`
	LastOK    time.Time `json:"lastOk,omitzero"`
}

// Status returns the outcome of the last sync.
func (r *Replicator) Status() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Status{Ready: r.hash != "" && !r.dirty, Hash: r.hash, Dirty: r.dirty, LastError: r.lastErr, LastOK: r.lastOK}
}

// Username returns the account name master assigned to uid in the last applied snapshot.
func (r *Replicator) Username(uid int64) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, u := range r.applied.Users {
		if u.UIDNumber == uid && u.Username != "" {
			return u.Username, true
		}
	}
	return "", false
}

// HealthHandler serves /healthz and /readyz from memory only; it never blocks on LDAP or the network.
func (r *Replicator) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		s := r.Status()
		w.Header().Set("Content-Type", "application/json")
		if !s.Ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		s.LastError = ""
		_ = json.NewEncoder(w).Encode(s)
	})
	return mux
}

// baseline observes the live directory and the SSH CAs sshd trusts through the first trust dir.
func (r *Replicator) baseline(dir DirectoryCloser) (replication.Snapshot, error) {
	reader, ok := dir.(interface{ SearchEntries(string) ([]Entry, error) })
	if !ok {
		return replication.Snapshot{}, errors.New("directory cannot be read")
	}
	entries, err := reader.SearchEntries(r.LocalBaseDN)
	if err != nil {
		return replication.Snapshot{}, err
	}
	s := Observed(entries, r.LocalBaseDN, r.AdminDN)
	if len(r.TrustDirs) > 0 {
		if s.CAs, err = TrustedCAs(r.TrustDirs[0]); err != nil {
			return replication.Snapshot{}, err
		}
	}
	return s, nil
}
