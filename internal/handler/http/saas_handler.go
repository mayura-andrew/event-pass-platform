package httphandler

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

// orgRoute is one endpoint inside /o/{org} or /api/v1/orgs/{org}. Every one
// is listed here so the tenant isolation test can call each as an outsider.
type orgRoute struct {
	method, pattern string
	api             bool
	fn              orgFunc
}

var orgRoutes = []orgRoute{
	{"GET", "/api/v1/orgs/{org}", true, (*Handler).apiGetOrg},
	{"GET", "/api/v1/orgs/{org}/members", true, (*Handler).apiListMembers},
	{"PATCH", "/api/v1/orgs/{org}/members/{account}", true, (*Handler).apiSetRole},
	{"DELETE", "/api/v1/orgs/{org}/members/{account}", true, (*Handler).apiRemoveMember},
	{"GET", "/api/v1/orgs/{org}/invites", true, (*Handler).apiListInvites},
	{"POST", "/api/v1/orgs/{org}/invites", true, (*Handler).apiCreateInvite},
	{"DELETE", "/api/v1/orgs/{org}/invites/{id}", true, (*Handler).apiRevokeInvite},
	{"PATCH", "/api/v1/orgs/{org}/settings", true, (*Handler).apiOrgSettings},
	{"GET", "/api/v1/orgs/{org}/activity", true, (*Handler).apiOrgActivity},
	{"GET", "/api/v1/orgs/{org}/events", true, (*Handler).apiListEvents},
	{"POST", "/api/v1/orgs/{org}/events", true, (*Handler).apiCreateEvent},

	{"GET", "/o/{org}", false, (*Handler).eventsHomePage},
	{"GET", "/o/{org}/events/new", false, (*Handler).newEventPage},
	{"POST", "/o/{org}/events", false, (*Handler).createEventPost},
	{"GET", "/o/{org}/members", false, (*Handler).membersPage},
	{"POST", "/o/{org}/settings", false, (*Handler).orgSettingsPost},
	{"GET", "/o/{org}/activity", false, (*Handler).orgActivityPage},
	{"POST", "/o/{org}/invites", false, (*Handler).orgInvitePost},
	{"POST", "/o/{org}/invites/{id}/revoke", false, (*Handler).orgRevokePost},
	{"POST", "/o/{org}/members/{account}/role", false, (*Handler).orgRolePost},
	{"POST", "/o/{org}/members/{account}/remove", false, (*Handler).orgRemovePost},
}

// registerSaaS adds the M1 routes: sign-in, organizations, invites.
func (h *Handler) registerSaaS(mux *http.ServeMux, signInLimit *rateLimiter) {
	h.signInLimit = signInLimit
	authed := h.apiAuthed

	// JSON API
	mux.Handle("POST /api/v1/auth/email-link", signInLimit.wrap(http.HandlerFunc(h.apiEmailLink)))
	mux.Handle("POST /api/v1/auth/logout", authed((*Handler).apiLogout))
	mux.Handle("GET /api/v1/me", authed((*Handler).apiMe))
	mux.Handle("GET /api/v1/orgs", authed((*Handler).apiListOrgs))
	mux.Handle("POST /api/v1/orgs", authed((*Handler).apiCreateOrg))
	mux.Handle("POST /api/v1/invites/accept", authed((*Handler).apiAcceptInvite))

	// Pages
	mux.HandleFunc("GET /login", h.loginPage)
	mux.HandleFunc("POST /login", h.loginPost)
	mux.HandleFunc("GET /auth/email/verify", h.verifyPage)
	mux.HandleFunc("POST /auth/email/verify", h.verifyPost)
	mux.HandleFunc("GET /auth/google/start", h.googleStart)
	mux.HandleFunc("GET /auth/google/callback", h.googleCallback)
	mux.HandleFunc("GET /auth/google/integration/callback", h.integrationCallback)
	mux.HandleFunc("POST /logout", h.logoutPost)
	mux.Handle("GET /app", h.pageAuthed((*Handler).appPage))
	mux.Handle("POST /app/orgs", h.pageAuthed((*Handler).createOrgPost))
	mux.Handle("GET /invite", h.pageAuthed((*Handler).invitePage))
	mux.Handle("POST /invite/accept", h.pageAuthed((*Handler).acceptInvitePost))

	mux.HandleFunc("GET /l/{token}", h.linkPage)
	// Public event pages and registration. Venue Wi-Fi puts many phones
	// behind one address, so the limit is generous.
	h.registerLimit = newRateLimiter(max(h.PassRatePerMin, 60), time.Minute, h.clientIP)
	h.registerPortal(mux)
	mux.HandleFunc("GET /e/{org}/{event}", h.publicEventPage)
	mux.HandleFunc("POST /e/{org}/{event}/register", h.registerPost)

	for _, rt := range orgRoutes {
		if rt.api {
			mux.Handle(rt.method+" "+rt.pattern, h.apiOrg(rt.fn))
		} else {
			mux.Handle(rt.method+" "+rt.pattern, h.pageOrg(rt.fn))
		}
	}
	for _, rt := range eventRoutes {
		if rt.api {
			mux.Handle(rt.method+" "+rt.pattern, h.apiEvent(rt.action, rt.fn))
		} else {
			mux.Handle(rt.method+" "+rt.pattern, h.pageEvent(rt.action, rt.fn))
		}
	}
}

