// Command api runs the NSAC QR delegate management API and serves the PWA.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
	"github.com/nasa-spaceapps/nsac-qr/internal/config"
	httphandler "github.com/nasa-spaceapps/nsac-qr/internal/handler/http"
	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
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

	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	// Repositories
	adminRepo := postgres.NewAdminRepo(db)
	participantRepo := postgres.NewParticipantRepo(db)
	scenarioRepo := postgres.NewScenarioRepo(db)
	scanRepo := postgres.NewScanRepo(db)
	dashboardRepo := postgres.NewDashboardRepo(db)
	staffRepo := postgres.NewStaffRepo(db)

	mailer, err := newMailer(cfg)
	if err != nil {
		return err
	}

	// Services
	tokens := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	scenarios := service.NewScenarioService(scenarioRepo)
	feed := service.NewScanFeed(dashboardRepo)
	h := &httphandler.Handler{
		Auth:         service.NewAuthService(adminRepo, tokens),
		Sync:         service.NewSyncService(participantRepo, scanRepo, scenarioRepo, staffRepo),
		Participants: service.NewParticipantService(participantRepo),
		Scenarios:    scenarios,
		Dashboard:    service.NewDashboardService(dashboardRepo, scenarios, cfg.CheckInCode),
		Feed:         feed,
		Staff:        service.NewStaffService(staffRepo, mailer, cfg.AppBaseURL, cfg.EventName),
		Accounts:     adminRepo,

		ClientIPHeader:  cfg.ClientIPHeader,
		LoginRatePerMin: cfg.LoginRatePerMin,
		PassRatePerMin:  cfg.PassRatePerMin,
		Tokens:          tokens,
		MapsAPIKey:      cfg.GoogleMapsAPIKey,
		MapsMapID:       cfg.GoogleMapsMapID,
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httphandler.NewRouter(h, cfg.PublicDir, cfg.AllowedOrigin),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// The live scan feed stops with ctx; that closes every open dashboard
	// stream so Shutdown below isn't held up waiting for them.
	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go feed.Run(feedCtx)
	srv.RegisterOnShutdown(stopFeed)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// newMailer builds the SMTP mailer, or a disabled one when SMTP_HOST is unset
// (staff provisioning then answers 503 instead of creating unusable accounts).
func newMailer(cfg *config.Config) (service.Mailer, error) {
	if cfg.SMTPHost == "" {
		slog.Warn("SMTP_HOST not set: staff accounts can't be created from the dashboard")
		return service.DisabledMailer(), nil
	}
	from, err := mail.ParseAddress(cfg.SMTPFrom)
	if err != nil {
		return nil, fmt.Errorf("SMTP_FROM: %w", err)
	}
	m, err := service.NewSMTPMailer(service.SMTPConfig{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		Username: cfg.SMTPUsername,
		Password: cfg.SMTPPassword,
		From:     *from,
		Security: cfg.SMTPSecurity,
	})
	if err != nil {
		return nil, err
	}
	slog.Info("email enabled", "smtp_host", cfg.SMTPHost, "smtp_port", cfg.SMTPPort, "security", cfg.SMTPSecurity)

	// Check in the background so a slow mail server never delays startup.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := m.Probe(ctx); err != nil {
			slog.Error("SMTP CHECK FAILED: staff credential emails will not send",
				"smtp_host", cfg.SMTPHost, "smtp_port", cfg.SMTPPort, "err", err,
				"hint", "timeout = the host blocks outbound SMTP; auth error = wrong username or app password")
			return
		}
		slog.Info("SMTP check passed: connected and signed in", "smtp_host", cfg.SMTPHost)
	}()
	return m, nil
}
