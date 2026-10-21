package httphandler

// SaaS sign-in sessions, CSRF protection and server-rendered pages
// (milestone M1). The current build's JWT staff login is untouched.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
	"github.com/nasa-spaceapps/nsac-qr/web"
)

const (
	sessionCookie = "sid"
	googleCookie  = "g_oauth"
	csrfHeader    = "X-CSRF-Token"
	csrfField     = "csrf"
	maxPageUpload = passes.MaxUploadBytes + 1<<20 // the image and the other fields

	// pageDeadline covers a page that sends an email (sign-in link, invite).
	pageDeadline = emailDeadline
)

// principal is the signed-in account behind a request.
type principal struct {
	token   string // cookie value, to sign out
	session *domain.Session
	account *domain.Account
}

func (p *principal) scope() authz.AccountScope { return authz.NewAccountScope(p.account.ID) }

// orgReq is a request inside one org the caller belongs to.
type orgReq struct {
	*principal
	sc  authz.OrgScope
	org *domain.OrgMembership
}

// eventReq is a request on one event the caller can open.
type eventReq struct {
	*orgReq
	es    authz.EventScope
	event *domain.Event
}

type (
	authedFunc func(h *Handler, w http.ResponseWriter, r *http.Request, p *principal)
	orgFunc    func(h *Handler, w http.ResponseWriter, r *http.Request, o *orgReq)
	eventFunc  func(h *Handler, w http.ResponseWriter, r *http.Request, e *eventReq)
)

func (h *Handler) secureCookies() bool { return strings.HasPrefix(h.BaseURL, "https://") }

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, Secure: h.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: path, MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

// currentPrincipal returns the signed-in account, or nil.
func (h *Handler) currentPrincipal(r *http.Request) (*principal, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	sess, acct, err := h.SignIn.Authenticate(r.Context(), c.Value)
	if errors.Is(err, domain.ErrUnauthenticated) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &principal{token: c.Value, session: sess, account: acct}, nil
}

func unsafeMethod(m string) bool {
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

// sameOrigin rejects cross-site form posts and fetches. Browsers send Origin
// on every POST and Sec-Fetch-Site on modern versions; non-browser clients
// send neither and are covered by the CSRF token instead.
func (h *Handler) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if h.BaseURL != "" {
		if u, err := url.Parse(h.BaseURL); err == nil && origin == u.Scheme+"://"+u.Host {
			return true
		}
	}
	o, err := url.Parse(origin)
	return err == nil && o.Host == r.Host
}

func csrfOK(p *principal, got string) bool {
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(p.session.CSRFToken)) == 1
}

func withDeadline(r *http.Request, d time.Duration) (*http.Request, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(r.Context(), d)
	return r.WithContext(ctx), cancel
}

// ---- API wrappers ------------------------------------------------------------

// apiAuthed requires a session. Changes also need the session's CSRF token
// in X-CSRF-Token (from GET /api/v1/me).
func (h *Handler) apiAuthed(fn authedFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := h.currentPrincipal(r)
		if err != nil {
			writeDomainError(w, r, err)
			return
		}
		if p == nil {
			writeError(w, http.StatusUnauthorized, domain.ErrUnauthenticated.Error())
			return
		}
		r = r.WithContext(domain.WithClientIP(r.Context(), h.clientIP(r)))
		if unsafeMethod(r.Method) && (!h.sameOrigin(r) || !csrfOK(p, r.Header.Get(csrfHeader))) {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "missing or invalid CSRF token", Code: "CSRF"})
			return
		}
		fn(h, w, r, p)
	})
}

// apiOrg also resolves {org}. A caller outside the org gets the same 404 as
// for an org that doesn't exist.
func (h *Handler) apiOrg(fn orgFunc) http.Handler {
	return h.apiAuthed(func(h *Handler, w http.ResponseWriter, r *http.Request, p *principal) {
		sc, org, err := h.Orgs.Scope(r.Context(), p.scope(), r.PathValue("org"))
		if err != nil {
			writeDomainError(w, r, err)
			return
		}
		fn(h, w, r, &orgReq{principal: p, sc: sc, org: org})
	})
}

// ---- Page wrappers -----------------------------------------------------------

