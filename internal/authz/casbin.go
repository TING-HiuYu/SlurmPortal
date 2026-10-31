package authz

import (
	"context"
	"sync"

	"github.com/casbin/casbin/v2"
	"github.com/casbin/casbin/v2/model"

	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
)

const rbacModel = `
[request_definition]
r = sub, obj, act

[policy_definition]
p = sub, obj, act

[policy_effect]
e = some(where (p.eft == allow))

[matchers]
m = r.sub == p.sub && (r.obj == p.obj || p.obj == "*") && (r.act == p.act || p.act == "*")
`

// Rule grants a role keyword an action on a logical object, not on a URL path.
type Rule struct {
	Role   string
	Object string
	Action string
}

// Casbin evaluates role permissions with a casbin RBAC model held in memory.
type Casbin struct {
	mu sync.RWMutex
	e  *casbin.SyncedEnforcer
}

// NewCasbin builds an enforcer loaded with rules.
func NewCasbin(rules []Rule) (*Casbin, error) {
	m, err := model.NewModelFromString(rbacModel)
	if err != nil {
		return nil, err
	}
	e, err := casbin.NewSyncedEnforcer(m)
	if err != nil {
		return nil, err
	}
	c := &Casbin{e: e}
	return c, c.Replace(rules)
}

// Replace swaps the whole policy set, used after snapshot sync or role edits.
func (c *Casbin) Replace(rules []Rule) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.e.ClearPolicy()
	if len(rules) == 0 {
		return nil
	}
	policies := make([][]string, 0, len(rules))
	for _, r := range rules {
		policies = append(policies, []string{r.Role, r.Object, r.Action})
	}
	_, err := c.e.AddPolicies(policies)
	return err
}

// Allow reports whether any of the principal's roles grants (object, action).
func (c *Casbin) Allow(_ context.Context, p *authn.Principal, object, action string) (bool, error) {
	if p == nil {
		return false, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, role := range p.Roles {
		ok, err := c.e.Enforce(role, object, action)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}
