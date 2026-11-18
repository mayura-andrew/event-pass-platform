// Package app holds the start-up code cmd/api and cmd/worker share: loading
// keys, building the mailer, checking the database login, and running the
// background jobs.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/config"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/secrets"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

// PassKeyRing loads the pass signing keys. Production requires real keys
// (config refuses to start without them); development derives one from
// JWT_SECRET.
func PassKeyRing(cfg *config.Config) (*passes.KeyRing, error) {
	if cfg.PassSigningKeys != "" {
		kr, err := passes.ParseKeyRing(cfg.PassSigningKeys)
		if err != nil {
			return nil, fmt.Errorf("PASS_SIGNING_KEYS: %w", err)
		}
		if cfg.Production() && kr.SameAs(passes.DerivedKeyRing(cfg.JWTSecret)) {
			return nil, errors.New("PASS_SIGNING_KEYS: a key equals the one derived from JWT_SECRET; generate an independent key (openssl rand -base64 32)")
		}
		return kr, nil
	}
	if cfg.Production() {
		return nil, errors.New("PASS_SIGNING_KEYS is required in production")
	}
	slog.Warn("PASS_SIGNING_KEYS not set: pass QR codes use a key derived from JWT_SECRET; set real keys in production (openssl rand -base64 32)")
	return passes.DerivedKeyRing(cfg.JWTSecret), nil
}

// IntegrationsBox loads INTEGRATIONS_KEY, or derives a development key.
func IntegrationsBox(cfg *config.Config) (*secrets.Box, error) {
	if cfg.IntegrationsKey != "" {
		box, err := secrets.FromBase64(cfg.IntegrationsKey)
		if err != nil {
			return nil, err
		}
		if cfg.Production() && box.SameKey(secrets.Derived(cfg.JWTSecret)) {
			return nil, errors.New("INTEGRATIONS_KEY equals the key derived from JWT_SECRET; generate an independent key (openssl rand -base64 32)")
		}
		return box, nil
	}
	if cfg.Production() {
		return nil, errors.New("INTEGRATIONS_KEY is required in production")
	}
	slog.Warn("INTEGRATIONS_KEY not set: Google tokens are encrypted with a key derived from JWT_SECRET; set a real key in production (openssl rand -base64 32)")
	return secrets.Derived(cfg.JWTSecret), nil
}

// CheckDatabaseLogin refuses, in production, a database login that row level
// security does not apply to: a superuser, a BYPASSRLS role, or the owner of
// the application tables. Such a login turns any query that forgets its
// tenant scope into a cross-tenant read. In development it only warns.
func CheckDatabaseLogin(ctx context.Context, db *sql.DB, cfg *config.Config) error {
	var user string
	var super, bypass bool
	var owned int
	err := db.QueryRowContext(ctx, `
		SELECT current_user, r.rolsuper, r.rolbypassrls,
		       (SELECT count(*) FROM pg_tables t
		         WHERE t.schemaname = 'public' AND t.tableowner = current_user)
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&user, &super, &bypass, &owned)
	if err != nil {
		return fmt.Errorf("check database login: %w", err)
	}
	if !super && !bypass && owned == 0 {
		slog.Info("database login is not privileged", "user", user)
		return nil
	}
	msg := fmt.Sprintf("database login %q bypasses row level security (superuser=%t bypassrls=%t owns %d tables); "+
		"connect as a member of event_pass_api (or event_pass_worker) and run migrations as the owner "+
		"(docs/decisions/0010-database-roles.md)", user, super, bypass, owned)
	if cfg.Production() {
		return errors.New(msg)
	}
	slog.Warn(msg)
	return nil
}

// NewGoogle returns the Google OAuth client for redirect, or nil when it
// isn't configured.
func NewGoogle(cfg *config.Config, redirect string) *service.GoogleOAuth {
	if cfg.GoogleClientID == "" {
		return nil
	}
	return service.NewGoogleOAuth(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.AppBaseURL+redirect)
}

// NewMailer builds the SMTP mailer, or a disabled one when SMTP_HOST is unset
// (email actions remain queued until SMTP is configured).
func NewMailer(cfg *config.Config) (service.Mailer, error) {
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

// Workers are the background jobs: the participant email outbox, Google
// Sheets / Forms sync, data retention and the rate limit sweep. cmd/worker
// runs them in their own process; cmd/api runs them too unless
// RUN_WORKERS=false.
type Workers struct {
	Outbox       *service.OutboxWorker // nil without SMTP: emails stay queued
	Integrations *service.IntegrationService
	Retention    *service.Retention
	RateLimits   *postgres.RateLimitRepo
}

// NewWorkers builds the jobs on db.
func NewWorkers(cfg *config.Config, db *sql.DB, mailer service.Mailer, passService *service.PassService) (*Workers, error) {
	box, err := IntegrationsBox(cfg)
	if err != nil {
		return nil, err
	}
	registrationRepo := postgres.NewRegistrationRepo(db)
	w := &Workers{
		Integrations: service.NewIntegrationService(postgres.NewIntegrationRepo(db), registrationRepo,
			NewGoogle(cfg, "/auth/google/callback"), service.NewGoogleAPI(), box, cfg.AppBaseURL+"/auth/google/integration/callback"),
		Retention:  service.NewRetention(postgres.NewRetentionRepo(db), cfg.BirthDateRetention),
		RateLimits: postgres.NewRateLimitRepo(db),
	}
	if cfg.SMTPHost != "" {
		w.Outbox = service.NewOutboxWorker(postgres.NewOutboxRepo(db), mailer, passService, cfg.AppBaseURL, cfg.ProductName)
	} else {
		slog.Warn("SMTP_HOST not set: participant emails stay queued")
	}
	return w, nil
}

// Run starts every job and returns a channel closed once the outbox worker
// has stopped (it may be mid-send when ctx ends).
func (w *Workers) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go w.Integrations.Run(ctx)
	go w.Retention.Run(ctx)
	go sweepRateLimits(ctx, w.RateLimits)
	if w.Outbox == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		w.Outbox.Run(ctx)
	}()
	return done
}

// sweepRateLimits drops finished rate limit windows every ten minutes.
func sweepRateLimits(ctx context.Context, repo *postgres.RateLimitRepo) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := repo.Sweep(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("rate limit sweep failed", "err", err)
			}
		}
	}
}
