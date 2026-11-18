package httphandler

// Help pages (milestone M6): short how-tos at /help, readable without
// signing in. Each topic's text is web/templates/help/<slug>.html.

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

type helpTopic struct {
	Slug, Title, Summary, Group string
}

// helpGroups orders the help index.
var helpGroups = []string{"Getting started", "On the day", "Google integrations", "For participants"}

var helpTopics = []helpTopic{
	{"getting-started", "Set up your first event", "From a new organization to a pass in someone's hand.", "Getting started"},
	{"registration", "Registration and publishing", "Questions, approval, capacity and the public page.", "Getting started"},
	{"pass-design", "Design the pass", "Templates, the designer and its scan checks.", "Getting started"},
	{"sharing", "Share an event and roles", "Who can do what: owner, editor, check-in staff, viewer.", "Getting started"},
	{"workshops", "Workshops and sign-ups", "Sessions inside an event, with their own capacity.", "Getting started"},
	{"check-in-day", "Running check-in on the day", "Checkpoints, the door, the dashboard and fixing mistakes.", "On the day"},
	{"scanner", "The scanner on a phone", "Camera, colors, fast lane, and scanning without a connection.", "On the day"},
	{"google-sheets", "Keep a Google Sheet up to date", "A spreadsheet of participants, check-ins and workshops.", "Google integrations"},
	{"google-form", "Import from a Google Form", "Turn form responses into participants with passes.", "Google integrations"},
	{"my-passes", "Your pass", "For participants: find and use your pass, your profile and the participant map.", "For participants"},
}

func helpSlugs() []string {
	out := make([]string, len(helpTopics))
	for i, t := range helpTopics {
		out[i] = t.Slug
	}
	return out
}

func helpTopicBySlug(slug string) (helpTopic, bool) {
	for _, t := range helpTopics {
		if t.Slug == slug {
			return t, true
		}
	}
	return helpTopic{}, false
}

type helpGroup struct {
	Name   string
	Topics []helpTopic
}

type helpData struct {
	Topic  helpTopic // empty on the index
	Groups []helpGroup
	Status string // STATUS_PAGE_URL
}

func (h *Handler) helpData(t helpTopic) helpData {
	d := helpData{Topic: t, Status: h.StatusPageURL}
	for _, g := range helpGroups {
		hg := helpGroup{Name: g}
		for _, x := range helpTopics {
			if x.Group == g {
				hg.Topics = append(hg.Topics, x)
			}
		}
		d.Groups = append(d.Groups, hg)
	}
	return d
}

// helpPrincipal shows the signed-in header when there is a session; help
// pages work either way.
func (h *Handler) helpPrincipal(r *http.Request) *principal {
	p, err := h.currentPrincipal(r)
	if err != nil {
		return nil
	}
	return p
}

// GET /api/v1/health: for an uptime monitor behind the status page. 200
// with "ok" or "degraded", 503 when the database is down. /healthz stays
// the cheap liveness probe for the platform.
//
// The per-check detail (how late emails are) is operational information, so
// it is shown only with "Authorization: Bearer <HEALTH_TOKEN>". Everyone
// else gets the status word alone, still enough for a public monitor.
func (h *Handler) apiHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if h.Health == nil {
		writeJSON(w, http.StatusOK, service.HealthReport{Status: "ok", Checks: map[string]string{}})
		return
	}
	rep := h.Health.Check(r.Context())
	status := http.StatusOK
	if rep.Status == "down" {
		status = http.StatusServiceUnavailable
	}
	if !h.healthAuthorized(r) {
		up := rep.Status
		if up == "degraded" {
			up = "ok" // "degraded" reveals the email queue is behind
		}
		rep = service.HealthReport{Status: up, Checks: map[string]string{}}
	}
	writeJSON(w, status, rep)
}

func (h *Handler) healthAuthorized(r *http.Request) bool {
	if h.HealthToken == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(h.HealthToken)) == 1
}

func (h *Handler) helpIndex(w http.ResponseWriter, r *http.Request) {
	h.render(w, http.StatusOK, "help/index", h.helpPrincipal(r), view{Title: "Help", Data: h.helpData(helpTopic{})})
}

func (h *Handler) helpPage(w http.ResponseWriter, r *http.Request) {
	t, ok := helpTopicBySlug(r.PathValue("topic"))
	if !ok {
		h.renderStatus(w, r, h.helpPrincipal(r), http.StatusNotFound, "No such help page", "See all help topics at /help.")
		return
	}
	h.render(w, http.StatusOK, "help/"+t.Slug, h.helpPrincipal(r), view{Title: t.Title + " · Help", Data: h.helpData(t)})
}
