//go:build !js

// slave keeps a local LDAP directory, trust share and triggers in sync with master (protocol v2).
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	joblifecycle "github.com/TING-HiuYu/SlurmPortal/internal/lifecycle/runtime"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugin"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/clusterstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/currentstatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/otel"
	"github.com/TING-HiuYu/SlurmPortal/internal/plugins/resourcestatus"
	"github.com/TING-HiuYu/SlurmPortal/internal/slave"
	"github.com/TING-HiuYu/SlurmPortal/internal/telemetry"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-plan" {
		if err := runPlan(os.Args[2:], os.Stdout); err != nil {
			slog.Error("plan failed", "err", err)
			os.Exit(1)
		}
		return
	}
	path := flag.String("config", "/etc/go-ldap-admin/slave.yml", "slave configuration (mode 0600)")
	once := flag.Bool("once", false, "run a single check and exit")
	flag.Parse()
	if err := run(*path, *once); err != nil {
		slog.Error("slave stopped", "err", err)
		os.Exit(1)
	}
}

func run(path string, once bool) error {
	cfg, err := slave.LoadConfig(path)
	if err != nil {
		return err
	}
	r, err := cfg.Build(os.Getenv)
	if err != nil {
		return err
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		tp, err := otel.Plugin{}.Provider(context.Background(), plugin.RoleSlave, func(any) error { return nil })
		if err != nil {
			return err
		}
		r.Telemetry = tp
		defer tp.Shutdown(context.Background())
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report := func(err error) {
		if err != nil {
			slog.Error("sync check failed", "err", err)
		}
		if f, ok := r.Telemetry.(telemetry.Flusher); ok {
			if err := f.Flush(ctx); err != nil {
				slog.Warn("telemetry flush failed", "err", err)
			}
		}
	}
	if once {
		err := r.Check(ctx)
		report(nil)
		return err
	}
	srv := &http.Server{Addr: cfg.HealthListen, Handler: r.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	if cfg.ClusterStatus.Enabled {
		col, err := clusterstatus.NewCollector(cfg.ClusterID, cfg.ClusterStatus.Config, nil, nil)
		if err != nil {
			return err
		}
		go slave.Reporter{Name: clusterstatus.ID, URL: r.MasterURL + clusterstatus.SnapshotPath, Client: r.Client, Interval: cfg.ClusterStatus.Interval,
			Collect: func(ctx context.Context, at time.Time) (any, error) { return col.Collect(ctx, at) }}.Run(ctx)
	}
	if cfg.ResourceStatus.Enabled {
		col, err := resourcestatus.NewCollector(cfg.ClusterID, cfg.ResourceStatus.Config, nil)
		if err != nil {
			return err
		}
		col.SetUsernames(r.Username)
		go slave.Reporter{Name: resourcestatus.ID, URL: r.MasterURL + resourcestatus.SnapshotPath, Client: r.Client, Interval: cfg.ResourceStatus.Interval,
			Collect: func(ctx context.Context, at time.Time) (any, error) { return col.Collect(ctx, at) }}.Run(ctx)
	}
	if cfg.CurrentStatus.Enabled {
		col, err := currentstatus.NewCollector(cfg.ClusterID, cfg.ResourceStatus.Config, cfg.ClusterStatus.Config)
		if err != nil {
			return err
		}
		col.SetUsernames(r.Username)
		go slave.Reporter{Name: currentstatus.ID, Method: http.MethodPut, URL: r.MasterURL + currentstatus.IngestPath, Client: r.Client, Interval: cfg.CurrentStatus.Interval,
			Collect: func(ctx context.Context, at time.Time) (any, error) { return col.Collect(ctx, at) }}.Run(ctx)
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("health listener", "err", err)
			stop()
		}
	}()
	if cfg.LifecycleFile != "" {
		lc, err := joblifecycle.Start(ctx, cfg.LifecycleFile, r.MasterURL, r.Client.Transport.(*http.Transport).TLSClientConfig)
		if err != nil {
			return err
		}
		defer func() {
			c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = lc.Close(c)
		}()
	}
	r.Run(ctx, cfg.CheckInterval, report)
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
