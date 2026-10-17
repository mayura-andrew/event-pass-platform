package httphandler

import (
	"net/http"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// POST /api/v1/scans/sync
func (h *Handler) syncScans(w http.ResponseWriter, r *http.Request) {
	var req domain.SyncRequest
	// 500 scans * ~300 bytes is well under 1 MiB.
	if !decodeJSON(w, r, 1<<20, &req) {
		return
	}
	c := claimsFrom(r.Context())

	results, err := h.Sync.ProcessBatchSync(r.Context(), c.Subject, c.Role, req.Scans)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, domain.SyncResponse{Results: results})
}

// GET /api/v1/scans/claims?since={RFC3339}
func (h *Handler) claims(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if s := r.URL.Query().Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be an RFC 3339 timestamp")
			return
		}
		since = t
	}
	list, err := h.Sync.ListClaims(r.Context(), since)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"claims":      list,
		"server_time": time.Now().UTC(),
	})
}
