package authz

import (
	"context"
	"testing"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

func TestRefreshingReloadsAfterTTL(t *testing.T) {
	c, _ := NewCasbin(nil)
	rules := []Rule{}
	loads := 0
	now := time.Unix(0, 0)
	r := &Refreshing{Casbin: c, TTL: 10 * time.Second, Now: func() time.Time { return now },
		Load: func(context.Context) ([]Rule, error) { loads++; return rules, nil }}
	p := &authn.Principal{Roles: []string{"ops"}}
	ctx := context.Background()
	if ok, _ := r.Allow(ctx, p, "user", "read"); ok || loads != 1 {
		t.Fatal("initial load wrong")
	}
	rules = []Rule{{Role: "ops", Object: "user", Action: "read"}}
	now = now.Add(5 * time.Second)
	if ok, _ := r.Allow(ctx, p, "user", "read"); ok || loads != 1 {
		t.Fatal("reloaded before TTL")
	}
	now = now.Add(6 * time.Second)
	if ok, _ := r.Allow(ctx, p, "user", "read"); !ok || loads != 2 {
		t.Fatal("did not pick up remote grant after TTL")
	}
}
