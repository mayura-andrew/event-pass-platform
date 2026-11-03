package httphandler

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// ---- Rate limiting -----------------------------------------------------------

// rateLimiter is a small fixed-window limiter keyed by client IP. It protects
// the login and public pass lookup endpoints from brute force. For multiple
// API replicas, move this to a shared store or the edge (e.g. Cloudflare).
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string]*window
	key    func(*http.Request) string // client identity, usually its IP
}

type window struct {
	start time.Time
	count int
}

func newRateLimiter(limit int, per time.Duration, key func(*http.Request) string) *rateLimiter {
	rl := &rateLimiter{limit: limit, window: per, hits: make(map[string]*window), key: key}
	go func() { // evict stale entries so the map can't grow without bound
		for range time.Tick(per) {
			rl.mu.Lock()
			for k, w := range rl.hits {
				if time.Since(w.start) > rl.window {
					delete(rl.hits, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	w, ok := rl.hits[key]
	if !ok || time.Since(w.start) > rl.window {
		rl.hits[key] = &window{start: time.Now(), count: 1}
		return true
	}
	w.count++
	return w.count <= rl.limit
}

func (rl *rateLimiter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.allow(rl.key(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "too many attempts, try again in a minute")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP returns the caller's address for rate limiting.
//
// Behind a load balancer every request arrives from the balancer's address, so
// all users would share one limit. Set CLIENT_IP_HEADER to the header your
// platform overwrites with the real client IP (DigitalOcean App Platform:
// DO-Connecting-IP). Only use a header the proxy sets itself: trusting a
// client-supplied one such as X-Forwarded-For lets anyone dodge the limiter.
func (h *Handler) clientIP(r *http.Request) string {
	if h.ClientIPHeader != "" {
		if v := strings.TrimSpace(r.Header.Get(h.ClientIPHeader)); v != "" {
			first, _, _ := strings.Cut(v, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---- Request deadlines -------------------------------------------------------

// Per-request deadlines bound how long a request may hold a database
// connection. Without them a stuck query or a client that never finishes
// keeps its pool connection until the TCP connection dies, and a handful of
// those exhaust a small Supabase pool. The context is cancelled at the
// deadline, which makes pgx cancel the running query on the server.
const (
	apiDeadline    = 15 * time.Second
	syncDeadline   = 30 * time.Second // up to 500 scans
	emailDeadline  = 45 * time.Second // SMTP send inside the request
	importDeadline = 55 * time.Second // CSV upload: up to 5 MB arriving, then one batch insert
)

func deadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var d time.Duration
		switch {
		case !strings.HasPrefix(p, "/api/"), p == "/api/v1/admin/scans/stream",
			strings.HasPrefix(p, "/api/v1/orgs/") && strings.HasSuffix(p, "/stream"):
			next.ServeHTTP(w, r) // static files; the live stream is long-lived by design
			return
		case p == "/api/v1/scans/sync":
			d = syncDeadline
		case p == "/api/v1/admin/staff/create", strings.HasSuffix(p, "/resend-credentials"),
			p == "/api/v1/auth/email-link", strings.HasPrefix(p, "/api/v1/orgs/") && strings.HasSuffix(p, "/invites"):
			d = emailDeadline
		case p == "/api/v1/admin/participants/upload-csv":
			d = importDeadline
		default:
			d = apiDeadline
		}
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---- Cross-cutting -------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the real writer, which the scan
// stream needs to flush and to lift the server's write deadline.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			slog.Info("http", "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "dur_ms", time.Since(start).Milliseconds())
		}
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "value", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(https bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		// Camera is needed by the scanner; nothing else.
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		if https {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// cors is only needed when the frontend is served from a different origin.
func cors(origin string, next http.Handler) http.Handler {
	if origin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == origin {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-None-Match")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Expose-Headers", "ETag")
			h.Set("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
