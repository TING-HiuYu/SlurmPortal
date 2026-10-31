package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type storedTransfer struct {
	Transfer        Transfer  `json:"transfer"`
	Owner           string    `json:"owner"`
	SessionID       string    `json:"session_id"`
	RemoteDirectory string    `json:"remote_directory"`
	CallbackHash    string    `json:"callback_hash"`
	CallbackExpires time.Time `json:"callback_expires"`
	Delivered       bool      `json:"delivered"`
}

func (g *Gateway) persist(t *Transfer) error {
	value := storedTransfer{*t, t.UserID, t.SessionID, t.RemoteDirectory, t.CallbackHash, t.CallbackExpires, t.Delivered}
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	tmp, e := os.CreateTemp(g.cfg.StateDirectory, ".transfer-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(raw)
	}
	if e == nil {
		e = tmp.Sync()
	}
	closeErr := tmp.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(name, filepath.Join(g.cfg.StateDirectory, t.TransferID+".json")); e != nil {
		return e
	}
	dir, e := os.Open(g.cfg.StateDirectory)
	if e != nil {
		return e
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}
func (g *Gateway) loadTransfers() error {
	entries, e := os.ReadDir(g.cfg.StateDirectory)
	if e != nil {
		return e
	}
	for _, v := range entries {
		if !strings.HasSuffix(v.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(v.Name(), ".json")
		if _, e = uuid.Parse(id); e != nil {
			return errors.New("invalid persisted task")
		}
		st, e := v.Info()
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() > 131072 {
			return errors.New("unsafe task record")
		}
		raw, e := os.ReadFile(filepath.Join(g.cfg.StateDirectory, v.Name()))
		if e != nil {
			return e
		}
		var stored storedTransfer
		if json.Unmarshal(raw, &stored) != nil || stored.Transfer.TransferID != id || stored.Transfer.GatewayID != g.cfg.GatewayID || stored.Transfer.ClusterID != g.cfg.ClusterID || stored.Owner != strconv.FormatInt(stored.Transfer.NumericUserID, 10) || len(stored.CallbackHash) != 64 {
			return errors.New("corrupt task record")
		}
		t := stored.Transfer
		t.UserID = stored.Owner
		t.SessionID = stored.SessionID
		t.RemoteDirectory = stored.RemoteDirectory
		t.CallbackHash = stored.CallbackHash
		t.CallbackExpires = stored.CallbackExpires
		t.Delivered = stored.Delivered
		if t.Status == "starting" || t.Status == "running" {
			t.Status = "unknown"
		}
		g.transfers[id] = &t
		if len(g.transfers) > 10000 {
			return errors.New("task record limit")
		}
	}
	return nil
}

type completionInput struct {
	ExitCode      int    `json:"exit_code"`
	StartedUnix   int64  `json:"started_unix"`
	FinishedUnix  int64  `json:"finished_unix"`
	BytesSent     *int64 `json:"bytes_sent"`
	BytesReceived *int64 `json:"bytes_received"`
	TotalBytes    *int64 `json:"total_bytes"`
}

func (input *completionInput) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return errors.New("invalid result")
	}
	for _, name := range []string{"exit_code", "started_unix", "finished_unix", "bytes_sent", "bytes_received", "total_bytes"} {
		value, ok := fields[name]
		if !ok || ((name == "exit_code" || name == "started_unix" || name == "finished_unix") && bytes.Equal(bytes.TrimSpace(value), []byte("null"))) {
			return errors.New("incomplete result")
		}
	}
	type wire completionInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*wire)(input))
}

