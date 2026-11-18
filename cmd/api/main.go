// Command api runs the event pass platform API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/app"
	"github.com/mayura-andrew/event-pass-platform/internal/config"
	httphandler "github.com/mayura-andrew/event-pass-platform/internal/handler/http"
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

	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := app.CheckDatabaseLogin(ctx, db, cfg); err != nil {
		return err
	}

	// Repositories
	accountRepo := postgres.NewAccountRepo(db)
	orgRepo := postgres.NewOrgRepo(db)
	eventRepo := postgres.NewEventRepo(db)
	registrationRepo := postgres.NewRegistrationRepo(db)
	outboxRepo := postgres.NewOutboxRepo(db)
	passKeys, err := app.PassKeyRing(cfg)
	if err != nil {
		return err
	}
	passService := service.NewPassService(passKeys)
	passAssets := postgres.NewPassAssetRepo(db)
	passService.Assets = passAssets
	checkinRepo := postgres.NewCheckinRepo(db)
	liveFeed := service.NewEventFeed(checkinRepo)
	liveFeed.Broadcast = checkinRepo.Notify

	mailer, err := app.NewMailer(cfg)
	if err != nil {
		return err
	}
	workers, err := app.NewWorkers(cfg, db, mailer, passService)
	if err != nil {
		return err
	}

	// Services
	h := &httphandler.Handler{
		ClientIPHeader:  cfg.ClientIPHeader,
		LoginRatePerMin: cfg.LoginRatePerMin,
		PassRatePerMin:  cfg.PassRatePerMin,
		RateStore:       postgres.NewRateLimitRepo(db),
		HealthToken:     cfg.HealthToken,
		MapsAPIKey:      cfg.GoogleMapsAPIKey,
		MapsMapID:       cfg.GoogleMapsMapID,

		SignIn: service.NewAccountService(accountRepo, mailer, app.NewGoogle(cfg, "/auth/google/callback"), service.AccountConfig{
			BaseURL: cfg.AppBaseURL, Product: cfg.ProductName, SessionTTL: cfg.SessionTTL, IdleTimeout: cfg.SessionIdle,
		}),
		Orgs:          newOrgService(orgRepo, mailer, cfg),
		Events:        service.NewEventService(eventRepo, accountRepo, orgRepo, mailer, cfg.AppBaseURL, cfg.ProductName),
		Registrations: service.NewRegistrationService(registrationRepo, eventRepo, outboxRepo),
		Passes:        passService,
		Templates:     service.NewPassTemplateService(postgres.NewPassTemplateRepo(db), passAssets, passService),
		Portal:        service.NewPortalService(postgres.NewPortalRepo(db)),
		Checker:       registrationRepo,
		LiveFeed:      liveFeed,
		Integrations:  workers.Integrations, // the connect flow; its sync loop runs with the workers
		BaseURL:       cfg.AppBaseURL,
		Product:       cfg.ProductName,
		CookieKey:     cfg.JWTSecret,
	}

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
	// Changes made on other replicas arrive by NOTIFY at once; where the
	// connection can't LISTEN (a transaction pooler), the poll still covers.
	go postgres.Listen(feedCtx, cfg.DatabaseURL, postgres.FeedChannel, liveFeed.Notify)
	srv.RegisterOnShutdown(stopFeed)

	// Background jobs (emails, Google sync, retention). Production runs them
	// in cmd/worker with RUN_WORKERS=false here, so an API deploy doesn't
	// interrupt a send and the jobs use their own, narrower database login.
	workersDone := make(chan struct{})
	if cfg.RunWorkers {
		workersDone = make(chan struct{})
		done := workers.Run(ctx)
		go func() { <-done; close(workersDone) }()
	} else {
		close(workersDone)
		slog.Info("RUN_WORKERS=false: emails, Google sync and retention run in cmd/worker")
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
	<-workersDone
	return err
}

// newOrgService lets only PLATFORM_ADMIN_EMAILS create organizations.
// Everyone else (pass holders) can sign in for their passes or join an org
// by invite, but can't open an org of their own.
func newOrgService(store service.OrgStore, mailer service.Mailer, cfg *config.Config) *service.OrgService {
	s := service.NewOrgService(store, mailer, cfg.AppBaseURL, cfg.ProductName)
	s.OrgCreators = service.OrgCreatorsByEmail(cfg.PlatformAdminEmails)
	if len(cfg.PlatformAdminEmails) == 0 {
		slog.Warn("PLATFORM_ADMIN_EMAILS not set: nobody can create organizations from the app")
	} else {
		slog.Info("organization creation limited to platform admins", "count", len(cfg.PlatformAdminEmails))
	}
	return s
}
