// Command worker runs the background jobs outside the API: the participant
// email outbox, Google Sheets / Forms sync, data retention and the rate limit
// sweep (security review F8).
//
// Running them here, with RUN_WORKERS=false on the API, means an API deploy
// or a crash in a request handler doesn't interrupt a send, and the jobs can
// connect as a member of event_pass_worker, which can't read sessions or
// sign-in links (migration 018). It reads the same environment as the API.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/app"
	"github.com/mayura-andrew/event-pass-platform/internal/config"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := postgres.Open(ctx, cfg.DatabaseURL, min(cfg.DBMaxConns, 4))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := app.CheckDatabaseLogin(ctx, db, cfg); err != nil {
		return err
	}

	keys, err := app.PassKeyRing(cfg)
	if err != nil {
		return err
	}
	passService := service.NewPassService(keys)
	passService.Assets = postgres.NewPassAssetRepo(db)
	mailer, err := app.NewMailer(cfg)
	if err != nil {
		return err
	}
	workers, err := app.NewWorkers(cfg, db, mailer, passService)
	if err != nil {
		return err
	}

	// A liveness endpoint for the container healthcheck, on loopback only.
	go func() {
		srv := &http.Server{Addr: "127.0.0.1:8081", ReadHeaderTimeout: 5 * time.Second,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
				defer cancel()
				if db.PingContext(ctx) != nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.WriteHeader(http.StatusOK)
			})}
		go func() { <-ctx.Done(); srv.Close() }()
		_ = srv.ListenAndServe()
	}()

	slog.Info("worker started")
	done := workers.Run(ctx)
	<-ctx.Done()
	slog.Info("worker shutting down")
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		slog.Warn("outbox still sending after 30s; exiting")
	}
	return nil
}
