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
}
