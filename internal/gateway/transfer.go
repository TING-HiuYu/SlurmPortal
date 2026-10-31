package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"
)

// Transfer is one cross-cluster copy (rsync over SSH) and its outcome, as reported to master.
type Transfer struct {
	Version              int        `json:"version"`
	EventID              string     `json:"event_id"`
	TransferID           string     `json:"transfer_id"`
	GatewayID            string     `json:"gateway_id"`
	ClusterID            string     `json:"cluster_id"`
	UserID               string     `json:"-"`
	NumericUserID        int64      `json:"user_id"`
	Username             string     `json:"username"`
	DestinationClusterID string     `json:"destination_cluster_id"`
	SourcePaths          []string   `json:"source_paths"`
	DestinationPath      string     `json:"destination_path"`
	Status               string     `json:"status"`
	ExitCode             *int       `json:"exit_code"`
	StartedAt            time.Time  `json:"started_at"`
	FinishedAt           *time.Time `json:"finished_at"`
	BytesSent            *int64     `json:"bytes_sent"`
	BytesReceived        *int64     `json:"bytes_received"`
	TotalBytes           *int64     `json:"total_bytes"`
	ElapsedSeconds       *float64   `json:"elapsed_seconds"`
	AverageMBPerSecond   *float64   `json:"average_mb_per_second"`
	Error                *string    `json:"error,omitempty"`
	SessionID            string     `json:"-"`
	RemoteDirectory      string     `json:"-"`
	CallbackHash         string     `json:"-"`
	CallbackExpires      time.Time  `json:"-"`
	Delivered            bool       `json:"-"`
}

