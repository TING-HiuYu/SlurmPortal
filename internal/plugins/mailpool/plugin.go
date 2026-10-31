// Package mailpool adds pool mail providers: placeholders that plugins send to like any provider,
// while each message is handed to one member provider chosen by a rule. A pool spreads mail over
// several free-tier accounts, e.g. the first 300 a day through smtp1, the next 500 through smtp2 and
// the rest through smtp3 (priority), or evenly or by weight across all of them.
package mailpool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

const (
	ID   = "mail-pool"
	Kind = "mail-pool"

	StrategyPriority   = "priority"
	StrategyRoundRobin = "round-robin"
	StrategyWeighted   = "weighted"

	maxMembers = 16
	maxQuota   = 10_000_000
	maxWeight  = 1000
)

var (
	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// ErrExhausted means every member has used its quota for the current period (or none is usable).
	ErrExhausted = &mail.Error{Kind: "mail_pool_quota_exhausted"}
)

// Plugin is the mail-pool plugin.
type Plugin struct{}

func (Plugin) ID() string                  { return ID }
func (Plugin) Roles() []plugin.Role        { return []plugin.Role{plugin.RoleStandalone, plugin.RoleMaster} }
func (Plugin) Close(context.Context) error { return nil }

func (Plugin) Init(_ context.Context, h *plugin.Host) error {
	if h.Mail == nil {
		return errors.New("mail-pool: no mail hub")
	}
	h.Mail.RegisterKind(poolKind{})
	s := &service{hub: h.Mail, db: h.Store}
	h.Router.Handle(httpx.Route{Method: http.MethodGet, Path: "/api/v2/mail-pool/usage", Permission: authz.Policy("mail", "read"), Handler: s.usage})
	return nil
}

// Member is one provider in a pool. Quota caps messages per period (0 = unlimited); Weight is the
// relative share under the weighted strategy.
type Member struct {
	Provider string `json:"provider"`
	Quota    int    `json:"quota"`
	Weight   int    `json:"weight"`
}

// Config is a pool provider's stored configuration.
type Config struct {
	Strategy string `json:"strategy"`
	// Period is the quota window: "day" or "month" (UTC), or "total" for a lifetime budget.
	Period string `json:"period"`
	// Failover retries the next eligible member when one fails with an error another provider may not hit.
	Failover bool     `json:"failover"`
	Members  []Member `json:"members"`
}

func periodKey(period string, now time.Time) string {
	now = now.UTC()
	switch period {
	case "day":
		return now.Format("2006-01-02")
	case "month":
		return now.Format("2006-01")
	}
	return "total"
}

func invalid(msg string) error { return apperr.BadRequest(msg) }

type poolKind struct{}

func (poolKind) ID() string            { return Kind }
func (poolKind) Label() string         { return "Mail pool" }
func (poolKind) SecretPurpose() string { return "" }

func (poolKind) Normalize(raw json.RawMessage, enabled, _ bool) (json.RawMessage, error) {
	c := Config{Strategy: StrategyPriority, Period: "day", Failover: true}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, invalid("invalid pool configuration")
	}
	switch c.Strategy {
	case StrategyPriority, StrategyRoundRobin, StrategyWeighted:
	default:
		return nil, invalid("strategy must be priority, round-robin or weighted")
	}
	switch c.Period {
	case "day", "month", "total":
	default:
		return nil, invalid("period must be day, month or total")
	}
	if len(c.Members) > maxMembers || (enabled && len(c.Members) == 0) {
		return nil, invalid("a pool needs 1-16 member providers")
	}
	seen := map[string]bool{}
	for i := range c.Members {
		m := &c.Members[i]
		if !namePattern.MatchString(m.Provider) || seen[m.Provider] {
			return nil, invalid("member providers must be distinct provider names")
		}
		seen[m.Provider] = true
		if m.Quota < 0 || m.Quota > maxQuota {
			return nil, invalid("quota must be 0 (unlimited) to 10000000")
		}
		if m.Weight == 0 {
			m.Weight = 1
		}
		if m.Weight < 1 || m.Weight > maxWeight {
			return nil, invalid("weight must be 1-1000")
		}
	}
	if c.Members == nil {
		c.Members = []Member{}
	}
	return json.Marshal(c)
}

func (poolKind) References(raw json.RawMessage) []string {
	var c Config
	_ = json.Unmarshal(raw, &c)
	out := make([]string, 0, len(c.Members))
	for _, m := range c.Members {
		out = append(out, m.Provider)
	}
	return out
}

func (poolKind) Open(_ context.Context, h *mail.Hub, s mail.Stored, _ string) (mail.Provider, error) {
	var c Config
	if err := json.Unmarshal(s.Config, &c); err != nil {
		return nil, err
	}
	return &pool{name: s.Name, cfg: c, hub: h, db: h.Store()}, nil
}

type pool struct {
	name string
	cfg  Config
	hub  *mail.Hub
	db   store.Store
}

