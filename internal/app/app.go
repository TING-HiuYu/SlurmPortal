// Package app wires the HTTP pipeline, plugins and modules. Platform entry points supply the Store.
package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/TING-HiuYu/SlurmPortal/internal/audit"
	"github.com/TING-HiuYu/SlurmPortal/internal/authn"
	"github.com/TING-HiuYu/SlurmPortal/internal/authz"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/auth"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/identity"
	"github.com/TING-HiuYu/SlurmPortal/internal/domain/pki"
	"github.com/TING-HiuYu/SlurmPortal/internal/httpx"
	"github.com/TING-HiuYu/SlurmPortal/internal/legacyimport"
	"github.com/TING-HiuYu/SlurmPortal/internal/mail"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/replication"
	"github.com/TING-HiuYu/SlurmPortal/internal/secretbox"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
	"github.com/TING-HiuYu/SlurmPortal/migrations"
)

// App is a wired master (or standalone) instance: the HTTP handler plus the modules that platform
// entry points need to reach directly (audit, telemetry, the replication publisher).
type App struct {
	Handler   http.Handler
	Router    *httpx.Router
	Audit     *audit.Recorder
	Telemetry telemetry.Provider
	Policies  *authz.Casbin
	JWT       *authn.JWT
	Auth      *auth.Service
	Identity  *identity.Service
	PKI       *pki.Service
	Publisher *replication.DNSPublisher
	Mail      *mail.Hub
	plugins   []plugin.Plugin
	writes    atomic.Bool
}

// New migrates the database, imports v1 data once, starts the enabled plugins and builds the
// request pipeline. The same function runs natively (cmd/server) and inside the Worker (cmd/worker).
func New(ctx context.Context, cfg *config.Config, db store.Store) (*App, error) {
	if _, err := store.Migrate(ctx, db, migrations.FS); err != nil {
		return nil, err
	}
	rep, err := legacyimport.Run(ctx, db, time.Now())
	if err != nil {
		// Another instance importing at the same moment makes our inserts conflict; each step only
		// runs while its v2 table is empty, so a second pass skips what the winner imported.
		rep, err = legacyimport.Run(ctx, db, time.Now())
	}
	if err != nil {
		return nil, fmt.Errorf("import v1 data: %w", err)
	} else if !rep.Empty() {
		slog.WarnContext(ctx, "imported v1 data", "webAccess", rep.WebAccess, "notification", rep.Notification, "resourceSummaries", rep.Summaries)
	}
	sel, err := plugin.Select(cfg.Role, cfg.Plugins.Enabled)
	if err != nil {
		return nil, err
	}
	tp, err := telemetryProvider(ctx, cfg, sel.Telemetry)
	if err != nil {
		return nil, err
	}
	telemetry.SetSampleRatio(cfg.Telemetry.SampleRatio)
	if _, noop := tp.(telemetry.Noop); !noop {
		db = telemetry.TracedStore{Store: db, Provider: tp}
	}
	rec, err := audit.NewRecorder(cfg.Audit.Enabled, audit.SQLStore{DB: db, MaxRecords: cfg.Audit.MaxRecords})
	if err != nil {
		return nil, err
	}
	revocations := auth.Revocations{DB: db, Now: time.Now}
	jwt, err := authn.NewJWT(cfg.Auth.Issuer, cfg.Auth.Audience, cfg.Auth.TokenTTL, cfg.JWTKeys, identity.Directory{DB: db}, revocations)
	if err != nil {
		return nil, err
	}
	imported, skipped, err := authz.ImportLegacy(ctx, db)
	if err != nil {
		return nil, err
	}
	if len(imported) > 0 || len(skipped) > 0 {
		slog.InfoContext(ctx, "imported legacy casbin policy", "rules", len(imported), "skipped", skipped)
	}
	rules, err := authz.LoadRules(ctx, db)
	if err != nil {
		return nil, err
	}
	policies, err := authz.NewCasbin(rules)
	if err != nil {
		return nil, err
	}

	ip := httpx.RemoteAddrIP
	if cfg.HTTP.TrustedIPHeader != "" {
		ip = httpx.TrustedHeaderIP(cfg.HTTP.TrustedIPHeader)
	}
	rt := httpx.NewRouter(jwt, &authz.Refreshing{Casbin: policies, TTL: 10 * time.Second, Now: time.Now,
		Load: func(ctx context.Context) ([]authz.Rule, error) { return authz.LoadRules(ctx, db) }})
	rt.SetPeerAuthenticator(authn.AnyPeer{authn.TLSPeer{Fingerprints: cfg.Sync.PeerFingerprints}, authn.BridgePeer{}})
	a := &App{Router: rt, Audit: rec, Telemetry: tp, Policies: policies, JWT: jwt}
	rt.Use(
		httpx.WithScope(),
		telemetry.HTTPMiddleware(tp, httpx.RouteOf),
		httpx.Recover(),
		httpx.SecurityHeaders(),
		httpx.RateLimit(cfg.HTTP.RatePerSecond, cfg.HTTP.RateBurst, ip),
		httpx.CORS(cfg.HTTP.CORSOrigins),
		a.trackWrites,
		audit.Middleware(rec),
	)
	registerHealth(rt)
	authService := auth.New(db, jwt, revocations)
	if k := cfg.JWTKeys.Keys[cfg.JWTKeys.ActiveKID]; len(k) > 0 {
		// Reset codes are stored as an HMAC under a key derived from the active JWT key.
		sum := sha256.Sum256(append([]byte("password-reset-key\x00"), k...))
		authService.ResetKey = sum[:]
	}
	authService.Register(rt, ip)
	identityService := &identity.Service{DB: db, Policies: policies, Layout: identity.Layout{
		BaseDN: cfg.Directory.BaseDN, UserDN: cfg.Directory.UserDN, DefaultShell: cfg.Directory.DefaultShell,
	}}
	identityService.Register(rt)
	if cfg.BootstrapAdmin.Password != "" {
		created, err := identityService.BootstrapAdmin(ctx, cfg.BootstrapAdmin.Username, cfg.BootstrapAdmin.Password)
		if err != nil {
			return nil, fmt.Errorf("bootstrap admin: %w", err)
		}
		if created {
			slog.WarnContext(ctx, "bootstrap administrator created; remove BOOTSTRAP_ADMIN_PASSWORD", "username", cfg.BootstrapAdmin.Username)
		}
	}
	pkiService, err := pki.New(ctx, db, cfg.PKIMasterKey)
	if err != nil {
		return nil, err
	}
	identityService.OnDeactivate = pkiService.RevokeUser
	pkiService.Register(rt, identityService.CanManage)
	replication.Register(rt, db, cfg.Directory.BaseDN)
	audit.Register(rt, rec, audit.SQLStore{DB: db, MaxRecords: cfg.Audit.MaxRecords})
	if cfg.Sync.DNSName != "" {
		a.Publisher = &replication.DNSPublisher{DB: db, BaseDN: cfg.Directory.BaseDN, Name: cfg.Sync.DNSName, ZoneID: cfg.Sync.ZoneID,
			RecordID: cfg.Sync.RecordID, Token: cfg.Sync.CloudflareToken, TTL: cfg.Sync.TTL}
	}

	a.Auth, a.Identity, a.PKI = authService, identityService, pkiService
	secrets, err := secretbox.New(cfg.PKIMasterKey)
	if err != nil {
		return nil, err
	}
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	mailHub := mail.NewHub(db, secrets)
	mailHub.Register(rt)
	authService.Mailer = mailHub
	a.Mail = mailHub
	services := &plugin.Services{}
	for _, p := range sel.Plugins {
		h := &plugin.Host{Role: cfg.Role, Router: rt, Store: db, Telemetry: tp, Audit: rec, Identity: identityService, PKI: pkiService, Tokens: jwt,
			Secrets: secrets, Mail: mailHub, Origins: cfg.HTTP.CORSOrigins, Clusters: cfg.Clusters, Getenv: getenv, Decode: cfg.PluginDecoder(p.ID()), Services: services}
		if err := p.Init(ctx, h); err != nil {
			return nil, errors.Join(err, a.Close(ctx))
		}
		a.plugins = append(a.plugins, p)
	}
	registerAccess(rt, a.plugins)
	a.Handler = rt.Handler()
	return a, nil
}

