package httphandler

import (
	"net/http"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// POST /api/v1/admin/staff/create (superadmin)
//
// Creates the account with a generated temporary password and emails it. If
// the email can't be sent, nothing is created and the response is 503.
func (h *Handler) createStaff(w http.ResponseWriter, r *http.Request) {
	var req domain.StaffCreateRequest
	if !decodeJSON(w, r, 8<<10, &req) {
		return
	}
	res, err := h.Staff.Create(r.Context(), claimsFrom(r.Context()).Subject, req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

// GET /api/v1/admin/staff/list (admin, superadmin)
func (h *Handler) listStaff(w http.ResponseWriter, r *http.Request) {
	list, err := h.Staff.List(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, list)
}

// PATCH /api/v1/admin/staff/{id} (superadmin)
// Body: any of {"full_name", "role", "is_active", "scenario_ids"}.
func (h *Handler) updateStaff(w http.ResponseWriter, r *http.Request) {
	var req domain.StaffUpdateRequest
	if !decodeJSON(w, r, 8<<10, &req) {
		return
	}
	id := r.PathValue("id")
	m, err := h.Staff.Update(r.Context(), claimsFrom(r.Context()).Subject, id, req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	h.accounts.forget(id) // apply deactivation/role change on this instance now
	writeJSON(w, http.StatusOK, m)
}

// POST /api/v1/admin/staff/{id}/resend-credentials (superadmin)
// Issues and emails a new temporary password; the old password stops working.
func (h *Handler) resendCredentials(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := h.Staff.ResendCredentials(r.Context(), claimsFrom(r.Context()).Subject, id)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	h.accounts.forget(id) // signed-in devices must now set a new password
	writeJSON(w, http.StatusOK, res)
}
