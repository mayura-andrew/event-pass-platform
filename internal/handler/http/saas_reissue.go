package httphandler

// Pass reissue (security review F4): a new QR generation, so a lost or
// photographed badge stops scanning.

import (
	"net/http"
	"net/url"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
)

func init() {
	eventRoutes = append(eventRoutes, []eventRoute{
		{"POST", ev + "/participants/{participant}/reissue", true, authz.EventEdit, "participant.pass.reissue", (*Handler).apiReissuePass},
		{"POST", evPage + "/participants/{participant}/reissue", false, authz.EventEdit, "participant.pass.reissue", (*Handler).reissuePassPost},
	}...)
}

func (h *Handler) apiReissuePass(w http.ResponseWriter, r *http.Request, e *eventReq) {
	noContent(w, r, h.Registrations.ReissuePass(r.Context(), e.es, r.PathValue("participant")))
}

func (h *Handler) reissuePassPost(w http.ResponseWriter, r *http.Request, e *eventReq) {
	h.eventAction(w, r, e, "participants", "reissued",
		h.Registrations.ReissuePass(r.Context(), e.es, r.PathValue("participant")))
}

// POST /me/passes/{participant}/reissue
func (h *Handler) myPassReissue(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("participant")
	if err := h.Portal.ReissueMyPass(r.Context(), p.scope(), id); err != nil {
		h.renderError(w, r, p, err)
		return
	}
	http.Redirect(w, r, "/me/passes/"+url.PathEscape(id)+"?notice=reissued", http.StatusSeeOther)
}
