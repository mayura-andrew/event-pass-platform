package httphandler

import (
	"net/http"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// POST /api/v1/admin/emails/send-single (admin, superadmin)
// Body: {"participant_id"} or {"membership_id"}. Queues one delegate's pass
// email, also as a resend; 409 if one is already waiting.
func (h *Handler) sendSingleEmail(w http.ResponseWriter, r *http.Request) {
	var req domain.EmailSingleRequest
	if !decodeJSON(w, r, 1<<10, &req) {
		return
	}
	res, err := h.Emails.SendSingle(r.Context(), claimsFrom(r.Context()).Subject, req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// POST /api/v1/admin/emails/send-bulk (admin, superadmin)
// Queues a pass email for every confirmed delegate with none sent or queued.
// Safe to repeat: nobody is queued twice.
func (h *Handler) sendBulkEmail(w http.ResponseWriter, r *http.Request) {
	res, err := h.Emails.SendBulk(r.Context(), claimsFrom(r.Context()).Subject)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

// GET /api/v1/admin/emails/status (admin, superadmin)
func (h *Handler) emailStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.Emails.Status(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// POST /api/v1/admin/emails/pause-toggle (admin, superadmin)
// Body: {"paused": true|false}. An explicit state rather than a flip, so a
// double click or a retried request can't undo itself.
func (h *Handler) toggleEmailPause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused *bool `json:"paused"`
	}
	if !decodeJSON(w, r, 1<<10, &req) {
		return
	}
	if req.Paused == nil {
		writeError(w, http.StatusBadRequest, `send {"paused": true} or {"paused": false}`)
		return
	}
	st, err := h.Emails.SetPaused(r.Context(), claimsFrom(r.Context()).Subject, *req.Paused)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
