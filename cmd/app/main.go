// Command app is the bank0 core-banking backend.
//
// Usage:
//
//	bank0                 serve the HTTP API/console (default)
//	bank0 serve           same as above
//	bank0 migrate up      apply embedded migrations  (Helm pre-upgrade Job)
//	bank0 migrate down    roll back one migration
//	bank0 migrate status  show migration status
//	bank0 maintenance     run expire_holds + cleanup once (CronJob alternative)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/minhtt159/bank0/internal/api"
	"github.com/minhtt159/bank0/internal/config"
	"github.com/minhtt159/bank0/internal/db"
	"github.com/minhtt159/bank0/internal/logger"
	"github.com/minhtt159/bank0/internal/migrate"
)

func main() {
	cfg, err := config.LoadConfig(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load: %v\n", err)
		os.Exit(1)
	}
	log := logger.New(cfg.Logging.Level, cfg.Logging.Encoding)

	if err := cfg.Validate(); err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "serve":
		serve(cfg, log)
	case "migrate":
		runMigrate(cfg, log)
	case "maintenance":
		runMaintenanceOnce(cfg, log)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (serve|migrate|maintenance)\n", cmd)
		os.Exit(2)
	}
}

func runMigrate(cfg config.Config, log *slog.Logger) {
	sub := "up"
	if len(os.Args) > 2 {
		sub = os.Args[2]
	}
	var err error
	switch sub {
	case "up":
		err = migrate.Up(cfg.Database.DSN)
	case "down":
		err = migrate.Down(cfg.Database.DSN)
	case "status":
		err = migrate.Status(cfg.Database.DSN)
	default:
		err = fmt.Errorf("unknown migrate subcommand %q", sub)
	}
	if err != nil {
		log.Error("migrate", "err", err)
		os.Exit(1)
	}
	log.Info("migrate done", "sub", sub)
}

func runMaintenanceOnce(cfg config.Config, log *slog.Logger) {
	pg, err := db.NewPostgres(cfg.Database)
	if err != nil {
		log.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	expired, cleaned, sessions, verifExpired, reconcileIssues, ran, err := pg.RunMaintenance(ctx)
	if err != nil {
		log.Error("maintenance", "err", err)
		os.Exit(1)
	}
	log.Info("maintenance", "ran", ran, "holds_expired", expired, "keys_cleaned", cleaned, "sessions_cleaned", sessions, "verifications_expired", verifExpired, "reconcile_issues", reconcileIssues)
	if reconcileIssues > 0 {
		log.Warn("reconcile drift detected — the ledger/cache invariants do not hold", "issues", reconcileIssues)
	}
}

func serve(cfg config.Config, log *slog.Logger) {
	log.Info("starting", "app", cfg.App.Name, "version", cfg.App.Version, "env", cfg.App.Env, "mode", cfg.Server.Mode)

	if cfg.Server.AutoMigrate {
		log.Info("auto-migrate enabled; applying migrations")
		if err := migrate.Up(cfg.Database.DSN); err != nil {
			log.Error("auto-migrate", "err", err)
			os.Exit(1)
		}
	}

	pg, err := db.NewPostgres(cfg.Database)
	if err != nil {
		log.Error("db connect", "err", err)
		os.Exit(1)
	}
	defer pg.Close()
	log.Info("connected to postgres")

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Server.Port),
		Handler:      api.NewServer(cfg, log, pg).Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Admin.RunMaintenance {
		go runMaintenanceLoop(ctx, log, pg, cfg.Admin.MaintenanceInterval)
	}

	go func() {
		log.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("forced shutdown", "err", err)
	}
	log.Info("stopped")
}

// runMaintenanceLoop ticks expire_holds + cleanup. Guarded by a Postgres
// advisory lock (in RunMaintenance) so multiple replicas don't duplicate work.
func runMaintenanceLoop(ctx context.Context, log *slog.Logger, pg *db.Postgres, every time.Duration) {
	if every <= 0 {
		every = 60 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			expired, cleaned, sessions, verifExpired, reconcileIssues, ran, err := pg.RunMaintenance(ctx)
			if err != nil {
				log.Warn("maintenance", "err", err)
				continue
			}
			if ran && (expired > 0 || cleaned > 0 || sessions > 0 || verifExpired > 0) {
				log.Info("maintenance", "holds_expired", expired, "keys_cleaned", cleaned, "sessions_cleaned", sessions, "verifications_expired", verifExpired)
			}
			if ran && reconcileIssues > 0 {
				// The correctness oracle found drift — page on this in prod.
				log.Warn("reconcile drift detected — the ledger/cache invariants do not hold", "issues", reconcileIssues)
			}
		}
	}
}
