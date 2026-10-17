package httphandler

import (
	"net/http"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// GET /api/v1/admin/fields (admin, superadmin)
// Every custom field with usage counts and, for filter fields, common values.
func (h *Handler) listFields(w http.ResponseWriter, r *http.Request) {
	list, err := h.Fields.List(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, list)
}

// POST /api/v1/admin/fields (admin, superadmin)
// Body: {"field_key", "field_label", "data_type", "show_in_scanner_popup",
// "show_in_dashboard_filter", "sort_order"}. field_key may be left out; it is
// then made from the label.
func (h *Handler) createField(w http.ResponseWriter, r *http.Request) {
	var req domain.FieldCreateRequest
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	f, err := h.Fields.Create(r.Context(), claimsFrom(r.Context()).Subject, req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	h.manifests.invalidate()
	writeJSON(w, http.StatusCreated, f)
}

// PATCH /api/v1/admin/fields/{key} (admin, superadmin)
// Body: any of {"field_label", "data_type", "show_in_scanner_popup",
// "show_in_dashboard_filter", "sort_order"}.
func (h *Handler) updateField(w http.ResponseWriter, r *http.Request) {
	var req domain.FieldUpdateRequest
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	f, err := h.Fields.Update(r.Context(), claimsFrom(r.Context()).Subject, r.PathValue("key"), req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	// Scanner popup fields are cut into the manifest: rebuild it now rather
	// than serve the old set until the cache expires.
	h.manifests.invalidate()
	writeJSON(w, http.StatusOK, f)
}

// DELETE /api/v1/admin/fields/{key} (superadmin)
// Removes the field and every delegate's value for it.
func (h *Handler) deleteField(w http.ResponseWriter, r *http.Request) {
	n, err := h.Fields.Delete(r.Context(), claimsFrom(r.Context()).Subject, r.PathValue("key"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	h.manifests.invalidate()
	writeJSON(w, http.StatusOK, map[string]int{"values_removed": n})
}

// GET /api/v1/fields/scanner (any signed-in account)
// Labels and types of the fields shown after a scan, in display order. The
// values themselves come with the manifest.
func (h *Handler) scannerFields(w http.ResponseWriter, r *http.Request) {
	list, err := h.Fields.ScannerFields(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-cache")
	writeJSON(w, http.StatusOK, list)
}
