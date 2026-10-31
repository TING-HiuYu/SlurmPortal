//go:build !js

// server runs the portal natively (Go HTTP server and SQLite) instead of on a Cloudflare Worker,
// for development and for sites without Cloudflare. It serves the same API as cmd/worker.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/TING-HiuYu/SlurmPortal/internal/app"
	"github.com/TING-HiuYu/SlurmPortal/internal/config"
	_ "github.com/TING-HiuYu/SlurmPortal/internal/plugins/all"
	"github.com/TING-HiuYu/SlurmPortal/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "", "YAML configuration file (optional; environment variables override it)")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sqldb, err := sql.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return err
	}
	sqldb.SetMaxOpenConns(1)
	db := store.SQL{DB: sqldb}
	defer db.Close()

	a, err := app.New(ctx, cfg, db)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.HTTP.Listen,
		Handler:           behindProxy(cfg.HTTP.BehindTLSProxy, withStatic(cfg.HTTP.StaticDir, a.Handler)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("listening", "addr", cfg.HTTP.Listen, "role", cfg.Role, "audit", a.Audit.Enabled())
	var syncSrv *http.Server
	if cfg.Sync.Listen != "" {
		if syncSrv, err = syncServer(cfg, a.Handler); err != nil {
			return err
		}
		go func() { errc <- syncSrv.ListenAndServeTLS("", "") }()
		slog.Info("sync listening", "addr", cfg.Sync.Listen)
	}
	go func() {
		hourly := time.NewTicker(time.Hour)
		publish := time.NewTicker(5 * time.Second)
		defer hourly.Stop()
		defer publish.Stop()
		lastPublish := time.Time{}
		tick := func() {
			if err := a.Tick(ctx); err != nil {
				slog.Error("periodic maintenance failed", "err", err)
			}
		}
		tick()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hourly.C:
				tick()
			case <-publish.C:
				if err := a.Flush(ctx); err != nil {
					slog.Error("telemetry flush failed", "err", err)
				}
				if a.TakeWrites() || time.Since(lastPublish) > time.Minute {
					lastPublish = time.Now()
					if err := a.Publish(ctx); err != nil {
						slog.Error("publish snapshot hash failed", "err", err)
					}
				}
			}
		}
	}()

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	errs := []error{ignoreClosed(err), srv.Shutdown(shutdown)}
	if syncSrv != nil {
		errs = append(errs, syncSrv.Shutdown(shutdown))
	}
	return errors.Join(append(errs, a.Close(shutdown))...)
}

// syncServer serves the machine routes (/sync/*) and /healthz, only to clients with a certificate from
// client-ca-file. Slaves and gateways connect here; browsers use the plain listener.
func syncServer(cfg *config.Config, h http.Handler) (*http.Server, error) {
	if cfg.Sync.CertFile == "" || cfg.Sync.KeyFile == "" || cfg.Sync.ClientCAFile == "" {
		return nil, errors.New("sync.listen requires cert-file, key-file and client-ca-file")
	}
	cert, err := tls.LoadX509KeyPair(cfg.Sync.CertFile, cfg.Sync.KeyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(cfg.Sync.ClientCAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("no certificates in sync.client-ca-file")
	}
	mux := http.NewServeMux()
	mux.Handle("/sync/", h)
	mux.Handle("GET /healthz", h)
	return &http.Server{
		Addr: cfg.Sync.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 120 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert},
	}, nil
}

// behindProxy marks a request that reached the proxy over HTTPS as secure, the same way the Worker
// edge does, so HTTPS-only actions (sign-in, private keys, OAuth) work behind a TLS proxy.
func behindProxy(on bool, next http.Handler) http.Handler {
	if !on {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			r = r.Clone(r.Context())
			r.TLS = &tls.ConnectionState{}
		}
		next.ServeHTTP(w, r)
	})
}

// withStatic serves the web app from dir for every path the API does not own, falling back to
// index.html so client-side routes load. Without dir it returns api unchanged.
func withStatic(dir string, api http.Handler) http.Handler {
	if dir == "" {
		return api
	}
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/sync/") || p == "/healthz" {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(p, "/assets/") {
			// Vite names assets by content hash; a missing chunk must 404, not return the app page.
			h.Set("Cache-Control", "public, max-age=31536000, immutable")
			files.ServeHTTP(w, r)
			return
		}
		if fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path.Clean("/"+p)))); err != nil || fi.IsDir() {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
