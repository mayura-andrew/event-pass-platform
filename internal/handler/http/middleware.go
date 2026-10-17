package httphandler

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/auth"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ctxKey int

const claimsKey ctxKey = iota

func claimsFrom(ctx context.Context) *auth.Claims {
	c, _ := ctx.Value(claimsKey).(*auth.Claims)
	return c
}

// AccountLookup returns an account's live state (role, active, temporary
// password) so changes apply before the JWT expires.
type AccountLookup interface {
	AccountState(ctx context.Context, id string) (*domain.AccountState, error)
}

// accountCacheTTL bounds how long a deactivation, role change or password
// reset can take to reach an already signed-in device.
const accountCacheTTL = 30 * time.Second

type accountCache struct {
	lookup AccountLookup
	mu     sync.Mutex
	items  map[string]cachedAccount
}

type cachedAccount struct {
	state   domain.AccountState
	fetched time.Time
}

func newAccountCache(l AccountLookup) *accountCache {
	return &accountCache{lookup: l, items: make(map[string]cachedAccount)}
}

func (c *accountCache) get(ctx context.Context, id string) (*domain.AccountState, error) {
	c.mu.Lock()
	id = strings.ToLower(id)
	it, ok := c.items[id]
	c.mu.Unlock()
	if ok && time.Since(it.fetched) < accountCacheTTL {
		return &it.state, nil
	}
	st, err := c.lookup.AccountState(ctx, id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.items) > 10_000 { // bound memory; accounts number in the hundreds
		clear(c.items)
	}
	c.items[id] = cachedAccount{state: *st, fetched: time.Now()}
	c.mu.Unlock()
	return st, nil
}

// forget drops an entry so a change made on this instance applies at once.
func (c *accountCache) forget(id string) {
	c.mu.Lock()
	delete(c.items, strings.ToLower(id))
	c.mu.Unlock()
}

// authenticate verifies the Bearer JWT, then checks the account's live state:
// deactivated or deleted accounts get 401, and the claims' role is replaced
// by the current one. Accounts on a temporary password get 403
// PASSWORD_CHANGE_REQUIRED unless allowPending (the change-password route).
func (h *Handler) authenticate(next http.Handler, allowPending bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || raw == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		claims, err := h.Tokens.Verify(raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}

		st, err := h.accounts.get(r.Context(), claims.Subject)
		switch {
		case errors.Is(err, domain.ErrNotFound):
			writeError(w, http.StatusUnauthorized, "account no longer exists")
			return
		case err != nil:
			writeDomainError(w, r, err)
			return
		case !st.IsActive:
			writeError(w, http.StatusUnauthorized, "account has been deactivated")
			return
		case st.ForcePasswordChange && !allowPending:
			writeDomainError(w, r, domain.ErrPasswordChangeRequired)
			return
		}
		claims.Role = st.Role // a role change applies without re-login

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claimsKey, claims)))
	})
}

// requireRole must run after authenticate.
func requireRole(roles []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := claimsFrom(r.Context())
		if c == nil || !slices.Contains(roles, c.Role) {
			writeError(w, http.StatusForbidden, "insufficient role")
			return
		}
		next.ServeHTTP(w, r)
	})
}

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
		case !strings.HasPrefix(p, "/api/"), p == "/api/v1/admin/scans/stream":
			next.ServeHTTP(w, r) // static files; the live stream is long-lived by design
			return
		case p == "/api/v1/scans/sync":
			d = syncDeadline
		case p == "/api/v1/admin/staff/create", strings.HasSuffix(p, "/resend-credentials"):
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
		// Camera is needed by the scanner; nothing else.
		h.Set("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
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
			h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
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
