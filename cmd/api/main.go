// Command api runs the event pass platform API.
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

	"github.com/mayura-andrew/event-pass-platform/internal/config"
	httphandler "github.com/mayura-andrew/event-pass-platform/internal/handler/http"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/secrets"
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

	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	// Repositories
	accountRepo := postgres.NewAccountRepo(db)
	orgRepo := postgres.NewOrgRepo(db)
	eventRepo := postgres.NewEventRepo(db)
	registrationRepo := postgres.NewRegistrationRepo(db)
	outboxRepo := postgres.NewOutboxRepo(db)
	passKeys, err := passKeyRing(cfg)
	if err != nil {
		return err
	}
	passService := service.NewPassService(passKeys)
	passAssets := postgres.NewPassAssetRepo(db)
	passService.Assets = passAssets
	checkinRepo := postgres.NewCheckinRepo(db)
	liveFeed := service.NewEventFeed(checkinRepo)

	mailer, err := newMailer(cfg)
	if err != nil {
		return err
	}

	// Services
	h := &httphandler.Handler{
		ClientIPHeader:  cfg.ClientIPHeader,
		LoginRatePerMin: cfg.LoginRatePerMin,
		PassRatePerMin:  cfg.PassRatePerMin,
		MapsAPIKey:      cfg.GoogleMapsAPIKey,
		MapsMapID:       cfg.GoogleMapsMapID,

		SignIn: service.NewAccountService(accountRepo, mailer, newGoogle(cfg), service.AccountConfig{
			BaseURL: cfg.AppBaseURL, Product: cfg.ProductName, SessionTTL: cfg.SessionTTL,
		}),
		Orgs:          service.NewOrgService(orgRepo, mailer, cfg.AppBaseURL, cfg.ProductName),
		Events:        service.NewEventService(eventRepo, accountRepo, orgRepo, mailer, cfg.AppBaseURL, cfg.ProductName),
		Registrations: service.NewRegistrationService(registrationRepo, eventRepo, outboxRepo),
		Passes:        passService,
		Templates:     service.NewPassTemplateService(postgres.NewPassTemplateRepo(db), passAssets, passService),
		Portal:        service.NewPortalService(postgres.NewPortalRepo(db)),
		Checker:       registrationRepo,
		LiveFeed:      liveFeed,
		BaseURL:       cfg.AppBaseURL,
		Product:       cfg.ProductName,
		CookieKey:     cfg.JWTSecret,
	}

	box, err := integrationsBox(cfg)
	if err != nil {
		return err
	}
	h.Integrations = service.NewIntegrationService(postgres.NewIntegrationRepo(db), registrationRepo, newGoogle(cfg),
		service.NewGoogleAPI(), box, cfg.AppBaseURL+"/auth/google/integration/callback")

	h.StatusPageURL = cfg.StatusPageURL
	h.Profiles = service.NewProfileService(postgres.NewPortalRepo(db), postgres.NewProfileRepo(db))
	h.MapTileURL, h.MapTileAttribution = cfg.MapTileURL, cfg.MapTileAttribution
	h.Health = service.NewHealth(postgres.NewHealthRepo(db), cfg.SMTPHost != "")

	h.Checkins = service.NewCheckinService(checkinRepo, postgres.NewWorkshopRepo(db), h.Registrations, registrationRepo,
		passService, registrationRepo)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           httphandler.NewRouter(h, cfg.PublicDir, cfg.AllowedOrigin),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	feedCtx, stopFeed := context.WithCancel(ctx)
	defer stopFeed()
	go liveFeed.Run(feedCtx)
	go h.Integrations.Run(feedCtx)
	srv.RegisterOnShutdown(stopFeed)

	// Participant emails (pass issued, approval, waitlist). Like the pass
	// email queue, only with SMTP configured; queued emails wait otherwise.
	outboxDone := make(chan struct{})
	if cfg.SMTPHost != "" {
		ow := service.NewOutboxWorker(outboxRepo, mailer, passService, cfg.AppBaseURL, cfg.ProductName)
		go func() {
			defer close(outboxDone)
			ow.Run(ctx)
		}()
	} else {
		close(outboxDone)
		slog.Warn("SMTP_HOST not set: participant emails stay queued")
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
	<-outboxDone
	return err
}

// passKeyRing loads the pass signing keys, or derives a development key.
func passKeyRing(cfg *config.Config) (*passes.KeyRing, error) {
	if cfg.PassSigningKeys != "" {
		kr, err := passes.ParseKeyRing(cfg.PassSigningKeys)
		if err != nil {
			return nil, fmt.Errorf("PASS_SIGNING_KEYS: %w", err)
		}
		return kr, nil
	}
	slog.Warn("PASS_SIGNING_KEYS not set: pass QR codes use a key derived from JWT_SECRET; set real keys in production (openssl rand -base64 32)")
	return passes.DerivedKeyRing(cfg.JWTSecret), nil
}

// integrationsBox loads INTEGRATIONS_KEY, or derives a development key.
func integrationsBox(cfg *config.Config) (*secrets.Box, error) {
	if cfg.IntegrationsKey != "" {
		return secrets.FromBase64(cfg.IntegrationsKey)
	}
	slog.Warn("INTEGRATIONS_KEY not set: Google tokens are encrypted with a key derived from JWT_SECRET; set a real key in production (openssl rand -base64 32)")
	return secrets.Derived(cfg.JWTSecret), nil
}

// newGoogle returns the Google sign-in client, or nil when it isn't configured.
func newGoogle(cfg *config.Config) *service.GoogleOAuth {
	if cfg.GoogleClientID == "" {
		return nil
	}
	return service.NewGoogleOAuth(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.AppBaseURL+"/auth/google/callback")
}

// newMailer builds the SMTP mailer, or a disabled one when SMTP_HOST is unset
// (email actions remain queued until SMTP is configured).
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
