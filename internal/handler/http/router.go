package httphandler

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/web"
)

// NewRouter wires every route using Go 1.22+ method-aware patterns.
func NewRouter(h *Handler, publicDir, allowedOrigin string) http.Handler {
	mux := http.NewServeMux()
	h.accounts = newAccountCache(h.Accounts)
	pages, err := loadPages()
	if err != nil {
		panic(err) // templates are embedded; a parse error is a build bug
	}
	h.pages = pages

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
	mux.Handle("PUT /api/v1/participants/pass/food-preference", passLimit.wrap(http.HandlerFunc(h.updateFoodPreference)))
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
	mux.Handle("GET /api/v1/fields/scanner", authed(h.scannerFields))

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

	// Adding delegates (membership IDs and QR signatures are generated)
	mux.Handle("POST /api/v1/admin/participants/create", manager(h.createParticipant))
	mux.Handle("POST /api/v1/admin/participants/upload-csv", manager(h.uploadParticipantsCSV))

	// Pass email queue
	mux.Handle("POST /api/v1/admin/emails/send-single", manager(h.sendSingleEmail))
	mux.Handle("POST /api/v1/admin/emails/send-bulk", manager(h.sendBulkEmail))
	mux.Handle("GET /api/v1/admin/emails/status", manager(h.emailStatus))
	mux.Handle("POST /api/v1/admin/emails/pause-toggle", manager(h.toggleEmailPause))

	// Custom delegate fields: deleting one erases its values, so superadmin only
	mux.Handle("GET /api/v1/admin/fields", manager(h.listFields))
	mux.Handle("POST /api/v1/admin/fields", manager(h.createField))
	mux.Handle("PATCH /api/v1/admin/fields/{key}", manager(h.updateField))
	mux.Handle("DELETE /api/v1/admin/fields/{key}", superadmin(h.deleteField))

	// Staff accounts: listing for managers, changes for superadmins only
	mux.Handle("GET /api/v1/admin/staff/list", manager(h.listStaff))
	mux.Handle("POST /api/v1/admin/staff/create", superadmin(h.createStaff))
	mux.Handle("PATCH /api/v1/admin/staff/{id}", superadmin(h.updateStaff))
	mux.Handle("POST /api/v1/admin/staff/{id}/resend-credentials", superadmin(h.resendCredentials))

	// SaaS: sign-in, organizations, invites (milestone M1). Sign-in links
	// share the login limiter: both are per-IP sign-in attempts.
	if h.SignIn != nil {
		h.registerSaaS(mux, loginLimit)
		static, _ := fs.Sub(web.FS, "static")
		mux.Handle("GET /static/", noCache(http.StripPrefix("/static/", http.FileServerFS(static))))
	}

	// Unknown API paths return JSON, not the static file server's HTML 404.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "no such endpoint")
	})

	// Static PWA and portal
	mux.Handle("/", staticFiles(publicDir))

	return recoverer(logging(securityHeaders(cors(allowedOrigin, deadlines(mux)))))
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
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