func (p *pool) usage(ctx context.Context, period string) (map[string][2]int, error) {
	rows, err := p.db.Query(ctx, "SELECT member, sent, failed FROM v2_mail_pool_usage WHERE pool = ? AND period = ?", p.name, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int{}
	for rows.Next() {
		var m string
		var sent, failed int
		if err := rows.Scan(&m, &sent, &failed); err != nil {
			return nil, err
		}
		out[m] = [2]int{sent, failed}
	}
	return out, rows.Err()
}

// order returns the members to try, best first, skipping those whose quota is used up.
func (p *pool) order(used map[string][2]int) []Member {
	var open []Member
	total := 0
	for _, m := range p.cfg.Members {
		total += used[m.Provider][0]
		if m.Quota == 0 || used[m.Provider][0] < m.Quota {
			open = append(open, m)
		}
	}
	switch p.cfg.Strategy {
	case StrategyRoundRobin:
		if n := len(open); n > 0 {
			k := total % n
			open = append(open[k:], open[:k]...)
		}
	case StrategyWeighted:
		// Smooth weighted: the member furthest below its share goes next; ties keep configured order.
		sort.SliceStable(open, func(i, j int) bool {
			a, b := open[i], open[j]
			return used[a.Provider][0]*b.Weight < used[b.Provider][0]*a.Weight
		})
	}
	return open
}

// reserve counts one message against a member before sending, atomically respecting the quota.
func (p *pool) reserve(ctx context.Context, m Member, period string) (bool, error) {
	res, err := p.db.Exec(ctx, `INSERT INTO v2_mail_pool_usage (pool, member, period, sent, failed, updated_at) VALUES (?, ?, ?, 1, 0, ?)
		ON CONFLICT(pool, member, period) DO UPDATE SET sent = v2_mail_pool_usage.sent + 1, updated_at = excluded.updated_at
		WHERE ? = 0 OR v2_mail_pool_usage.sent < ?`, p.name, m.Provider, period, p.hub.Now().UTC().Format(time.RFC3339Nano), m.Quota, m.Quota)
	return err == nil && res.RowsAffected > 0, err
}

func (p *pool) release(ctx context.Context, m Member, period string, failed bool) error {
	f := 0
	if failed {
		f = 1
	}
	_, err := p.db.Exec(ctx, "UPDATE v2_mail_pool_usage SET sent = sent - 1, failed = failed + ?, updated_at = ? WHERE pool = ? AND member = ? AND period = ?",
		f, p.hub.Now().UTC().Format(time.RFC3339Nano), p.name, m.Provider, period)
	return err
}

func (p *pool) Send(ctx context.Context, msg mail.Message) error {
	period := periodKey(p.cfg.Period, p.hub.Now())
	used, err := p.usage(ctx, period)
	if err != nil {
		return err
	}
	var last error
	bg := context.WithoutCancel(ctx)
	for _, m := range p.order(used) {
		ok, err := p.reserve(ctx, m, period)
		if err != nil {
			return err
		}
		if !ok {
			continue // another sender took the last of this member's quota
		}
		err = p.hub.Send(ctx, m.Provider, msg)
		if err == nil {
			return nil
		}
		unusable := mail.Unavailable(err)
		if rerr := p.release(bg, m, period, !unusable); rerr != nil {
			return errors.Join(err, rerr)
		}
		switch {
		case unusable:
			continue // disabled or deleted member: not an attempt, always skip
		case mail.RecipientRejected(err) || !p.cfg.Failover:
			return err
		}
		last = err
	}
	if last != nil {
		return last
	}
	return ErrExhausted
}

type service struct {
	hub *mail.Hub
	db  store.Store
}

// MemberUsage is one member's counters in the current period.
type MemberUsage struct {
	Member
	Kind      string `json:"kind"`
	Enabled   bool   `json:"enabled"`
	Missing   bool   `json:"missing"`
	Sent      int    `json:"sent"`
	Failed    int    `json:"failed"`
	Remaining *int   `json:"remaining"`
}

// PoolUsage is the current-period view of one pool.
type PoolUsage struct {
	Pool      string        `json:"pool"`
	Enabled   bool          `json:"enabled"`
	Strategy  string        `json:"strategy"`
	Period    string        `json:"period"`
	PeriodKey string        `json:"periodKey"`
	Failover  bool          `json:"failover"`
	Members   []MemberUsage `json:"members"`
}

func (s *service) usage(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	all, err := s.hub.List(ctx)
	if err != nil {
		return err
	}
	byName := map[string]mail.Stored{}
	for _, p := range all {
		byName[p.Name] = p
	}
	out := []PoolUsage{}
	for _, st := range all {
		if st.Kind != Kind {
			continue
		}
		var c Config
		if err := json.Unmarshal(st.Config, &c); err != nil {
			return err
		}
		p := &pool{name: st.Name, cfg: c, hub: s.hub, db: s.db}
		key := periodKey(c.Period, s.hub.Now())
		used, err := p.usage(ctx, key)
		if err != nil {
			return err
		}
		u := PoolUsage{Pool: st.Name, Enabled: st.Enabled, Strategy: c.Strategy, Period: c.Period, PeriodKey: key, Failover: c.Failover, Members: []MemberUsage{}}
		for _, m := range c.Members {
			mu := MemberUsage{Member: m, Sent: used[m.Provider][0], Failed: used[m.Provider][1]}
			if target, ok := byName[m.Provider]; ok {
				mu.Kind, mu.Enabled = target.Kind, target.Enabled
			} else {
				mu.Missing = true
			}
			if m.Quota > 0 {
				left := max(m.Quota-mu.Sent, 0)
				mu.Remaining = &left
			}
			u.Members = append(u.Members, mu)
		}
		out = append(out, u)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
	return nil
}