// pageAuthed requires a session, sending visitors to sign in and back.
// Form posts need the session's CSRF token in the "csrf" field.
func (h *Handler) pageAuthed(fn authedFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, cancel := withDeadline(r, pageDeadline)
		defer cancel()
		p, err := h.currentPrincipal(r)
		if err != nil {
			h.renderError(w, r, nil, err)
			return
		}
		if p == nil {
			next := ""
			if r.Method == http.MethodGet {
				next = r.URL.RequestURI()
			}
			http.Redirect(w, r, "/login"+nextQuery(next), http.StatusSeeOther)
			return
		}
		r = r.WithContext(domain.WithClientIP(r.Context(), h.clientIP(r)))
		// File uploads (pass artwork) are read here, for the CSRF field, so
		// the size limit applies here too.
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			r.Body = http.MaxBytesReader(w, r.Body, maxPageUpload)
			var tooBig *http.MaxBytesError
			if err := r.ParseMultipartForm(1 << 20); errors.As(err, &tooBig) {
				h.renderStatus(w, r, p, http.StatusRequestEntityTooLarge, "File too large",
					fmt.Sprintf("Upload an image of at most %d MB.", passes.MaxUploadBytes>>20))
				return
			}
		}
		if unsafeMethod(r.Method) && (!h.sameOrigin(r) || !csrfOK(p, r.PostFormValue(csrfField))) {
			h.renderStatus(w, r, p, http.StatusForbidden, "Page expired",
				"That form was out of date. Go back, reload the page and try again.")
			return
		}
		fn(h, w, r, p)
	})
}

func (h *Handler) pageOrg(fn orgFunc) http.Handler {
	return h.pageAuthed(func(h *Handler, w http.ResponseWriter, r *http.Request, p *principal) {
		sc, org, err := h.Orgs.Scope(r.Context(), p.scope(), r.PathValue("org"))
		if err != nil {
			h.renderError(w, r, p, err)
			return
		}
		fn(h, w, r, &orgReq{principal: p, sc: sc, org: org})
	})
}

// resolveEvent opens {event} for the caller and enforces the route's action.
// No access is 404; access without this action is 403. A trashed event only
// opens for viewing and restoring.
func (h *Handler) resolveEvent(r *http.Request, o *orgReq, action authz.EventAction) (*eventReq, error) {
	es, ev, err := h.Events.Resolve(r.Context(), o.sc, r.PathValue("event"))
	if err != nil {
		return nil, err
	}
	if err := es.Require(action); err != nil {
		return nil, err
	}
	if ev.DeletedAt != nil && action != authz.EventView && action != authz.EventDelete {
		return nil, fmt.Errorf("%w: this event is in Trash; restore it first", domain.ErrConflict)
	}
	return &eventReq{orgReq: o, es: es, event: ev}, nil
}

func (h *Handler) apiEvent(action authz.EventAction, fn eventFunc) http.Handler {
	return h.apiOrg(func(h *Handler, w http.ResponseWriter, r *http.Request, o *orgReq) {
		e, err := h.resolveEvent(r, o, action)
		if err != nil {
			writeDomainError(w, r, err)
			return
		}
		fn(h, w, r, e)
	})
}

func (h *Handler) pageEvent(action authz.EventAction, fn eventFunc) http.Handler {
	return h.pageOrg(func(h *Handler, w http.ResponseWriter, r *http.Request, o *orgReq) {
		e, err := h.resolveEvent(r, o, action)
		if err != nil {
			h.renderError(w, r, o.principal, err)
			return
		}
		fn(h, w, r, e)
	})
}

func nextQuery(next string) string {
	if next == "" || next == "/" {
		return ""
	}
	return "?next=" + url.QueryEscape(next)
}

// ---- Rendering ---------------------------------------------------------------

var pageNames = []string{"login", "login_sent", "verify", "app", "members", "invite", "error",
	"events", "event_new", "event", "event_share", "event_settings", "activity", "link",
	"event_registration", "event_participants", "public_event", "public_registered", "my_passes", "my_pass",
	"scanner", "event_dashboard", "event_checkin", "event_workshops",
	"pass_templates", "designer", "event_pass", "event_integrations", "participant_map", "event_map"}

type pageSet map[string]*template.Template

func loadPages() (pageSet, error) {
	ps := pageSet{}
	for _, name := range pageNames {
		t, err := template.New(name).Funcs(pageFuncs).ParseFS(web.FS,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("page %s: %w", name, err)
		}
		ps[name] = t
	}
	// Help pages: one shared frame, each topic's text in help/<slug>.html.
	for _, slug := range append([]string{"index"}, helpSlugs()...) {
		t, err := template.New("help").Funcs(pageFuncs).ParseFS(web.FS,
			"templates/layout.html", "templates/partials.html", "templates/help.html", "templates/help/"+slug+".html")
		if err != nil {
			return nil, fmt.Errorf("help page %s: %w", slug, err)
		}
		ps["help/"+slug] = t
	}
	return ps, nil
}

// view is what every page template receives.
type view struct {
	Title   string
	Product string
	Account *domain.Account
	CSRF    string
	Error   string
	Notice  string
	Data    any
}

// pageCSP: scripts and styles from this site only (no inline code), fonts
// from Google Fonts, avatars from Google.
const pageCSP = "default-src 'self'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; img-src 'self' data: https://*.googleusercontent.com; " +
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