// CallbackHandler receives completion reports of transfers started by this gateway (from loopback
// only) and forwards them to master.
func (g *Gateway) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		host, _, e := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if e != nil || ip == nil || !ip.IsLoopback() || r.Method != "POST" || r.URL.RawQuery != "" || !strings.HasPrefix(r.URL.Path, "/v1/completions/") {
			fail(w, 403, "callback_denied")
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/completions/")
		g.mu.Lock()
		defer g.mu.Unlock()
		t := g.transfers[id]
		h := r.Header.Get("Authorization")
		if t == nil || !strings.HasPrefix(h, "Bearer ") || len(h) > 256 || subtle.ConstantTimeCompare([]byte(hashCapability(strings.TrimPrefix(h, "Bearer "))), []byte(t.CallbackHash)) != 1 || time.Now().After(t.CallbackExpires) {
			fail(w, 403, "callback_denied")
			return
		}
		var input completionInput
		if body(r, &input) != nil || input.ExitCode < 0 || input.ExitCode > 255 || input.StartedUnix < t.StartedAt.Add(-time.Minute).Unix() || input.FinishedUnix < input.StartedUnix || input.FinishedUnix > time.Now().Add(30*time.Second).Unix() || input.FinishedUnix-input.StartedUnix > 30*86400 {
			fail(w, 400, "result_invalid")
			return
		}
		for _, v := range []*int64{input.BytesSent, input.BytesReceived, input.TotalBytes} {
			if v != nil && (*v < 0 || *v > 1<<53-1) {
				fail(w, 400, "result_invalid")
				return
			}
		}
		if t.FinishedAt != nil {
			if t.ExitCode != nil && *t.ExitCode == input.ExitCode && t.FinishedAt.Unix() == input.FinishedUnix {
				reply(w, 200, map[string]bool{"accepted": true})
				return
			}
			fail(w, 409, "result_conflict")
			return
		}
		next := *t
		start, end := time.Unix(input.StartedUnix, 0).UTC(), time.Unix(input.FinishedUnix, 0).UTC()
		next.StartedAt = start
		next.FinishedAt = &end
		next.ExitCode = &input.ExitCode
		next.BytesSent = input.BytesSent
		next.BytesReceived = input.BytesReceived
		next.TotalBytes = input.TotalBytes
		elapsed := end.Sub(start).Seconds()
		next.ElapsedSeconds = &elapsed
		if elapsed > 0 && input.BytesSent != nil && input.BytesReceived != nil {
			rate := (float64(*input.BytesSent) + float64(*input.BytesReceived)) / elapsed / 1e6
			next.AverageMBPerSecond = &rate
		}
		switch input.ExitCode {
		case 0:
			next.Status = "completed"
		case 23, 24:
			next.Status = "partial"
		default:
			next.Status = "failed"
		}
		if e = g.persist(&next); e != nil {
			fail(w, 503, "state_unavailable")
			return
		}
		*t = next
		reply(w, 200, map[string]bool{"accepted": true})
	})
}
func masterHTTPClient(cfg Config) (*http.Client, error) {
	transport := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 45 * time.Second, TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 30 * time.Second}
	if cfg.MasterOrigin != "" {
		cert, e := tls.LoadX509KeyPair(cfg.MasterClientCertificate, cfg.MasterClientKey)
		if e != nil {
			return nil, e
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{Transport: transport, Timeout: 50 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func (g *Gateway) flushOutbox() {
	if g.cfg.MasterOrigin == "" {
		return
	}
	g.mu.Lock()
	var tasks []Transfer
	for _, t := range g.transfers {
		if t.FinishedAt != nil && !t.Delivered {
			tasks = append(tasks, *t)
			if len(tasks) == 8 {
				break
			}
		}
	}
	g.mu.Unlock()
	for _, t := range tasks {
		if g.ctx.Err() != nil {
			return
		}
		raw, e := json.Marshal(t)
		if e != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(g.ctx, 45*time.Second)
		req, e := http.NewRequestWithContext(ctx, "POST", g.cfg.MasterOrigin+"/sync/v2/web-access/events", bytes.NewReader(raw))
		if e != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		response, e := g.masterClient.Do(req)
		if e == nil {
			var result struct {
				Code int `json:"code"`
				Data struct {
					Outcome string `json:"outcome"`
				} `json:"data"`
			}
			if response.StatusCode == 200 && json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result) == nil && result.Code == 0 && (result.Data.Outcome == "smtp_accepted" || result.Data.Outcome == "skipped") {
				g.mu.Lock()
				if current := g.transfers[t.TransferID]; current != nil {
					next := *current
					next.Delivered = true
					if g.persist(&next) == nil {
						*current = next
					}
				}
				g.mu.Unlock()
			}
			_ = response.Body.Close()
		}
		cancel()
	}
}
