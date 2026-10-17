package httphandler

import (
	"net/http"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// NewRouter wires every route using Go 1.22+ method-aware patterns.
func NewRouter(h *Handler, publicDir, allowedOrigin string) http.Handler {
	mux := http.NewServeMux()
	h.accounts = newAccountCache(h.Accounts)

	authed := func(f http.HandlerFunc) http.Handler { return h.authenticate(f, false) }
	managers := []string{domain.RoleSuperAdmin, domain.RoleAdmin}
	manager := func(f http.HandlerFunc) http.Handler {
		return h.authenticate(requireRole(managers, f), false)
	}
	superadmin := func(f http.HandlerFunc) http.Handler {
		return h.authenticate(requireRole([]string{domain.RoleSuperAdmin}, f), false)
	}

	// Per client IP. Venue Wi-Fi puts every phone behind one public address,
	// so these must cover a crowd signing in at once; bcrypt keeps guessing
	// slow regardless.
	loginLimit := newRateLimiter(h.LoginRatePerMin, time.Minute, h.clientIP)
	passLimit := newRateLimiter(h.PassRatePerMin, time.Minute, h.clientIP)

	// Public
	mux.Handle("POST /api/v1/auth/login", loginLimit.wrap(http.HandlerFunc(h.login)))
	// Signed in, possibly with a temporary password. Shares the login limiter:
	// it also checks a password.
	mux.Handle("POST /api/v1/auth/change-password", loginLimit.wrap(h.authenticate(http.HandlerFunc(h.changePassword), true)))
	mux.Handle("GET /api/v1/participants/pass", passLimit.wrap(http.HandlerFunc(h.pass)))
	mux.HandleFunc("GET /api/v1/config/public", h.publicConfig)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Any signed-in account (scanner devices)
	mux.Handle("GET /api/v1/participants/manifest", authed(h.manifest))
	mux.Handle("GET /api/v1/scenarios/active", authed(h.activeScenarios))
	mux.Handle("POST /api/v1/scans/sync", authed(h.syncScans))
	mux.Handle("GET /api/v1/scans/claims", authed(h.claims))
	mux.Handle("POST /api/v1/devices/heartbeat", authed(h.deviceHeartbeat))

	// Scenario management: admin and superadmin only
	mux.Handle("GET /api/v1/scenarios", manager(h.allScenarios))
	mux.Handle("POST /api/v1/scenarios", manager(h.createScenario))
	mux.Handle("PATCH /api/v1/scenarios/{id}", manager(h.updateScenario))

	// Admin dashboard: admin and superadmin only
	mux.Handle("GET /api/v1/admin/dashboard/stats", manager(h.dashboardStats))
	mux.Handle("POST /api/v1/admin/participants/filter", manager(h.filterParticipants))
	mux.Handle("POST /api/v1/admin/scenarios/toggle", manager(h.toggleScenario))
	mux.Handle("POST /api/v1/admin/scans/manual-override", manager(h.manualOverride))
	mux.Handle("GET /api/v1/admin/devices/health", manager(h.deviceHealth))
	mux.Handle("GET /api/v1/admin/venues", manager(h.listVenues))
	mux.Handle("GET /api/v1/admin/scans/recent", manager(h.recentScans))
	mux.Handle("GET /api/v1/admin/scans/stream", manager(h.scanStream))

	// Staff accounts: listing for managers, changes for superadmins only
	mux.Handle("GET /api/v1/admin/staff/list", manager(h.listStaff))
	mux.Handle("POST /api/v1/admin/staff/create", superadmin(h.createStaff))
	mux.Handle("PATCH /api/v1/admin/staff/{id}", superadmin(h.updateStaff))
	mux.Handle("POST /api/v1/admin/staff/{id}/resend-credentials", superadmin(h.resendCredentials))

	// Unknown API paths return JSON, not the static file server's HTML 404.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})

	// Static PWA and portal
	mux.Handle("/", staticFiles(publicDir))

	return recoverer(logging(securityHeaders(cors(allowedOrigin, deadlines(mux)))))
}

// staticFiles serves /public. The service worker and app shell must never be
// cached by the HTTP layer, or devices would keep running an old scanner.
func staticFiles(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.Redirect(w, r, "/pass.html", http.StatusFound)
			return
		case r.URL.Path == "/admin" || r.URL.Path == "/admin/":
			// Without this the file server would list the directory.
			http.Redirect(w, r, "/admin/dashboard.html", http.StatusFound)
			return
		case r.URL.Path == "/sw.js", strings.HasSuffix(r.URL.Path, ".html"),
			strings.HasSuffix(r.URL.Path, ".webmanifest"), strings.HasPrefix(r.URL.Path, "/js/"),
			strings.HasPrefix(r.URL.Path, "/admin/"):
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasPrefix(r.URL.Path, "/vendor/"):
			// Vendor files carry their version in the file name.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fs.ServeHTTP(w, r)
	})
}
