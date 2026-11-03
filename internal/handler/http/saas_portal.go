package httphandler

// "My passes": the participant portal (milestone M3).

import (
	"net/http"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

func (h *Handler) registerPortal(mux *http.ServeMux) {
	mux.Handle("GET /me", h.pageAuthed((*Handler).myPassesPage))
	mux.Handle("GET /me/passes/{participant}", h.pageAuthed((*Handler).myPassPage))
	mux.Handle("GET /me/passes/{participant}/pass.png", h.pageAuthed((*Handler).myPassPNG))
	mux.Handle("GET /me/passes/{participant}/pass.pdf", h.pageAuthed((*Handler).myPassPDF))
	mux.Handle("POST /me/passes/{participant}/workshops/{workshop}/signup", h.pageAuthed((*Handler).myWorkshopSignUp))
	mux.Handle("POST /me/passes/{participant}/workshops/{workshop}/cancel", h.pageAuthed((*Handler).myWorkshopCancel))
	mux.Handle("GET /api/v1/me/passes", h.apiAuthed((*Handler).apiMyPasses))
	h.registerProfiles(mux)
}

type portalPass struct {
	domain.PassContext
	Upcoming bool
}

func (h *Handler) myPassesPage(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := h.Portal.MyPasses(r.Context(), p.scope())
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	now := time.Now()
	var cards []portalPass
	for _, pc := range list {
		cards = append(cards, portalPass{PassContext: pc, Upcoming: pc.Event.EndsAt == nil || pc.Event.EndsAt.After(now)})
	}
	h.render(w, http.StatusOK, "my_passes", p, view{Title: "My passes", Data: cards})
}

type myPassData struct {
	Pass      *domain.PassContext
	Answers   []passes.Line
	Workshops []domain.Workshop
	// Profile (beta): the saved one, or the form as posted when it had
	// mistakes, with the mistakes by field.
	Profile       *domain.ParticipantProfile
	ProfileForm   *service.ProfileInput
	ProfileErrors service.FieldErrors
	MapOn         bool
}

var myPassNotices = map[string]string{
	domain.SignupSignedUp:   "You're signed up.",
	domain.SignupWaitlisted: "That workshop is full, so you're on its waitlist.",
	"cancelled":             "Sign-up cancelled.",
	"profile":               "Profile saved.",
}

func (h *Handler) myPassPage(w http.ResponseWriter, r *http.Request, p *principal) {
	pc, err := h.Portal.MyPass(r.Context(), p.scope(), r.PathValue("participant"))
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	h.renderMyPass(w, r, p, pc, nil)
}

// renderMyPass shows a pass with its event's workshops; actionErr is a
// failed sign-up shown on the page.
func (h *Handler) renderMyPass(w http.ResponseWriter, r *http.Request, p *principal, pc *domain.PassContext, actionErr error) {
	h.renderMyPassWith(w, r, p, pc, actionErr, nil, nil)
}

func (h *Handler) renderMyPassWith(w http.ResponseWriter, r *http.Request, p *principal, pc *domain.PassContext, actionErr error,
	profileErrs service.FieldErrors, posted *service.ProfileInput) {
	d := myPassData{Pass: pc, Answers: service.ParticipantAnswers(pc), ProfileErrors: profileErrs, ProfileForm: posted}
	if h.Profiles != nil {
		prof, on, err := h.Profiles.Profile(r.Context(), p.scope(), pc.Participant.ID)
		if err != nil {
			h.renderError(w, r, p, err)
			return
		}
		d.Profile, d.MapOn = prof, on && pc.Participant.Status == domain.PartRegistered
	}
	if len(profileErrs) > 0 && actionErr == nil {
		actionErr = profileErrs
	}
	if h.Checkins != nil {
		ws, err := h.Checkins.MyWorkshops(r.Context(), p.scope(), pc)
		if err != nil {
			h.renderError(w, r, p, err)
			return
		}
		d.Workshops = ws
	}
	status, msg := http.StatusOK, ""
	if actionErr != nil {
		if status, msg = pageErrorStatus(actionErr); status == http.StatusInternalServerError {
			h.renderError(w, r, p, actionErr)
			return
		}
	}
	h.render(w, status, "my_pass", p, view{Title: pc.Event.Name, Error: msg,
		Notice: myPassNotices[r.URL.Query().Get("notice")], Data: d})
}

func (h *Handler) myPassPNG(w http.ResponseWriter, r *http.Request, p *principal) {
	h.myPassFile(w, r, p, "png")
}
func (h *Handler) myPassPDF(w http.ResponseWriter, r *http.Request, p *principal) {
	h.myPassFile(w, r, p, "pdf")
}

func (h *Handler) myPassFile(w http.ResponseWriter, r *http.Request, p *principal, kind string) {
	pc, err := h.Portal.MyPass(r.Context(), p.scope(), r.PathValue("participant"))
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	h.sendPass(w, r, p, pc, kind)
}

// GET /api/v1/me/passes
func (h *Handler) apiMyPasses(w http.ResponseWriter, r *http.Request, p *principal) {
	list, err := h.Portal.MyPasses(r.Context(), p.scope())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	type item struct {
		domain.EventParticipant
		EventName string `json:"event_name"`
		OrgName   string `json:"org_name"`
		When      string `json:"when"`
	}
	out := make([]item, 0, len(list))
	for _, pc := range list {
		out = append(out, item{EventParticipant: pc.Participant, EventName: pc.Event.Name, OrgName: pc.OrgName, When: pc.Event.DateRange()})
	}
	writeJSON(w, http.StatusOK, out)
}
