package httphandler

// "My passes": the participant portal (milestone M3).

import (
	"net/http"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/passes"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

func (h *Handler) registerPortal(mux *http.ServeMux) {
	mux.Handle("GET /me", h.pageAuthed((*Handler).myPassesPage))
	mux.Handle("GET /me/passes/{participant}", h.pageAuthed((*Handler).myPassPage))
	mux.Handle("GET /me/passes/{participant}/pass.png", h.pageAuthed((*Handler).myPassPNG))
	mux.Handle("GET /me/passes/{participant}/pass.pdf", h.pageAuthed((*Handler).myPassPDF))
	mux.Handle("GET /api/v1/me/passes", h.apiAuthed((*Handler).apiMyPasses))
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
	Pass    *domain.PassContext
	Answers []passes.Line
}

func (h *Handler) myPassPage(w http.ResponseWriter, r *http.Request, p *principal) {
	pc, err := h.Portal.MyPass(r.Context(), p.scope(), r.PathValue("participant"))
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	h.render(w, http.StatusOK, "my_pass", p, view{Title: pc.Event.Name, Data: myPassData{Pass: pc, Answers: service.ParticipantAnswers(pc)}})
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