func (t *Transfer) public() map[string]any {
	return map[string]any{"transfer_id": t.TransferID, "source_cluster_id": t.ClusterID, "destination_cluster_id": t.DestinationClusterID, "status": t.Status, "exit_code": t.ExitCode, "started_at": t.StartedAt, "finished_at": t.FinishedAt, "bytes_sent": t.BytesSent, "bytes_received": t.BytesReceived, "total_bytes": t.TotalBytes, "elapsed_seconds": t.ElapsedSeconds, "average_mb_per_second": t.AverageMBPerSecond, "error": t.Error}
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'" }
func command(args ...string) string {
	out := make([]string, len(args))
	for i, v := range args {
		out[i] = quote(v)
	}
	return strings.Join(out, " ")
}
func hashCapability(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }

// Browser WebCrypto exports PKCS#8, which the Go SSH client accepts but the
// OpenSSH CLI cannot load for Ed25519. Preserve the key pair and certificate;
// only normalize the verified private key's representation for the runner.
func runnerPrivateKey(value string) (string, error) {
	key, err := ssh.ParseRawPrivateKey([]byte(value))
	if err != nil {
		return "", errors.New("invalid runner private key")
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return "", errors.New("unsupported runner private key")
	}
	return string(pem.EncodeToMemory(block)), nil
}

func (g *Gateway) startTransfer(w http.ResponseWriter, r *http.Request, s *session, c Claims) {
	if code := g.configurationError(); code != "" {
		fail(w, 503, code)
		return
	}
	var input struct {
		credential
		DestinationClusterID string   `json:"destination_cluster_id"`
		SourcePaths          []string `json:"source_paths"`
		DestinationPath      string   `json:"destination_path"`
	}
	if body(r, &input) != nil || !pathsValid(input.SourcePaths) || !path.IsAbs(input.DestinationPath) || len(input.DestinationPath) > 4096 || strings.ContainsAny(input.DestinationPath, "\x00\r\n") {
		fail(w, 400, "request_invalid")
		return
	}
	allowed := false
	for _, id := range c.DestinationClusterIDs {
		if id == input.DestinationClusterID {
			allowed = true
		}
	}
	destination, exists := g.cfg.cluster(input.DestinationClusterID)
	if !allowed || !exists || destination.ID == s.cluster {
		fail(w, 403, "destination_denied")
		return
	}
	signer, e := g.signer(input.credential, c.Username)
	if e != nil {
		fail(w, 400, "credential_invalid")
		return
	}
	if purpose, err := certificatePurpose(signer, destination.ID, c.Subject); err != nil || purpose != "access" {
		fail(w, 400, "credential_identity_invalid")
		return
	}
	input.PrivateKey, e = runnerPrivateKey(input.PrivateKey)
	if e != nil {
		fail(w, 400, "credential_invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	stop := cancelSession(ctx, s)
	defer stop()
	for i, value := range input.SourcePaths {
		resolved, e := remotePath(s, value)
		if e != nil {
			fail(w, 403, "path_unavailable")
			return
		}
		input.SourcePaths[i] = resolved
	}
	g.mu.Lock()
	active := 0
	for _, t := range g.transfers {
		if t.Status == "starting" || t.Status == "running" || t.Status == "unknown" {
			active++
		}
	}
	if active >= g.cfg.MaxTransfers || len(g.transfers) >= 10000 {
		g.mu.Unlock()
		fail(w, 429, "transfer_limit")
		return
	}
	id := uuid.NewString()
	now := time.Now().UTC()
	uid, _ := strconv.ParseInt(c.Subject, 10, 64)
	t := &Transfer{Version: 1, EventID: id, TransferID: id, GatewayID: g.cfg.GatewayID, ClusterID: s.cluster, UserID: c.Subject, NumericUserID: uid, Username: c.Username, DestinationClusterID: destination.ID, SourcePaths: append([]string{}, input.SourcePaths...), DestinationPath: input.DestinationPath, Status: "starting", StartedAt: now, SessionID: s.id, RemoteDirectory: path.Join(s.home, ".go-ldap-admin-transfers", id), CallbackExpires: now.Add(30 * 24 * time.Hour)}
	capability := opaque() + opaque()
	t.CallbackHash = hashCapability(capability)
	if e := g.persist(t); e != nil {
		g.mu.Unlock()
		fail(w, 503, "state_unavailable")
		return
	}
	g.transfers[id] = t
	g.mu.Unlock()
	files := runnerFiles(g.cfg, t, destination, input.credential, capability)
	input.PrivateKey = ""
	input.Certificate = ""
	if e := prepareRunner(s, t, files); e != nil {
		code := "runner_start_failed"
		if errors.Is(e, errRunnerUnsupported) {
			code = "runner_unsupported"
		}
		g.finishStartFailure(t, code)
		fail(w, 502, code)
		return
	}
	g.mu.Lock()
	if t.Status == "starting" {
		t.Status = "running"
		_ = g.persist(t)
	}
	status := t.Status
	g.mu.Unlock()
	reply(w, 201, map[string]string{"transfer_id": id, "status": status})
}

var errRunnerUnsupported = errors.New("runner requires modern rsync and source programs")

func prepareRunner(s *session, t *Transfer, files map[string]string) error {
	parent := path.Dir(t.RemoteDirectory)
	if e := s.files.Mkdir(parent); e != nil {
		if _, err := s.files.Lstat(parent); err != nil {
			return e
		}
	}
	info, e := s.files.Lstat(parent)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runner parent must be a real directory")
	}
	resolved, e := s.files.RealPath(parent)
	if e != nil || resolved != parent {
		return errors.New("runner parent is a symlink")
	}
	if e := s.files.Chmod(parent, 0700); e != nil {
		return e
	}
	if e = s.files.Mkdir(t.RemoteDirectory); e != nil {
		return e
	}
	if e = s.files.Chmod(t.RemoteDirectory, 0700); e != nil {
		return e
	}
	started := false
	defer func() {
		if !started {
			for name := range files {
				_ = s.files.Remove(path.Join(t.RemoteDirectory, name))
			}
			_ = s.files.RemoveDirectory(t.RemoteDirectory)
		}
	}()
	for name, value := range files {
		f, e := s.files.OpenFile(path.Join(t.RemoteDirectory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if e != nil {
			return e
		}
		if e = f.Chmod(0600); e == nil {
			_, e = io.WriteString(f, value)
		}
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	remote, e := s.client.NewSession()
	if e != nil {
		return e
	}
	defer func() { _ = remote.Close() }()
	line := "(test -x /usr/bin/curl && test -x /usr/bin/screen && /usr/bin/rsync --version | awk 'BEGIN{ok=0} NR==1 {split($3,v,\".\");ok=((v[1]>3)||(v[1]==3&&v[2]>2)||(v[1]==3&&v[2]==2&&v[3]>=4))} END{exit !ok}') || exit 61; " + command("/usr/bin/screen", "-dmS", "web-rsync-"+t.TransferID, "/bin/sh", path.Join(t.RemoteDirectory, "run.sh"))
	if e = remote.Run(line); e != nil {
		var exit *ssh.ExitError
		if errors.As(e, &exit) && exit.ExitStatus() == 61 {
			return errRunnerUnsupported
		}
		return e
	}
	started = true
	return nil
}
func runnerFiles(cfg Config, t *Transfer, destination Cluster, key credential, capability string) map[string]string {
	dir := t.RemoteDirectory
	host, port, _ := net.SplitHostPort(destination.SSHAddress)
	sshConfig := "Host destination\n  HostName " + host + "\n  Port " + port + "\n  User " + t.Username + "\n  IdentityFile " + strconv.Quote(path.Join(dir, "identity")) + "\n  CertificateFile " + strconv.Quote(path.Join(dir, "identity-cert.pub")) + "\n  UserKnownHostsFile /dev/null\n  GlobalKnownHostsFile /dev/null\n  StrictHostKeyChecking no\n  CheckHostIP no\n  BatchMode yes\n  IdentitiesOnly yes\n  PasswordAuthentication no\n  KbdInteractiveAuthentication no\n  ForwardAgent no\n  ClearAllForwardings yes\n  ProxyCommand none\n  ProxyJump none\n  ConnectionAttempts 1\n  ConnectTimeout 10\n  ServerAliveInterval 30\n  ServerAliveCountMax 3\n  LogLevel ERROR\n"
	args := []string{"/usr/bin/rsync", "-avz", "--info=progress2", "--stats", "--partial-dir=.rsync-partial", "-e", command("/usr/bin/ssh", "-F", path.Join(dir, "ssh_config")), "--"}
	args = append(args, t.SourcePaths...)
	args = append(args, "destination:"+t.DestinationPath)
	// No shell interpolation of user paths, hosts or credentials. stdout remains
	// a plain-text log; only the fixed numeric rsync trailer enters JSON.
	script := "#!/bin/sh\numask 077\ncd " + quote(dir) + " || exit 125\nexport LC_ALL=C PATH=/usr/bin:/bin RSYNC_OLD_ARGS=0 RSYNC_PROTECT_ARGS=0\ntrap 'rm -f identity identity-cert.pub ssh_config' 0\ntrap 'exit 143' HUP INT TERM\nstarted=$(date +%s)\n" + command(args...) + " > raw.log 2>&1\ncode=$?\nfinished=$(date +%s)\nrm -f identity identity-cert.pub ssh_config\n" + `awk -v code="$code" -v start="$started" -v end="$finished" 'BEGIN {sent="null";received="null";total="null"} /^Total bytes sent: / {v=$0;sub(/^Total bytes sent: /,"",v);gsub(/[, ]/,"",v);if(v ~ /^[0-9]+$/)sent=v} /^Total bytes received: / {v=$0;sub(/^Total bytes received: /,"",v);gsub(/[, ]/,"",v);if(v ~ /^[0-9]+$/)received=v} /^Total transferred file size: / {v=$0;sub(/^Total transferred file size: /,"",v);sub(/ bytes.*$/,"",v);gsub(/[, ]/,"",v);if(v ~ /^[0-9]+$/)total=v} END {printf "{\"exit_code\":%d,\"started_unix\":%d,\"finished_unix\":%d,\"bytes_sent\":%s,\"bytes_received\":%s,\"total_bytes\":%s}\n",code,start,end,sent,received,total}' raw.log > exit.json.tmp
mv exit.json.tmp exit.json
attempt=0
while [ "$attempt" -lt 2880 ]; do
  if /usr/bin/curl --config callback.conf --data-binary @exit.json --fail --silent --output /dev/null; then
    rm -f callback.conf run.sh
    exit 0
  fi
  attempt=$((attempt + 1))
  sleep 30
done
exit 1
`
	callback := "url = " + strconv.Quote("http://"+cfg.CallbackListen+"/v1/completions/"+t.TransferID) + "\nrequest = \"POST\"\nheader = " + strconv.Quote("Authorization: Bearer "+capability) + "\nheader = \"Content-Type: application/json\"\nconnect-timeout = 2\nmax-time = 5\nproto = \"=http\"\nnoproxy = \"*\"\n"
	return map[string]string{"identity": key.PrivateKey, "identity-cert.pub": key.Certificate, "ssh_config": sshConfig, "callback.conf": callback, "run.sh": script}
}
func (g *Gateway) finishStartFailure(t *Transfer, code string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if t.FinishedAt != nil {
		return
	}
	v := 125
	now := time.Now().UTC()
	t.ExitCode = &v
	t.FinishedAt = &now
	t.Status = "failed"
	t.Error = &code
	elapsed := now.Sub(t.StartedAt).Seconds()
	t.ElapsedSeconds = &elapsed
	_ = g.persist(t)
}
func (g *Gateway) logs(w http.ResponseWriter, r *http.Request, id string) {
	conn, c, _, e := g.accept(w, r, "transfer:read")
	if e != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	g.mu.Lock()
	t := g.transfers[id]
	if t == nil || t.UserID != c.Subject || t.Username != c.Username {
		g.mu.Unlock()
		_ = conn.Close(websocket.StatusPolicyViolation, "transfer unavailable")
		return
	}
	sid, filename := t.SessionID, path.Join(t.RemoteDirectory, "raw.log")
	g.mu.Unlock()
	s, done, e := g.getSession(sid, c)
	if e != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "source session unavailable")
		return
	}
	defer done()
	lease := newSocketLease(r.Context(), g.cfg, c, "transfer:read", conn)
	defer lease.stop()
	ctx, cancel := context.WithCancel(lease.ctx)
	defer cancel()
	logFiles, closeLog, e := fileChannel(ctx, s, 0)
	if e != nil {
		_ = conn.Close(websocket.StatusInternalError, "log channel unavailable")
		return
	}
	defer closeLog()
	go func() {
		for {
			kind, raw, e := conn.Read(ctx)
			if e != nil {
				cancel()
				return
			}
			var auth struct {
				Type   string `json:"type"`
				Ticket string `json:"ticket"`
			}
			if kind != websocket.MessageText || json.Unmarshal(raw, &auth) != nil || auth.Type != "auth" || !lease.renew(auth.Ticket) {
				_ = conn.Close(websocket.StatusPolicyViolation, "renewal denied")
				cancel()
				return
			}
		}
	}()
	var offset int64
	buffer := make([]byte, 32768)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !lease.valid() {
				_ = conn.Close(websocket.StatusPolicyViolation, "ticket expired")
				return
			}
			f, e := logFiles.Open(filename)
			if e == nil {
				for {
					n, err := f.ReadAt(buffer, offset)
					if n > 0 {
						writeCtx, stop := context.WithTimeout(ctx, 10*time.Second)
						e = conn.Write(writeCtx, websocket.MessageBinary, buffer[:n])
						stop()
						if e != nil {
							_ = f.Close()
							return
						}
						offset += int64(n)
					}
					if err != nil {
						break
					}
				}
				_ = f.Close()
			}
			g.mu.Lock()
			finished := t.FinishedAt != nil
			g.mu.Unlock()
			if finished {
				_ = conn.Close(websocket.StatusNormalClosure, "transfer finished")
				return
			}
		}
	}
}
