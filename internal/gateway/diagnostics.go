package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

type probeResult struct {
	ProbeID              string `json:"probe_id"`
	DestinationClusterID string `json:"destination_cluster_id,omitempty"`
	Status               string `json:"status"`
	LatencyMS            int64  `json:"latency_ms"`
	Detail               string `json:"detail"`
	Coverage             string `json:"coverage"`
}

func (g *Gateway) sshProbe(ctx context.Context, cluster Cluster, id string) probeResult {
	started := time.Now()
	result := probeResult{ProbeID: id, Status: "error", Detail: "ssh_transport_failed", Coverage: "SSH transport/key exchange; host identity is not pinned; user auth not tested"}
	if id == "transfer-ssh" {
		result.DestinationClusterID = cluster.ID
	}
	raw, e := (&net.Dialer{}).DialContext(ctx, "tcp", cluster.SSHAddress)
	if e == nil {
		deadline, _ := ctx.Deadline()
		_ = raw.SetDeadline(deadline)
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		exchanged := false
		config := &ssh.ClientConfig{User: "diagnostic-no-auth", HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			_ = ssh.InsecureIgnoreHostKey()(cluster.SSHAddress, raw.RemoteAddr(), key)
			exchanged = true
			return errors.New("diagnostic key exchange completed; stop before authentication")
		}, Timeout: 5 * time.Second}
		conn, _, _, err := ssh.NewClientConn(raw, cluster.SSHAddress, config)
		if conn != nil {
			_ = conn.Close()
		}
		stop()
		_ = raw.Close()
		e = err
		if exchanged {
			result.Status, result.Detail = "ok", "ssh_key_exchange_completed"
		}
	}
	if result.Status != "ok" {
		var timeout net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(e, context.DeadlineExceeded) || (errors.As(e, &timeout) && timeout.Timeout()) {
			result.Status, result.Detail = "timeout", "ssh_transport_timeout"
		}
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	return result
}

type probeBuffer struct{ bytes.Buffer }

func (b *probeBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8192 {
		return 0, errors.New("version response too large")
	}
	return b.Buffer.Write(p)
}
func transferRuntimeProbe(ctx context.Context) probeResult {
	started := time.Now()
	result := probeResult{ProbeID: "transfer-runtime", Status: "error", Detail: "transfer_program_unavailable", Coverage: "Local screen, SSH, curl, awk and rsync>=3.2.4 prerequisites; no shell session or transfer started"}
	for _, program := range []string{"/usr/bin/screen", "/usr/bin/ssh", "/usr/bin/curl", "/usr/bin/awk", "/usr/bin/rsync"} {
		info, e := os.Stat(program)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			result.LatencyMS = time.Since(started).Milliseconds()
			return result
		}
	}
	var output probeBuffer
	cmd := exec.CommandContext(ctx, "/usr/bin/rsync", "--version")
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	e := cmd.Run()
	result.Detail = "rsync_version_unsupported"
	if e == nil {
		line := strings.SplitN(output.String(), "\n", 2)[0]
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			version := strings.Split(fields[2], ".")
			if len(version) >= 3 {
				major, _ := strconv.Atoi(version[0])
				minor, _ := strconv.Atoi(version[1])
				patch, _ := strconv.Atoi(version[2])
				if major > 3 || major == 3 && (minor > 2 || minor == 2 && patch >= 4) {
					result.Status, result.Detail = "ok", "transfer_programs_available"
				}
			}
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Status, result.Detail = "timeout", "runtime_probe_timeout"
	}
	result.LatencyMS = time.Since(started).Milliseconds()
	return result
}
func (g *Gateway) diagnostics(w http.ResponseWriter, r *http.Request) {
	if _, e := g.auth(r, "diagnostics"); e != nil {
		fail(w, 401, "ticket_invalid")
		return
	}
	var input struct{}
	if body(r, &input) != nil {
		fail(w, 400, "request_invalid")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	source, _ := g.cfg.cluster(g.cfg.ClusterID)
	jobs := []func() probeResult{func() probeResult { return g.sshProbe(ctx, source, "ssh-handshake") }, func() probeResult { return transferRuntimeProbe(ctx) }}
	results := []probeResult{{ProbeID: "ssh-handshake"}, {ProbeID: "transfer-runtime"}}
	for _, cluster := range g.cfg.Clusters {
		if cluster.ID != g.cfg.ClusterID {
			destination := cluster
			jobs = append(jobs, func() probeResult { return g.sshProbe(ctx, destination, "transfer-ssh") })
			results = append(results, probeResult{ProbeID: "transfer-ssh", DestinationClusterID: destination.ID})
		}
	}
	if code := g.configurationError(); code != "" {
		for i := range jobs {
			results[i].Status = "error"
			results[i].Detail = code
			results[i].Coverage = "Authoritative gateway configuration has not been verified"
		}
	} else {
		queue := make(chan int, len(jobs))
		for i := range jobs {
			queue <- i
		}
		close(queue)
		var wg sync.WaitGroup
		for worker := 0; worker < 4; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range queue {
					results[i] = jobs[i]()
				}
			}()
		}
		wg.Wait()
	}
	reply(w, 200, map[string]any{"gateway_id": g.cfg.GatewayID, "cluster_id": g.cfg.ClusterID, "results": results})
}
func (g *Gateway) diagnosticSocket(w http.ResponseWriter, r *http.Request) {
	conn, _, _, e := g.accept(w, r, "diagnostics")
	if e != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, []byte(`{"probe_id":"gateway-websocket","status":"ok"}`))
	_ = conn.Close(websocket.StatusNormalClosure, "probe complete")
}
