package httphandler

import "github.com/mayura-andrew/event-pass-platform/internal/service"

// Handler groups every endpoint's dependencies.
type Handler struct {
	// Rate limiting (see clientIP).
	ClientIPHeader  string
	LoginRatePerMin int
	PassRatePerMin  int

	// Values exposed to browsers through the public configuration endpoint.
	MapsAPIKey string
	MapsMapID  string

	// SaaS (milestone M1): accounts, sessions and organizations.
	SignIn *service.AccountService
	Orgs   *service.OrgService
	Events *service.EventService
	// Registrations: publishing, forms, public registration (M3).
	Registrations *service.RegistrationService
	Passes        *service.PassService
	Portal        *service.PortalService
	Checker       service.ParticipantChecker // pass checks (the registration repo)
	// Check-in, workshops and live updates (M4).
	Checkins                       *service.CheckinService
	Templates                      *service.PassTemplateService // pass templates and the designer (M5)
	Integrations                   *service.IntegrationService  // Google Sheets sync and Form import (M5)
	StatusPageURL                  string                       // public status page, linked from help (M6)
	Health                         *service.Health              // deep status check for uptime monitors (M6)
	Profiles                       *service.ProfileService      // participant profiles and the map beta (M6)
	MapTileURL, MapTileAttribution string                       // participant map tiles; empty: OpenStreetMap
	LiveFeed                       *service.EventFeed
	BaseURL                        string // APP_BASE_URL; https enables Secure cookies
	Product                        string // platform name on pages
	CookieKey                      []byte // signs the Google sign-in state cookie

	pages         pageSet      // set by NewRouter
	signInLimit   *rateLimiter // set by NewRouter
	registerLimit *rateLimiter // public registration, per IP
}
