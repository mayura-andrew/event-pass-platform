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
	"path/filepath"
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
	ingestRepo := postgres.NewIngestRepo(db)
	fieldRepo := postgres.NewFieldRepo(db)
	emailRepo := postgres.NewEmailRepo(db)

	mailer, err := newMailer(cfg)
	if err != nil {
		return err
	}

	logo := loadEmailLogo(cfg.PublicDir)

	// Pass email queue. The worker only runs when SMTP is configured; the
	// status endpoint works either way.
	emailCfg := service.EmailWorkerConfig{
		Interval:   cfg.EmailSendInterval,
		DailyLimit: cfg.EmailDailyLimit,
		BaseURL:    cfg.AppBaseURL,
		EventName:  cfg.EventName,
		Logo:       logo,
	}
	var emailWorker *service.EmailWorker
	if cfg.SMTPHost != "" {
		emailWorker = service.NewEmailWorker(emailRepo, mailer, emailCfg)
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
		Dashboard:    service.NewDashboardService(dashboardRepo, scenarios, fieldRepo, cfg.CheckInCode),
		Fields:       service.NewFieldService(fieldRepo),
		Emails:       service.NewEmailQueueService(emailRepo, emailWorker, emailCfg),
		Feed:         feed,
		Staff:        service.NewStaffService(staffRepo, mailer, cfg.AppBaseURL, cfg.EventName, logo),
		Ingest:       newIngest(ingestRepo, cfg.HMACSecretKey),
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

	// The email worker stops with ctx too, after recording any message it
	// is in the middle of sending; shutdown waits for that below.
	workerDone := make(chan struct{})
	if emailWorker != nil {
		go func() {
			defer close(workerDone)
			emailWorker.Run(ctx)
		}()
	} else {
		close(workerDone)
	}

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
	err = srv.Shutdown(shutdownCtx)
	select {
	case <-workerDone:
	case <-time.After(60 * time.Second):
		slog.Warn("email worker still sending at shutdown; an interrupted job is retried on the next start")
	}
	return err
}

// newIngest returns nil when HMAC_SECRET_KEY is unset: the dashboard then
// answers 503 instead of creating delegates whose passes can't be signed.
func newIngest(repo *postgres.IngestRepo, key []byte) *service.IngestService {
	if len(key) == 0 {
		slog.Warn("HMAC_SECRET_KEY not set: delegates can't be added from the dashboard")
		return nil
	}
	return service.NewIngestService(repo, key)
}

// emailLogoFile is the white event logo sized for email headers, made from
// public/assets/Colombo LW.png (README: "Changing the logo").
const emailLogoFile = "assets/colombo-logo-email.png"

// loadEmailLogo reads the email header logo once. Without it emails still go
// out, with the event name in text, so a missing file is only a warning.
func loadEmailLogo(publicDir string) service.EmailLogo {
	b, err := os.ReadFile(filepath.Join(publicDir, emailLogoFile))
	if err != nil {
		slog.Warn("email logo not found: emails use a text header", "file", emailLogoFile, "err", err)
		return nil
	}
	return b
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
