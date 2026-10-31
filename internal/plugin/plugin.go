// Package plugin is the compile-time plugin registry. Plugins register from init() in
// build-tagged files; configuration only enables what was compiled in.
package plugin

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	"github.com/TING-HiuYu/SlurmPortal/internal/audit"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/pki"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/secretbox"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

// Role is what a process runs as: standalone (all in one), master (the portal), slave (one per
// cluster) or gateway (web terminal and files on a login node).
type Role string

const (
	RoleStandalone Role = "standalone"
	RoleMaster     Role = "master"
	RoleSlave      Role = "slave"
	RoleGateway    Role = "gateway"
)

// Host is what a plugin receives at Init: the router to add routes to and the core modules it may use.
type Host struct {
	Role      Role
	Router    *httpx.Router
	Store     store.Store
	Telemetry telemetry.Provider
	Audit     *audit.Recorder
	Identity  *identity.Service
	PKI       *pki.Service
	Tokens    *authn.JWT
	Secrets   *secretbox.Box
	// Mail resolves named e-mail providers; plugins send through it and may register provider kinds.
	Mail *mail.Hub
	// Origins are the trusted browser origins (CORS allowlist).
	Origins []string
	// Clusters maps verified slave client certificates to cluster identities.
	Clusters Clusters
	Getenv   func(string) string
	// Decode unmarshals this plugin's own config section, rejecting unknown fields.
	Decode func(dst any) error
	// Services is shared by every plugin's Host: a plugin offers a helper, others look it up when
	// they handle a request (so plugin order does not matter).
	Services *Services
}

// Services is a small registry of helpers plugins offer each other.
type Services struct {
	mu sync.RWMutex
	m  map[string]any
}

// Provide registers a helper under name, replacing any earlier one.
func (s *Services) Provide(name string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]any{}
	}
	s.m[name] = v
}

// Lookup returns the helper registered under name. It is safe on a nil registry.
func (s *Services) Lookup(name string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[name]
	return v, ok
}

// Plugin is a compiled-in feature. Init runs once at start for each enabled plugin whose Roles
// include the process role; Close runs at shutdown in reverse order.
type Plugin interface {
	ID() string
	Roles() []Role
	Init(ctx context.Context, h *Host) error
	Close(ctx context.Context) error
}

// TelemetryPlugin is initialised before the router so its provider wraps every request.
type TelemetryPlugin interface {
	ID() string
	Roles() []Role
	Provider(ctx context.Context, role Role, decode func(dst any) error) (telemetry.Provider, error)
}

var (
	mu               sync.Mutex
	plugins          = map[string]Plugin{}
	telemetryPlugins = map[string]TelemetryPlugin{}
)

// Register adds a plugin to the registry; call it from init(). Duplicate IDs panic.
func Register(p Plugin) {
	mu.Lock()
	defer mu.Unlock()
	mustUnique(p.ID())
	plugins[p.ID()] = p
}

// RegisterTelemetry adds a telemetry plugin; call it from init(). Duplicate IDs panic.
func RegisterTelemetry(p TelemetryPlugin) {
	mu.Lock()
	defer mu.Unlock()
	mustUnique(p.ID())
	telemetryPlugins[p.ID()] = p
}

func mustUnique(id string) {
	if id == "" {
		panic("plugin: empty ID")
	}
	if _, ok := plugins[id]; ok {
		panic("plugin: duplicate ID " + id)
	}
	if _, ok := telemetryPlugins[id]; ok {
		panic("plugin: duplicate ID " + id)
	}
}

// Compiled lists every plugin ID linked into this binary.
func Compiled() []string {
	mu.Lock()
	defer mu.Unlock()
	ids := make([]string, 0, len(plugins)+len(telemetryPlugins))
	for id := range plugins {
		ids = append(ids, id)
	}
	for id := range telemetryPlugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Selection resolves enabled IDs against compiled plugins and the node role.
type Selection struct {
	Telemetry []TelemetryPlugin
	Plugins   []Plugin
}

// Select returns the enabled plugins in the order given, failing on an unknown ID or one that does
// not support role.
func Select(role Role, enabled []string) (Selection, error) {
	mu.Lock()
	defer mu.Unlock()
	var sel Selection
	seen := map[string]bool{}
	for _, id := range enabled {
		if seen[id] {
			return sel, fmt.Errorf("plugin %q enabled twice", id)
		}
		seen[id] = true
		if t, ok := telemetryPlugins[id]; ok {
			if !slices.Contains(t.Roles(), role) {
				return sel, fmt.Errorf("plugin %q does not support role %s", id, role)
			}
			sel.Telemetry = append(sel.Telemetry, t)
			continue
		}
		p, ok := plugins[id]
		if !ok {
			return sel, fmt.Errorf("plugin %q is not compiled into this binary", id)
		}
		if !slices.Contains(p.Roles(), role) {
			return sel, fmt.Errorf("plugin %q does not support role %s", id, role)
		}
		sel.Plugins = append(sel.Plugins, p)
	}
	return sel, nil
}