// scannerCSP lets the camera library style its own preview; scripts stay
// limited to this site.
var scannerCSP = strings.Replace(pageCSP, "style-src 'self'", "style-src 'self' 'unsafe-inline'", 1)

// mapCSP adds OpenStreetMap's map tiles; Leaflet positions tiles and
// markers with inline styles. Scripts stay limited to this site.
var mapCSP = strings.Replace(scannerCSP, "img-src 'self' data:", "img-src 'self' data: https://tile.openstreetmap.org", 1)

func (h *Handler) render(w http.ResponseWriter, status int, page string, p *principal, v view) {
	v.Product = h.Product
	if p != nil {
		v.Account = p.account
		v.CSRF = p.session.CSRFToken
	}
	// Render fully before sending, so a template error is a 500 rather than
	// half a page under a 200.
	var buf bytes.Buffer
	if err := h.pages[page].ExecuteTemplate(&buf, "layout", v); err != nil {
		slog.Error("render page", "page", page, "err", err)
		http.Error(w, "Something went wrong showing this page.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	csp := pageCSP
	switch page {
	case "scanner":
		csp = scannerCSP
	case "participant_map":
		csp = mapCSP
		// The one page that may ask for the phone's location (every other
		// page keeps securityHeaders' geolocation=()).
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(self)")
	}
	w.Header().Set("Content-Security-Policy", csp)
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

type errorPage struct{ Heading, Message string }

func (h *Handler) renderStatus(w http.ResponseWriter, r *http.Request, p *principal, status int, heading, msg string) {
	h.render(w, status, "error", p, view{Title: heading, Data: errorPage{heading, msg}})
}

// renderError shows a service error as a page.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, p *principal, err error) {
	status, msg := pageErrorStatus(err)
	switch status {
	case http.StatusNotFound:
		h.renderStatus(w, r, p, status, "Not found", "This page doesn't exist, or you don't have access to it.")
	case http.StatusInternalServerError:
		slog.Error("page failed", "method", r.Method, "path", r.URL.Path, "err", err)
		h.renderStatus(w, r, p, status, "Something went wrong", "Try again in a moment.")
	default:
		h.renderStatus(w, r, p, status, "That didn't work", msg)
	}
}

// pageErrorStatus maps a service error to a status and a message safe to
// show: the sentinel's own text is dropped from wrapped errors.
func pageErrorStatus(err error) (int, string) {
	for _, m := range []struct {
		sentinel error
		status   int
	}{
		{domain.ErrValidation, http.StatusBadRequest},
		{domain.ErrForbidden, http.StatusForbidden},
		{domain.ErrConflict, http.StatusConflict},
		{domain.ErrRateLimited, http.StatusTooManyRequests},
		{domain.ErrEmailUnavailable, http.StatusServiceUnavailable},
		{domain.ErrNotFound, http.StatusNotFound},
		{domain.ErrUnauthenticated, http.StatusUnauthorized},
	} {
		if errors.Is(err, m.sentinel) {
			msg := strings.TrimPrefix(err.Error(), m.sentinel.Error()+": ")
			if msg == m.sentinel.Error() {
				msg = "You don't have permission to do that."
			}
			return m.status, msg
		}
	}
	return http.StatusInternalServerError, ""
}

// ---- Google sign-in state cookie ---------------------------------------------

// googleState rides in a short-lived cookie between /auth/google/start and the
// callback, signed so it can't be swapped.
type googleState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Next     string `json:"x"`
}

func (h *Handler) signCookie(payload []byte) string {
	mac := hmac.New(sha256.New, h.CookieKey)
	mac.Write([]byte("google-oauth-state:"))
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (h *Handler) readSignedCookie(v string) ([]byte, bool) {
	body, sig, ok := strings.Cut(v, ".")
	if !ok {
		return nil, false
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(body)
	want, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, h.CookieKey)
	mac.Write([]byte("google-oauth-state:"))
	mac.Write(payload)
	return payload, hmac.Equal(mac.Sum(nil), want)
}

func (h *Handler) setGoogleState(w http.ResponseWriter, st googleState) {
	b, _ := json.Marshal(st)
	http.SetCookie(w, &http.Cookie{
		Name: googleCookie, Value: h.signCookie(b), Path: "/auth/google", MaxAge: 600,
		HttpOnly: true, Secure: h.secureCookies(), SameSite: http.SameSiteLaxMode,
	})
}

func (h *Handler) googleState(r *http.Request) (*googleState, bool) {
	c, err := r.Cookie(googleCookie)
	if err != nil {
		return nil, false
	}
	payload, ok := h.readSignedCookie(c.Value)
	if !ok {
		return nil, false
	}
	var st googleState
	if json.Unmarshal(payload, &st) != nil || st.State == "" {
		return nil, false
	}
	return &st, true
}
