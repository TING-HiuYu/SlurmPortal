package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// A browser's authorization lease is separate from the already authenticated
// SSH transport. Refreshing it never reuses or renews an SSH private key.
type socketLease struct {
	cfg     Config
	owner   Claims
	scope   string
	conn    *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	expires time.Time
	wake    chan struct{}
	done    chan struct{}
}

func newSocketLease(parent context.Context, cfg Config, owner Claims, scope string, conn *websocket.Conn) *socketLease {
	ctx, cancel := context.WithCancel(parent)
	l := &socketLease{cfg: cfg, owner: owner, scope: scope, conn: conn, ctx: ctx, cancel: cancel, expires: owner.ExpiresAt.Time, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go l.run()
	return l
}
func (l *socketLease) valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ctx.Err() == nil && time.Now().Before(l.expires)
}
func (l *socketLease) renew(raw string) bool {
	c, e := l.cfg.VerifyTicket(raw, l.scope, time.Now())
	if e != nil || c.Subject != l.owner.Subject || c.Username != l.owner.Username || c.ClusterID != l.owner.ClusterID {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx.Err() != nil || !time.Now().Before(l.expires) {
		return false
	}
	l.expires = c.ExpiresAt.Time
	select {
	case l.wake <- struct{}{}:
	default:
	}
	return true
}
func (l *socketLease) run() {
	defer close(l.done)
	for {
		l.mu.Lock()
		duration := time.Until(l.expires)
		l.mu.Unlock()
		if duration <= 0 {
			if !l.valid() {
				_ = l.conn.Close(websocket.StatusPolicyViolation, "ticket expired")
				l.cancel()
				return
			}
			continue
		}
		timer := time.NewTimer(duration)
		select {
		case <-l.ctx.Done():
			timer.Stop()
			return
		case <-l.wake:
			timer.Stop()
		case <-timer.C:
			if !l.valid() {
				_ = l.conn.Close(websocket.StatusPolicyViolation, "ticket expired")
				l.cancel()
				return
			}
		}
	}
}
func (l *socketLease) stop() { l.cancel(); <-l.done }
