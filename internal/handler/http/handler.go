package httphandler

import (
	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

// Handler groups every endpoint's dependencies.
type Handler struct {
	Auth         *service.AuthService
	Sync         *service.SyncService
	Participants *service.ParticipantService
	Scenarios    *service.ScenarioService
	Dashboard    *service.DashboardService
	Feed         *service.ScanFeed
	Staff        *service.StaffService
	Ingest       *service.IngestService // nil when HMAC_SECRET_KEY is unset
	Fields       *service.FieldService
	Emails       *service.EmailQueueService
	Tokens       *auth.TokenManager
	Accounts     AccountLookup // live role/status checks on every request

	// Rate limiting (see clientIP).
	ClientIPHeader  string
	LoginRatePerMin int
	PassRatePerMin  int

	accounts  *accountCache // set by NewRouter
	manifests manifestCache

	// Values exposed to browsers through GET /api/v1/config/public.
	MapsAPIKey string
	MapsMapID  string

	// SaaS (milestone M1): accounts, sessions and organizations.
	SignIn    *service.AccountService
	Orgs      *service.OrgService
	Events    *service.EventService
	BaseURL   string // APP_BASE_URL; https enables Secure cookies
	Product   string // platform name on pages
	CookieKey []byte // signs the Google sign-in state cookie

	pages       pageSet      // set by NewRouter
	signInLimit *rateLimiter // set by NewRouter
}
