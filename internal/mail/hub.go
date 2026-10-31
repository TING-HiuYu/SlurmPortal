package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/apperr"
	"github.com/TING-HiuYu/SlurmPortal/internal/secretbox"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

var (
	// ErrNoProvider means the requested (or default) provider does not exist.
	ErrNoProvider = errors.New("mail provider not configured")
	// ErrDisabled means the provider exists but is switched off.
	ErrDisabled = errors.New("mail provider disabled")

	errLoop = &Error{Kind: "mail_provider_loop", Permanent: true}

	namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	// transportFactory is swapped by tests (SetTransport).
	transportFactory = defaultTransport
)

func invalid(msg string) error { return apperr.BadRequest(msg) }

// Kind is a type of provider. smtp is built in; plugins register more (mail-pool pools).
type Kind interface {
	ID() string
	Label() string
	// SecretPurpose binds the sealed secret of this kind; "" means the kind keeps no secret.
	SecretPurpose() string
	// Normalize validates admin input and returns the canonical config to store.
	Normalize(raw json.RawMessage, enabled, secretSet bool) (json.RawMessage, error)
	// Open builds a provider for a stored entry; secret is the opened secret, "" when none is set.
	Open(ctx context.Context, h *Hub, s Stored, secret string) (Provider, error)
}

// SecretMerger is implemented by kinds whose secret is a set of named values (rest): Save passes the
// opened current secret and the admin's update, and seals what MergeSecret returns ("" = none).
type SecretMerger interface {
	MergeSecret(current, update string, config json.RawMessage) (string, error)
}

// Referrer is implemented by kinds whose providers send through other providers.
type Referrer interface {
	References(config json.RawMessage) []string
}

// Stored is one row of v2_mail_providers.
type Stored struct {
	Name      string
	Kind      string
	Enabled   bool
	Default   bool
	Config    json.RawMessage
	Sealed    string
	UpdatedAt string
}

// Hub resolves named providers. Plugins call Send (or Provider) with the provider name from their own
// settings; "" selects the default provider.
type Hub struct {
	db        store.Store
	secrets   *secretbox.Box
	transport Transport
	// httpClient serves the rest kind.
	httpClient *http.Client
	now        func() time.Time

	mu    sync.RWMutex
	kinds map[string]Kind
}

// NewHub returns a hub with the built-in SMTP and REST provider kinds.
func NewHub(db store.Store, secrets *secretbox.Box) *Hub {
	h := &Hub{db: db, secrets: secrets, transport: transportFactory(), httpClient: &http.Client{}, now: time.Now, kinds: map[string]Kind{}}
	h.RegisterKind(smtpKind{})
	h.RegisterKind(restKind{})
	return h
}

// RegisterKind adds a provider kind; plugins call it from Init.
func (h *Hub) RegisterKind(k Kind) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, dup := h.kinds[k.ID()]; dup {
		panic("mail: duplicate provider kind " + k.ID())
	}
	h.kinds[k.ID()] = k
}

func (h *Hub) kind(id string) (Kind, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	k, ok := h.kinds[id]
	return k, ok
}

// Kinds lists registered kinds sorted by ID.
func (h *Hub) Kinds() []Kind {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Kind, 0, len(h.kinds))
	for _, k := range h.kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Now is the hub clock, shared with kinds that keep time-windowed state.
func (h *Hub) Now() time.Time { return h.now() }

// Store is the hub's database, shared with kinds that keep their own state.
func (h *Hub) Store() store.Store { return h.db }

const providerColumns = "name, kind, enabled, is_default, config_json, secret_sealed, updated_at"

func scanStored(r interface{ Scan(...any) error }) (Stored, error) {
	var s Stored
	var cfg string
	var enabled, def int64
	err := r.Scan(&s.Name, &s.Kind, &enabled, &def, &cfg, &s.Sealed, &s.UpdatedAt)
	s.Enabled, s.Default, s.Config = enabled == 1, def == 1, json.RawMessage(cfg)
	return s, err
}

func (h *Hub) load(ctx context.Context, name string) (Stored, error) {
	q := "SELECT " + providerColumns + " FROM v2_mail_providers WHERE name = ?"
	args := []any{name}
	if name == "" {
		q, args = "SELECT "+providerColumns+" FROM v2_mail_providers WHERE is_default = 1 ORDER BY name LIMIT 1", nil
	}
	rows, err := h.db.Query(ctx, q, args...)
	if err != nil {
		return Stored{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Stored{}, err
		}
		return Stored{}, ErrNoProvider
	}
	return scanStored(rows)
}

