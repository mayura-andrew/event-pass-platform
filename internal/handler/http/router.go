package httphandler

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/web"
)

// NewRouter wires every route using Go 1.22+ method-aware patterns.
func NewRouter(h *Handler, publicDir, allowedOrigin string) http.Handler {
	mux := http.NewServeMux()
	pages, err := loadPages()
	if err != nil {
		panic(err) // templates are embedded; a parse error is a build bug
	}
	h.pages = pages

	// Per client IP. Event venues often put many phones behind one address,
	// so this limit must cover a crowd signing in at once.
	loginLimit := newRateLimiter(h.LoginRatePerMin, time.Minute, h.clientIP)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

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

	return recoverer(logging(securityHeaders(h.secureCookies(), cors(allowedOrigin, deadlines(mux)))))
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
			http.Redirect(w, r, "/app", http.StatusFound)
			return
		case strings.HasSuffix(r.URL.Path, ".html"),
			strings.HasSuffix(r.URL.Path, ".webmanifest"), strings.HasPrefix(r.URL.Path, "/js/"):
			w.Header().Set("Cache-Control", "no-cache")
		case strings.HasPrefix(r.URL.Path, "/vendor/"):
			// Vendor files carry their version in the file name.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fs.ServeHTTP(w, r)
	})
}