func telemetryProvider(ctx context.Context, cfg *config.Config, tps []plugin.TelemetryPlugin) (telemetry.Provider, error) {
	var multi telemetry.Multi
	for _, t := range tps {
		p, err := t.Provider(ctx, cfg.Role, cfg.PluginDecoder(t.ID()))
		if err != nil {
			return nil, errors.Join(err, multi.Shutdown(ctx))
		}
		multi = append(multi, p)
	}
	if len(multi) == 0 {
		return telemetry.Noop{}, nil
	}
	return multi, nil
}

// Tick runs periodic maintenance; native entry points call it hourly, the Worker from a cron trigger.
func (a *App) Tick(ctx context.Context) error {
	return errors.Join(a.PKI.RenewExpiring(ctx), a.Publish(ctx))
}

// Publish announces the current snapshot hash; it is a no-op when unchanged or unconfigured.
func (a *App) Publish(ctx context.Context) error {
	if a.Publisher == nil {
		return nil
	}
	_, err := a.Publisher.Publish(ctx)
	return err
}

// TakeWrites reports (and clears) whether a successful write happened since the last call.
func (a *App) TakeWrites() bool { return a.writes.Swap(false) }

// Flush exports buffered telemetry; the Worker calls it after each request because it has no idle time.
func (a *App) Flush(ctx context.Context) error {
	if f, ok := a.Telemetry.(telemetry.Flusher); ok {
		return f.Flush(ctx)
	}
	return nil
}

// trackWrites flags a successful write so the caller republishes the sync snapshot. Machine
// reports under /sync/ (status, registrations, job and transfer events) never change users, groups
// or certificates, so they do not trigger a snapshot rebuild; the periodic Tick still publishes.
func (a *App) trackWrites(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !strings.HasPrefix(r.URL.Path, "/sync/") {
			if s := httpx.ScopeFrom(r.Context()); s != nil && s.Status > 0 && s.Status < 400 {
				a.writes.Store(true)
			}
		}
	})
}

// Close shuts plugins down in reverse start order and returns every error they report.
func (a *App) Close(ctx context.Context) error {
	var errs []error
	for i := len(a.plugins) - 1; i >= 0; i-- {
		errs = append(errs, a.plugins[i].Close(ctx))
	}
	if a.Telemetry != nil {
		errs = append(errs, a.Telemetry.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func registerHealth(rt *httpx.Router) {
	rt.Handle(httpx.Route{Method: http.MethodGet, Path: "/healthz", Permission: authz.Public(),
		Handler: func(w http.ResponseWriter, _ *http.Request) error {
			httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return nil
		}})
}