// List returns every provider ordered by name.
func (h *Hub) List(ctx context.Context) ([]Stored, error) {
	rows, err := h.db.Query(ctx, "SELECT "+providerColumns+" FROM v2_mail_providers ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stored
	for rows.Next() {
		s, err := scanStored(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type depthKey struct{}

// maxDepth bounds pools of pools; a cycle fails instead of recursing forever.
const maxDepth = 4

func (h *Hub) open(ctx context.Context, s Stored) (Provider, error) {
	k, ok := h.kind(s.Kind)
	if !ok {
		return nil, fmt.Errorf("%w: kind %q is not registered", ErrDisabled, s.Kind)
	}
	secret := ""
	if s.Sealed != "" && k.SecretPurpose() != "" {
		b, err := h.secrets.Open(k.SecretPurpose(), s.Sealed)
		if err != nil {
			return nil, apperr.ErrUnavailable.Wrap(err)
		}
		secret = string(b)
	}
	return k.Open(ctx, h, s, secret)
}

// Provider resolves an enabled provider by name ("" = default).
func (h *Hub) Provider(ctx context.Context, name string) (Provider, error) {
	s, err := h.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if !s.Enabled {
		return nil, ErrDisabled
	}
	return h.open(ctx, s)
}

// Send delivers m through the named provider ("" = default).
func (h *Hub) Send(ctx context.Context, provider string, m Message) error {
	depth, _ := ctx.Value(depthKey{}).(int)
	if depth >= maxDepth {
		return errLoop
	}
	ctx = context.WithValue(ctx, depthKey{}, depth+1)
	s, err := h.load(ctx, provider)
	if err != nil {
		return err
	}
	if !s.Enabled {
		return ErrDisabled
	}
	p, err := h.open(ctx, s)
	if err != nil {
		return err
	}
	return h.deliver(ctx, s, p, m)
}

// deliver sends through an opened provider and counts the outcome against it. Pools are not counted:
// they send through the hub again, so the member that delivered is counted instead.
func (h *Hub) deliver(ctx context.Context, s Stored, p Provider, m Message) error {
	err := p.Send(ctx, m)
	if k, ok := h.kind(s.Kind); ok {
		if _, pool := k.(Referrer); !pool {
			h.count(ctx, s.Name, m.Purpose, err == nil)
		}
	}
	return err
}

func (h *Hub) count(ctx context.Context, provider, purpose string, ok bool) {
	if purpose == "" {
		purpose = "other"
	}
	sent, failed := 1, 0
	if !ok {
		sent, failed = 0, 1
	}
	if _, err := h.db.Exec(ctx, `INSERT INTO v2_mail_usage (day, provider, purpose, sent, failed) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(day, provider, purpose) DO UPDATE SET sent = v2_mail_usage.sent + excluded.sent, failed = v2_mail_usage.failed + excluded.failed`,
		h.now().UTC().Format("2006-01-02"), provider, purpose, sent, failed); err != nil {
		slog.ErrorContext(ctx, "mail usage count failed", "provider", provider, "err", err)
	}
}

// UsageRow is one day's counts for one provider and purpose.
type UsageRow struct {
	Day      string `json:"day"`
	Provider string `json:"provider"`
	Purpose  string `json:"purpose"`
	Sent     int    `json:"sent"`
	Failed   int    `json:"failed"`
}

// Usage returns the counts of the last days days (UTC), oldest first.
func (h *Hub) Usage(ctx context.Context, days int) ([]UsageRow, error) {
	since := h.now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := h.db.Query(ctx, "SELECT day, provider, purpose, sent, failed FROM v2_mail_usage WHERE day >= ? ORDER BY day, provider, purpose", since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Day, &u.Provider, &u.Purpose, &u.Sent, &u.Failed); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Exists reports whether a provider with this name is configured; plugins validate their settings with it.
func (h *Hub) Exists(ctx context.Context, name string) (bool, error) {
	_, err := h.load(ctx, name)
	if errors.Is(err, ErrNoProvider) {
		return false, nil
	}
	return err == nil, err
}

// Unavailable reports errors that mean "no usable provider", which callers treat as mail being switched off.
func Unavailable(err error) bool { return errors.Is(err, ErrNoProvider) || errors.Is(err, ErrDisabled) }

// Input is the admin PUT body for one provider.
type Input struct {
	Kind        string          `json:"kind"`
	Enabled     bool            `json:"enabled"`
	Default     bool            `json:"default"`
	Config      json.RawMessage `json:"config"`
	Secret      *string         `json:"secret"`
	ClearSecret bool            `json:"clearSecret"`
	SecretSet   *bool           `json:"secretSet"` // ignored; lets clients PUT back a GET response
	Name        *string         `json:"name"`      // ignored; the path names the provider
	UpdatedAt   *string         `json:"updatedAt"` // ignored
}

// Save creates or replaces provider name.
func (h *Hub) Save(ctx context.Context, name string, in Input) (Stored, error) {
	if !namePattern.MatchString(name) {
		return Stored{}, invalid("provider name must match " + namePattern.String())
	}
	k, ok := h.kind(in.Kind)
	if !ok {
		return Stored{}, invalid("unknown provider kind")
	}
	cur, err := h.load(ctx, name)
	switch {
	case errors.Is(err, ErrNoProvider):
		cur = Stored{}
	case err != nil:
		return Stored{}, err
	case cur.Kind != in.Kind:
		return Stored{}, apperr.New(http.StatusConflict, "conflict", "a provider's kind cannot change; delete and recreate it")
	}
	sealed, replace := cur.Sealed, false
	merger, merges := k.(SecretMerger)
	if in.Secret != nil && *in.Secret != "" && in.ClearSecret {
		return Stored{}, invalid("secret replacement and removal are exclusive")
	}
	switch {
	case k.SecretPurpose() == "":
		if in.Secret != nil && *in.Secret != "" {
			return Stored{}, invalid("this provider kind has no secret")
		}
	case merges:
		// Merged after the config is validated, below.
	case in.ClearSecret:
		sealed, replace = "", true
	case in.Secret != nil && *in.Secret != "":
		if len(*in.Secret) > 4096 || containsNUL(*in.Secret) {
			return Stored{}, invalid("invalid secret")
		}
		sealed, err = h.secrets.Seal(k.SecretPurpose(), []byte(*in.Secret))
		if errors.Is(err, secretbox.ErrNoKey) {
			return Stored{}, invalid("storing a provider secret requires PKI_MASTER_KEY")
		}
		if err != nil {
			return Stored{}, err
		}
		replace = true
	}
	if in.Enabled && sealed != "" && k.SecretPurpose() != "" {
		if _, err := h.secrets.Open(k.SecretPurpose(), sealed); err != nil {
			return Stored{}, invalid("stored secret cannot be decrypted; set it again")
		}
	}
	if len(in.Config) == 0 {
		in.Config = json.RawMessage("{}")
	}
	cfg, err := k.Normalize(in.Config, in.Enabled, sealed != "")
	if err != nil {
		return Stored{}, err
	}
	if merges {
		current, update := "", ""
		if cur.Sealed != "" && !in.ClearSecret {
			b, err := h.secrets.Open(k.SecretPurpose(), cur.Sealed)
			if err != nil {
				return Stored{}, invalid("stored secret cannot be decrypted; set it again")
			}
			current = string(b)
		}
		if in.Secret != nil {
			if len(*in.Secret) > 64<<10 || containsNUL(*in.Secret) {
				return Stored{}, invalid("invalid secret")
			}
			update = *in.Secret
		}
		merged, err := merger.MergeSecret(current, update, cfg)
		if err != nil {
			return Stored{}, err
		}
		sealed, replace = "", true
		if merged != "" {
			if sealed, err = h.secrets.Seal(k.SecretPurpose(), []byte(merged)); errors.Is(err, secretbox.ErrNoKey) {
				return Stored{}, invalid("storing a provider secret requires PKI_MASTER_KEY")
			} else if err != nil {
				return Stored{}, err
			}
		}
	}
	if r, ok := k.(Referrer); ok {
		for _, ref := range r.References(cfg) {
			if ref == name {
				return Stored{}, invalid("a provider cannot send through itself")
			}
			if _, err := h.load(ctx, ref); errors.Is(err, ErrNoProvider) {
				return Stored{}, invalid("unknown member provider " + ref)
			} else if err != nil {
				return Stored{}, err
			}
		}
	}
	now := h.now().UTC().Format(time.RFC3339Nano)
	stmts := []store.Statement{}
	if in.Default {
		stmts = append(stmts, store.Statement{Query: "UPDATE v2_mail_providers SET is_default = 0 WHERE name <> ?", Args: []any{name}})
	}
	// An unchanged secret keeps the value present at write time, not the one read above.
	stmts = append(stmts, store.Statement{Query: `INSERT INTO v2_mail_providers (name, kind, enabled, is_default, config_json, secret_sealed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET enabled = excluded.enabled, is_default = excluded.is_default, config_json = excluded.config_json,
		updated_at = excluded.updated_at, secret_sealed = CASE WHEN ? = 1 THEN excluded.secret_sealed ELSE v2_mail_providers.secret_sealed END`,
		Args: []any{name, in.Kind, b2i(in.Enabled), b2i(in.Default), string(cfg), sealed, now, now, b2i(replace)}})
	if _, err := h.db.Batch(ctx, stmts); err != nil {
		return Stored{}, err
	}
	return h.load(ctx, name)
}

// Delete removes a provider that no other provider sends through.
func (h *Hub) Delete(ctx context.Context, name string) error {
	all, err := h.List(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, s := range all {
		if s.Name == name {
			found = true
			continue
		}
		if k, ok := h.kind(s.Kind); ok {
			if r, ok := k.(Referrer); ok {
				for _, ref := range r.References(s.Config) {
					if ref == name {
						return apperr.New(http.StatusConflict, "conflict", "provider is used by "+s.Name)
					}
				}
			}
		}
	}
	if !found {
		return apperr.ErrNotFound
	}
	_, err = h.db.Exec(ctx, "DELETE FROM v2_mail_providers WHERE name = ?", name)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func containsNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}
