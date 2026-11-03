package httphandler

// Google Sheets sync and Google Form import (milestone M5): the event's
// Integrations tab and the consent round trip.

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

const integrationCookie = "g_int"

// Kinds in paths: /integrations/sheets/..., /integrations/form/...
var integrationKinds = map[string]string{"sheets": domain.IntegrationSheets, "form": domain.IntegrationForm}

func init() {
	eventRoutes = append(eventRoutes, []eventRoute{
		{"GET", ev + "/integrations", true, authz.EventEdit, "", (*Handler).apiIntegrations},
		{"GET", evPage + "/integrations", false, authz.EventEdit, "", (*Handler).integrationsPage},
	}...)
	for path, kind := range integrationKinds {
		eventRoutes = append(eventRoutes, []eventRoute{
			{"GET", evPage + "/integrations/" + path + "/connect", false, authz.EventEdit, "", integrationConnect(kind)},
			{"POST", evPage + "/integrations/" + path + "/sync", false, authz.EventEdit, "", integrationSync(kind)},
			{"POST", evPage + "/integrations/" + path + "/disconnect", false, authz.EventEdit, "", integrationDisconnect(kind)},
		}...)
	}
	eventRoutes = append(eventRoutes, []eventRoute{
		{"POST", evPage + "/integrations/form/link", false, authz.EventEdit, "", (*Handler).linkFormPost},
		{"POST", evPage + "/integrations/form/map", false, authz.EventEdit, "", (*Handler).mapFormPost},
	}...)
}