// =============================================================================
// JSON API
// =============================================================================

// POST /api/v1/auth/email-link {email, next}
func (h *Handler) apiEmailLink(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	var req struct {
		Email string `json:"email"`
		Next  string `json:"next"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if err := h.SignIn.RequestEmailLink(r.Context(), req.Email, req.Next); err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"sent": true})
}

func (h *Handler) apiLogout(w http.ResponseWriter, r *http.Request, p *principal) {
	if err := h.SignIn.SignOut(r.Context(), p.token); err != nil {
		writeDomainError(w, r, err)
		return
	}
	h.clearCookie(w, sessionCookie, "/")
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/me: the account, its orgs, and the CSRF token for changes.
func (h *Handler) apiMe(w http.ResponseWriter, r *http.Request, p *principal) {
	orgs, err := h.Orgs.ListOrgs(r.Context(), p.scope())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"account": p.account, "orgs": orgs, "csrf_token": p.session.CSRFToken,
		"session_expires_at": p.session.ExpiresAt,
	})
}

func (h *Handler) apiListOrgs(w http.ResponseWriter, r *http.Request, p *principal) {
	orgs, err := h.Orgs.ListOrgs(r.Context(), p.scope())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, orgs)
}

func (h *Handler) apiCreateOrg(w http.ResponseWriter, r *http.Request, p *principal) {
	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	m, err := h.Orgs.CreateOrg(r.Context(), p.scope(), req.Name, req.Slug)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) apiAcceptInvite(w http.ResponseWriter, r *http.Request, p *principal) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	m, _, err := h.Orgs.AcceptInvite(r.Context(), p.scope(), p.account, req.Token)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) apiGetOrg(w http.ResponseWriter, r *http.Request, o *orgReq) {
	writeJSON(w, http.StatusOK, o.org)
}

func (h *Handler) apiListMembers(w http.ResponseWriter, r *http.Request, o *orgReq) {
	ms, err := h.Orgs.ListMembers(r.Context(), o.sc)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ms)
}

func (h *Handler) apiSetRole(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if err := h.Orgs.SetRole(r.Context(), o.sc, r.PathValue("account"), req.Role); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiRemoveMember(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if err := h.Orgs.RemoveMember(r.Context(), o.sc, r.PathValue("account")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiListInvites(w http.ResponseWriter, r *http.Request, o *orgReq) {
	invs, err := h.Orgs.ListInvites(r.Context(), o.sc)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, invs)
}

func (h *Handler) apiCreateInvite(w http.ResponseWriter, r *http.Request, o *orgReq) {
	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	inv, err := h.Orgs.Invite(r.Context(), o.sc, o.org, o.account, req.Email, req.Role)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, inv)
}

func (h *Handler) apiRevokeInvite(w http.ResponseWriter, r *http.Request, o *orgReq) {
	if err := h.Orgs.RevokeInvite(r.Context(), o.sc, r.PathValue("id")); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// =============================================================================
// Pages: sign-in
// =============================================================================

type loginData struct {
	Google bool
	Next   string
	Email  string
}

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	next := service.SafeNext(r.URL.Query().Get("next"))
	if p, _ := h.currentPrincipal(r); p != nil {
		http.Redirect(w, r, orDefault(next, "/app"), http.StatusSeeOther)
		return
	}
	h.render(w, http.StatusOK, "login", nil, view{Title: "Sign in",
		Data: loginData{Google: h.SignIn.GoogleEnabled(), Next: next}})
}

func (h *Handler) loginPost(w http.ResponseWriter, r *http.Request) {
	r, cancel := withDeadline(r, pageDeadline)
	defer cancel()
	data := loginData{Google: h.SignIn.GoogleEnabled(), Next: service.SafeNext(r.PostFormValue("next")),
		Email: r.PostFormValue("email")}
	fail := func(status int, msg string) {
		h.render(w, status, "login", nil, view{Title: "Sign in", Error: msg, Data: data})
	}
	if !h.sameOrigin(r) {
		fail(http.StatusForbidden, "That form came from another site. Open the sign-in page and try again.")
		return
	}
	if !h.signInLimit.allow(h.clientIP(r)) {
		fail(http.StatusTooManyRequests, "Too many attempts. Wait a minute, then try again.")
		return
	}
	if err := h.SignIn.RequestEmailLink(r.Context(), data.Email, data.Next); err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError {
			h.renderError(w, r, nil, err)
			return
		}
		fail(status, msg)
		return
	}
	h.render(w, http.StatusOK, "login_sent", nil, view{Title: "Check your email", Data: data})
}

func (h *Handler) verifyPage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		h.renderStatus(w, r, nil, http.StatusBadRequest, "Link incomplete", "Open the link from your email again, or request a new one.")
		return
	}
	h.render(w, http.StatusOK, "verify", nil, view{Title: "Finish signing in", Data: map[string]string{"Token": token}})
}

func (h *Handler) verifyPost(w http.ResponseWriter, r *http.Request) {
	if !h.sameOrigin(r) {
		h.renderStatus(w, r, nil, http.StatusForbidden, "Sign-in refused", "That form came from another site.")
		return
	}
	ns, err := h.SignIn.VerifyEmailLink(r.Context(), r.PostFormValue("token"), r.UserAgent(), h.clientIP(r))
	if err != nil {
		h.renderError(w, r, nil, err)
		return
	}
	h.finishSignIn(w, r, ns)
}

// finishSignIn replaces any earlier session on this browser with the new one.
func (h *Handler) finishSignIn(w http.ResponseWriter, r *http.Request, ns *service.NewSession) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = h.SignIn.SignOut(r.Context(), c.Value)
	}
	h.setSessionCookie(w, ns.Token, ns.Session.ExpiresAt)
	http.Redirect(w, r, orDefault(ns.Next, "/app"), http.StatusSeeOther)
}

func (h *Handler) googleStart(w http.ResponseWriter, r *http.Request) {
	g := h.SignIn.Google()
	if g == nil {
		h.renderStatus(w, r, nil, http.StatusNotFound, "Not available", "Google sign-in isn't set up on this server. Use an email link instead.")
		return
	}
	st := googleState{
		State: service.NewSecret(), Nonce: service.NewSecret(), Verifier: service.NewSecret(),
		Next: service.SafeNext(r.URL.Query().Get("next")),
	}
	h.setGoogleState(w, st)
	http.Redirect(w, r, g.AuthCodeURL(st.State, st.Nonce, st.Verifier), http.StatusFound)
}

func (h *Handler) googleCallback(w http.ResponseWriter, r *http.Request) {
	r, cancel := withDeadline(r, apiDeadline)
	defer cancel()
	st, ok := h.googleState(r)
	h.clearCookie(w, googleCookie, "/auth/google")
	q := r.URL.Query()
	if !ok || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		h.renderStatus(w, r, nil, http.StatusBadRequest, "Sign-in expired", "Start again from the sign-in page.")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		h.render(w, http.StatusOK, "login", nil, view{Title: "Sign in", Error: "Google sign-in was cancelled.",
			Data: loginData{Google: true, Next: st.Next}})
		return
	}
	ns, err := h.SignIn.SignInWithGoogle(r.Context(), q.Get("code"), st.Verifier, st.Nonce, st.Next, r.UserAgent(), h.clientIP(r))
	if err != nil {
		h.renderError(w, r, nil, err)
		return
	}
	h.finishSignIn(w, r, ns)
}

func (h *Handler) logoutPost(w http.ResponseWriter, r *http.Request) {
	p, err := h.currentPrincipal(r)
	if err != nil {
		h.renderError(w, r, nil, err)
		return
	}
	if p != nil {
		// Signing someone out is a change too: refuse it from other sites.
		if !h.sameOrigin(r) || !csrfOK(p, r.PostFormValue(csrfField)) {
			h.renderStatus(w, r, p, http.StatusForbidden, "Page expired", "Reload the page and try again.")
			return
		}
		if err := h.SignIn.SignOut(r.Context(), p.token); err != nil {
			h.renderError(w, r, p, err)
			return
		}
	}
	h.clearCookie(w, sessionCookie, "/")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// =============================================================================
// Pages: organizations and invites
// =============================================================================

type appData struct {
	Orgs       []domain.OrgMembership
	Name, Slug string
}

func (h *Handler) appPage(w http.ResponseWriter, r *http.Request, p *principal) {
	h.renderApp(w, r, p, http.StatusOK, "", appData{})
}

func (h *Handler) renderApp(w http.ResponseWriter, r *http.Request, p *principal, status int, errMsg string, data appData) {
	orgs, err := h.Orgs.ListOrgs(r.Context(), p.scope())
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	data.Orgs = orgs
	h.render(w, status, "app", p, view{Title: "Your organizations", Error: errMsg, Data: data})
}

func (h *Handler) createOrgPost(w http.ResponseWriter, r *http.Request, p *principal) {
	name, slug := r.PostFormValue("name"), r.PostFormValue("slug")
	m, err := h.Orgs.CreateOrg(r.Context(), p.scope(), name, slug)
	if err != nil {
		status, msg := pageErrorStatus(err)
		if status == http.StatusInternalServerError {
			h.renderError(w, r, p, err)
			return
		}
		h.renderApp(w, r, p, status, msg, appData{Name: name, Slug: slug})
		return
	}
	http.Redirect(w, r, "/o/"+m.Slug+"?notice=created", http.StatusSeeOther)
}

type inviteData struct {
	Invite       *domain.InviteDetails
	Token        string
	WrongAccount bool
}

func (h *Handler) invitePage(w http.ResponseWriter, r *http.Request, p *principal) {
	token := r.URL.Query().Get("token")
	inv, err := h.Orgs.InviteDetails(r.Context(), token)
	if errors.Is(err, domain.ErrNotFound) {
		h.renderStatus(w, r, p, http.StatusNotFound, "Invite not found", "This invite link is incomplete or no longer exists. Ask for a new one.")
		return
	}
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	switch {
	case inv.Accepted:
		h.renderStatus(w, r, p, http.StatusGone, "Invite already used", "Open your organizations to find "+inv.OrgName+".")
		return
	case inv.Revoked || time.Now().After(inv.ExpiresAt):
		h.renderStatus(w, r, p, http.StatusGone, "Invite no longer valid", "This invite was withdrawn or has expired. Ask for a new one.")
		return
	}
	h.render(w, http.StatusOK, "invite", p, view{Title: "Join " + inv.OrgName, Data: inviteData{
		Invite: inv, Token: token, WrongAccount: !strings.EqualFold(inv.Email, p.account.Email),
	}})
}

func (h *Handler) acceptInvitePost(w http.ResponseWriter, r *http.Request, p *principal) {
	m, inv, err := h.Orgs.AcceptInvite(r.Context(), p.scope(), p.account, r.PostFormValue("token"))
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	if inv.EventID != "" {
		http.Redirect(w, r, "/o/"+m.Slug+"/events/"+inv.EventID+"?notice=joined", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/o/"+m.Slug+"?notice=joined", http.StatusSeeOther)
}

// orgData backs members.html; its methods decide which controls to show.
// The service checks again on submit.
type orgData struct {
	Org     *domain.OrgMembership
	Tab     string
	Me      string
	Members []domain.OrgMember
	Invites []domain.Invite
	sc      authz.OrgScope
}

func (d orgData) CanEditOrg() bool     { return d.sc.Can(authz.UpdateOrg) }
func (d orgData) OrgTab() string       { return d.Tab }
func (d orgData) CanSeeMembers() bool  { return d.sc.Can(authz.ViewMembers) }
func (d orgData) CanSeeActivity() bool { return d.sc.Can(authz.ViewAudit) }
func (d orgData) CanCreate() bool      { return d.sc.Can(authz.CreateEvent) }

func (d orgData) CanManage() bool     { return d.sc.Can(authz.ManageMembers) }
func (d orgData) CanGrantOwner() bool { return d.sc.Can(authz.GrantOwner) }
func (d orgData) CanChange(m domain.OrgMember) bool {
	return d.CanManage() && (m.Role != domain.OrgRoleOwner || d.CanGrantOwner())
}
func (d orgData) Expired(i domain.Invite) bool { return time.Now().After(i.ExpiresAt) }

// Fixed notices, chosen by key so the query string can't inject text.
var orgNotices = map[string]string{
	"created":  "Organization created. Create your first event, or invite your team from Members.",
	"joined":   "You joined the organization.",
	"invited":  "Invite sent.",
	"revoked":  "Invite withdrawn.",
	"role":     "Role updated.",
	"removed":  "Member removed.",
	"settings": "Organization settings saved.",
	"trashed":  "Event moved to Trash. You can restore it for 30 days.",
}

func (h *Handler) membersPage(w http.ResponseWriter, r *http.Request, o *orgReq) {
	h.renderOrg(w, r, o, http.StatusOK, "", orgNotices[r.URL.Query().Get("notice")])
}

func (h *Handler) renderOrg(w http.ResponseWriter, r *http.Request, o *orgReq, status int, errMsg, notice string) {
	members, err := h.Orgs.ListMembers(r.Context(), o.sc)
	if err != nil {
		h.renderError(w, r, o.principal, err)
		return
	}
	d := orgData{Org: o.org, Tab: "members", Me: o.account.ID, Members: members, sc: o.sc}
	if o.sc.Can(authz.ViewInvites) {
		if d.Invites, err = h.Orgs.ListInvites(r.Context(), o.sc); err != nil {
			h.renderError(w, r, o.principal, err)
			return
		}
	}
	h.render(w, status, "members", o.principal, view{Title: "Members · " + o.org.Name, Error: errMsg, Notice: notice, Data: d})
}

// orgAction runs a change from the org page: back to the page with a notice,
// or the page again with the error.
func (h *Handler) orgAction(w http.ResponseWriter, r *http.Request, o *orgReq, notice string, err error) {
	if err != nil {
		status, msg := pageErrorStatus(err)
		switch status {
		case http.StatusInternalServerError, http.StatusNotFound:
			h.renderError(w, r, o.principal, err)
		default:
			h.renderOrg(w, r, o, status, msg, "")
		}
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"/members?notice="+notice, http.StatusSeeOther)
}

func (h *Handler) orgInvitePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	_, err := h.Orgs.Invite(r.Context(), o.sc, o.org, o.account, r.PostFormValue("email"), r.PostFormValue("role"))
	h.orgAction(w, r, o, "invited", err)
}

func (h *Handler) orgRevokePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	h.orgAction(w, r, o, "revoked", h.Orgs.RevokeInvite(r.Context(), o.sc, r.PathValue("id")))
}

func (h *Handler) orgRolePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	h.orgAction(w, r, o, "role", h.Orgs.SetRole(r.Context(), o.sc, r.PathValue("account"), r.PostFormValue("role")))
}

func (h *Handler) orgRemovePost(w http.ResponseWriter, r *http.Request, o *orgReq) {
	accountID := r.PathValue("account")
	if err := h.Orgs.RemoveMember(r.Context(), o.sc, accountID); err != nil {
		h.orgAction(w, r, o, "", err)
		return
	}
	if accountID == o.account.ID { // left the org: its page is gone for you
		http.Redirect(w, r, "/app", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/o/"+o.org.Slug+"/members?notice=removed", http.StatusSeeOther)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
