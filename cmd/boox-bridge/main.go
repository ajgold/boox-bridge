// boox-bridge ingests Boox .note files dropped via WebDAV, parses + renders
// pages, runs handwriting recognition via the llm.jacomail.com Claude
// gateway, and creates per-note docs in self-hosted Affine.
//
// Single binary, systemd-managed. All state lives under the data dir
// (default /var/lib/boox) — inbox/ (WebDAV drop), archive/ (success),
// dlq/ (failure), state/ (seen.json + spend.json).
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	cfg, err := loadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(2)
	}
	slog.Info("starting",
		"version", "v0",
		"data_dir", cfg.DataDir,
		"inbox", cfg.InboxDir(),
		"llm", cfg.LLMGatewayURL,
		"affine_mcp", cfg.AffineMCPURL,
		"max_daily_usd", cfg.MaxDailyUSD,
		"max_pages", cfg.MaxPagesPerNote,
	)

	dedup, err := openDedup(cfg.StateDir())
	if err != nil {
		slog.Error("dedup", "err", err)
		os.Exit(1)
	}
	spend, err := openSpend(cfg.StateDir(), cfg.MaxDailyUSD)
	if err != nil {
		slog.Error("spend", "err", err)
		os.Exit(1)
	}

	hwr := newHWRClient(cfg)
	hwr.spend = spend
	affine := newAffineClient(cfg)
	spool := newSpool(cfg)

	routes, err := loadRoutes(cfg.RoutesPath(), routeTarget{
		WorkspaceID: cfg.AffineWorkspace,
		ParentDocID: cfg.AffineParentDoc,
	})
	if err != nil {
		if errors.Is(err, errMissingBootstrap) {
			slog.Error("routes_bootstrap_missing", "err", err)
		} else {
			slog.Error("routes_load", "err", err)
		}
		os.Exit(1)
	}
	slog.Info("routes_loaded", "path", cfg.RoutesPath(), "mappings", len(routes.Get().Mappings))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := startupSelfCheck(startupCtx, cfg, hwr, affine); err != nil {
		slog.Error("startup_self_check", "err", err)
		os.Exit(1)
	}
	slog.Info("startup_self_check ok")

	pipe := &pipeline{
		cfg:    cfg,
		dedup:  dedup,
		spend:  spend,
		hwr:    hwr,
		affine: affine,
		spool:  spool,
		routes: routes,
	}

	go spool.run(ctx)

	// SIGHUP triggers a reload of routes.json — lets the admin hand-edit
	// the file and apply changes without a service restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if err := routes.Reload(); err != nil {
					slog.Error("routes_reload_failed", "err", err)
				} else {
					slog.Info("routes_reloaded", "mappings", len(routes.Get().Mappings))
				}
			}
		}
	}()

	web := newWebServer(cfg, dedup, spend, affine, routes)
	go func() {
		if err := web.listen(ctx); err != nil {
			slog.Error("web_ui_exit", "err", err)
		}
	}()

	if err := watch(ctx, cfg, pipe.process); err != nil {
		slog.Error("watcher_exit", "err", err)
		os.Exit(1)
	}
	slog.Info("shutdown clean")
}