func (h *Handler) apiIntegrations(w http.ResponseWriter, r *http.Request, e *eventReq) {
	list, err := h.Integrations.List(r.Context(), e.es)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---- Page ------------------------------------------------------------------------

type integrationsData struct {
	eventPageData
	Configured bool
	Sheets     *domain.Integration
	SheetsCfg  domain.SheetsConfig
	Form       *domain.Integration
	FormCfg    domain.FormConfig
	Fields     []domain.FormField
}

var integrationNotices = map[string]string{
	"connected":    "Connected to Google.",
	"sync":         "Syncing now: the spreadsheet updates within a minute.",
	"import":       "Checking the form for new responses now.",
	"disconnected": "Disconnected. Nothing was deleted in Google.",
	"linked":       "Form linked. Check how its questions match, then start importing.",
	"mapped":       "Importing: new responses become participants within a minute.",
	"none":         "Connect Google first.",
	"unconfigured": "Google isn't set up on this server yet (GOOGLE_OAUTH_CLIENT_ID and GOOGLE_OAUTH_CLIENT_SECRET).",
	"cancelled":    "Google access wasn't granted.",
}

func (h *Handler) integrationsPage(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.renderIntegrations(w, r, e, http.StatusOK, "")
}

func (h *Handler) renderIntegrations(w http.ResponseWriter, r *http.Request, e *eventReq, status int, errMsg string) {
	list, err := h.Integrations.List(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	d := integrationsData{eventPageData: h.eventData(e, "integrations"), Configured: h.Integrations.Configured()}
	for i := range list {
		switch list[i].Kind {
		case domain.IntegrationSheets:
			d.Sheets = &list[i]
			_ = json.Unmarshal(list[i].Config, &d.SheetsCfg)
		case domain.IntegrationForm:
			d.Form = &list[i]
			_ = json.Unmarshal(list[i].Config, &d.FormCfg)
		}
	}
	if d.Form != nil {
		if d.Fields, err = h.Registrations.Fields(r.Context(), e.es); err != nil {
			h.renderError(w, r, e.principal, err)
			return
		}
	}
	h.render(w, status, "event_integrations", e.principal, view{Title: "Integrations · " + e.event.Name,
		Error: errMsg, Notice: integrationNotices[r.URL.Query().Get("notice")], Data: d})
}

// integrationAction finishes a POST: back with a notice; a missing
// connection or a mistake is shown on the page rather than as an error page.
func (h *Handler) integrationAction(w http.ResponseWriter, r *http.Request, e *eventReq, notice string, err error) {
	switch {
	case err == nil:
		h.backToEvent(w, r, e, "integrations", notice)
	case errors.Is(err, domain.ErrNotFound):
		h.backToEvent(w, r, e, "integrations", "none")
	case errors.Is(err, domain.ErrValidation):
		_, msg := pageErrorStatus(err)
		h.renderIntegrations(w, r, e, http.StatusUnprocessableEntity, msg)
	default:
		h.renderError(w, r, e.principal, err)
	}
}

// ---- Consent ----------------------------------------------------------------------

// integrationState rides in a signed cookie from connect to the callback.
type integrationState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Kind     string `json:"k"`
	Org      string `json:"o"` // slug
	Event    string `json:"e"`
}

func integrationConnect(kind string) eventFunc {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, e *eventReq) {
		st := integrationState{State: service.NewSecret(), Verifier: service.NewSecret(), Kind: kind, Org: e.org.Slug, Event: e.event.ID}
		u, err := h.Integrations.ConsentURL(e.es, kind, st.State, st.Verifier)
		if errors.Is(err, service.ErrGoogleNotConfigured) {
			h.backToEvent(w, r, e, "integrations", "unconfigured")
			return
		}
		if err != nil {
			h.renderError(w, r, e.principal, err)
			return
		}
		b, _ := json.Marshal(st)
		http.SetCookie(w, &http.Cookie{
			Name: integrationCookie, Value: h.signCookie(b), Path: "/auth/google/integration", MaxAge: 600,
			HttpOnly: true, Secure: h.secureCookies(), SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, u, http.StatusFound)
	}
}

// integrationCallback is where Google returns. The cookie says which event;
// access is checked again with the current session, as for any event page.
func (h *Handler) integrationCallback(w http.ResponseWriter, r *http.Request) {
	r, cancel := withDeadline(r, apiDeadline)
	defer cancel()
	var st integrationState
	ok := false
	if c, err := r.Cookie(integrationCookie); err == nil {
		if payload, valid := h.readSignedCookie(c.Value); valid {
			ok = json.Unmarshal(payload, &st) == nil && st.State != "" && st.Kind != ""
		}
	}
	h.clearCookie(w, integrationCookie, "/auth/google/integration")
	q := r.URL.Query()
	if !ok || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(st.State)) != 1 {
		h.renderStatus(w, r, nil, http.StatusBadRequest, "Connection expired", "Start again from the event's Integrations tab.")
		return
	}
	r.SetPathValue("org", st.Org)
	r.SetPathValue("event", st.Event)
	h.pageEvent(authz.EventEdit, func(h *Handler, w http.ResponseWriter, r *http.Request, e *eventReq) {
		if q.Get("error") != "" || q.Get("code") == "" {
			h.backToEvent(w, r, e, "integrations", "cancelled")
			return
		}
		_, err := h.Integrations.Complete(r.Context(), e.es, e.event, st.Kind, q.Get("code"), st.Verifier)
		notice := "connected"
		if st.Kind == domain.IntegrationSheets && err == nil {
			notice = "sync"
		}
		h.integrationAction(w, r, e, notice, err)
	}).ServeHTTP(w, r)
}

// ---- Actions ---------------------------------------------------------------------

func integrationSync(kind string) eventFunc {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, e *eventReq) {
		notice := "sync"
		if kind == domain.IntegrationForm {
			notice = "import"
		}
		h.integrationAction(w, r, e, notice, h.Integrations.SyncNow(r.Context(), e.es, kind))
	}
}

func integrationDisconnect(kind string) eventFunc {
	return func(h *Handler, w http.ResponseWriter, r *http.Request, e *eventReq) {
		h.integrationAction(w, r, e, "disconnected", h.Integrations.Disconnect(r.Context(), e.es, kind))
	}
}

func (h *Handler) linkFormPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.integrationAction(w, r, e, "linked", h.Integrations.LinkForm(r.Context(), e.es, r.PostFormValue("form")))
}

// mapFormPost: name=<question>, email=<question or "">, q.<question>=<field key>.
func (h *Handler) mapFormPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	fields, err := h.Registrations.Fields(r.Context(), e.es)
	if err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.renderError(w, r, e.principal, err)
		return
	}
	m := map[string]string{}
	for k, v := range r.PostForm {
		if q, ok := strings.CutPrefix(k, "q."); ok && len(v) > 0 {
			m[q] = v[0]
		}
	}
	h.integrationAction(w, r, e, "mapped", h.Integrations.MapForm(r.Context(), e.es, fields,
		r.PostFormValue("name"), r.PostFormValue("email"), m))
}
