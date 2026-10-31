package authz

import (
	"context"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

// Refreshing reloads policy at most every TTL so other instances' grant changes take effect promptly.
type Refreshing struct {
	*Casbin
	Load func(ctx context.Context) ([]Rule, error)
	TTL  time.Duration
	Now  func() time.Time

	mu     sync.Mutex
	loaded time.Time
}

// Allow reloads the rules when they are older than TTL, then evaluates them.
func (r *Refreshing) Allow(ctx context.Context, p *authn.Principal, object, action string) (bool, error) {
	if err := r.refresh(ctx); err != nil {
		return false, err
	}
	return r.Casbin.Allow(ctx, p, object, action)
}

// refresh never holds the lock across the load: on Workers (Go/Wasm) a request blocked on a lock held
// by another request resumes inside that request's I/O context and is cancelled as hung once the
// other request finishes. Concurrent refreshes may each load; the last one wins, which is harmless.
func (r *Refreshing) refresh(ctx context.Context) error {
	r.mu.Lock()
	now := r.Now()
	fresh := !r.loaded.IsZero() && now.Sub(r.loaded) < r.TTL
	r.mu.Unlock()
	if fresh {
		return nil
	}
	rules, err := r.Load(ctx)
	if err != nil {
		return err
	}
	if err := r.Casbin.Replace(rules); err != nil {
		return err
	}
	r.mu.Lock()
	r.loaded = now
	r.mu.Unlock()
	return nil
}

// Touch marks the current policy fresh after an in-process reload.
func (r *Refreshing) Touch() {
	r.mu.Lock()
	r.loaded = r.Now()
	r.mu.Unlock()
}
