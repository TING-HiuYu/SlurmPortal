package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

type wsAuth struct {
	Ticket string `json:"ticket"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
}

func (g *Gateway) accept(w http.ResponseWriter, r *http.Request, scope string) (*websocket.Conn, Claims, wsAuth, error) {
	preliminary, e := g.auth(r, "")
	if e != nil {
		fail(w, 403, "ticket_invalid")
		return nil, Claims{}, wsAuth{}, e
	}
	// ServeHTTP already checked the exact Origin allowlist. Coder's default
	// same-host policy would reject the separately hosted, allowed Pages origin.
	conn, e := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true, CompressionMode: websocket.CompressionDisabled})
	if e != nil {
		return nil, Claims{}, wsAuth{}, e
	}
	conn.SetReadLimit(16384)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	typ, raw, e := conn.Read(ctx)
	var input wsAuth
	if e == nil && typ == websocket.MessageText {
		e = json.Unmarshal(raw, &input)
	} else if e == nil {
		e = io.ErrUnexpectedEOF
	}
	var c Claims
	if e == nil {
		c, e = g.cfg.VerifyTicket(input.Ticket, scope, time.Now())
	}
	if e == nil && (c.Subject != preliminary.Subject || c.Username != preliminary.Username || c.ClusterID != preliminary.ClusterID) {
		e = io.ErrUnexpectedEOF
	}
	if e != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "authentication required")
		return nil, c, input, e
	}
	return conn, c, input, nil
}
func dimensions(cols, rows int) (int, int) {
	if cols < 1 || cols > 500 {
		cols = 100
	}
	if rows < 1 || rows > 300 {
		rows = 30
	}
	return cols, rows
}

type terminalWriter struct {
	ctx   context.Context
	conn  *websocket.Conn
	mu    *sync.Mutex
	lease *socketLease
}

func (w terminalWriter) Write(p []byte) (int, error) {
	if !w.lease.valid() {
		return 0, io.ErrClosedPipe
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	defer cancel()
	if e := w.conn.Write(ctx, websocket.MessageBinary, p); e != nil {
		return 0, e
	}
	return len(p), nil
}
func (g *Gateway) terminal(w http.ResponseWriter, r *http.Request, id string) {
	conn, c, input, e := g.accept(w, r, "terminal")
	if e != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	s, done, e := g.getSession(id, c)
	if e != nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "session unavailable")
		return
	}
	defer done()
	if s.purpose != "access" {
		_ = conn.Close(websocket.StatusPolicyViolation, "terminal credential required")
		return
	}
	lease := newSocketLease(r.Context(), g.cfg, c, "terminal", conn)
	defer lease.stop()
	setupTimeout := time.AfterFunc(sshTimeout, func() { _ = s.client.Close() })
	defer setupTimeout.Stop()
	remote, e := s.client.NewSession()
	if e != nil {
		_ = conn.Close(websocket.StatusInternalError, "SSH channel unavailable")
		return
	}
	defer func() { _ = remote.Close() }()
	cols, rows := dimensions(input.Cols, input.Rows)
	if e = remote.RequestPty("xterm-256color", rows, cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}); e != nil {
		_ = conn.Close(websocket.StatusInternalError, "PTY unavailable")
		return
	}
	stdin, e := remote.StdinPipe()
	if e != nil {
		return
	}
	ctx, cancel := context.WithCancel(lease.ctx)
	defer cancel()
	var mu sync.Mutex
	remote.Stdout = terminalWriter{ctx, conn, &mu, lease}
	remote.Stderr = remote.Stdout
	if remote.Shell() != nil {
		_ = conn.Close(websocket.StatusInternalError, "shell unavailable")
		return
	}
	setupTimeout.Stop()
	finished := make(chan error, 1)
	go func() { finished <- remote.Wait(); cancel() }()
	for {
		kind, data, e := conn.Read(ctx)
		if e != nil {
			break
		}
		if !lease.valid() {
			_ = conn.Close(websocket.StatusPolicyViolation, "ticket expired")
			break
		}
		if kind == websocket.MessageBinary {
			if _, e = stdin.Write(data); e != nil {
				break
			}
		} else {
			var update struct {
				Type   string `json:"type"`
				Ticket string `json:"ticket"`
				Cols   int    `json:"cols"`
				Rows   int    `json:"rows"`
			}
			if json.Unmarshal(data, &update) == nil && update.Type == "auth" {
				if !lease.renew(update.Ticket) {
					_ = conn.Close(websocket.StatusPolicyViolation, "renewal denied")
					break
				}
				continue
			}
			if json.Unmarshal(data, &update) != nil || update.Type != "resize" {
				_ = conn.Close(websocket.StatusUnsupportedData, "resize or binary input required")
				break
			}
			cols, rows = dimensions(update.Cols, update.Rows)
			if remote.WindowChange(rows, cols) != nil {
				break
			}
		}
	}
	_ = stdin.Close()
	_ = remote.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
	}
	_ = conn.Close(websocket.StatusNormalClosure, "terminal closed")
}
