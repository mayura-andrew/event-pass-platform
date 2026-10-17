package httphandler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// GET /api/v1/admin/dashboard/stats
func (h *Handler) dashboardStats(w http.ResponseWriter, r *http.Request) {
	st, err := h.Dashboard.Stats(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}

// POST /api/v1/admin/participants/filter
//
// POST rather than GET so the search text (names, emails) stays out of proxy
// and access logs.
func (h *Handler) filterParticipants(w http.ResponseWriter, r *http.Request) {
	var f domain.ParticipantFilter
	if !decodeJSON(w, r, 4<<10, &f) {
		return
	}
	page, err := h.Dashboard.FilterParticipants(r.Context(), f)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

// POST /api/v1/admin/scenarios/toggle  {"scenario_id": "...", "is_active": false}
func (h *Handler) toggleScenario(w http.ResponseWriter, r *http.Request) {
	var req domain.ScenarioToggleRequest
	if !decodeJSON(w, r, 1<<10, &req) {
		return
	}
	s, err := h.Dashboard.ToggleScenario(r.Context(), req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// claimConflictBody is the 409 returned when a manual override hits a claim
// that already exists, so the desk can tell the delegate who scanned them.
type claimConflictBody struct {
	Error     string    `json:"error"`
	ClaimedBy string    `json:"claimed_by"`
	ClaimedAt time.Time `json:"claimed_at"`
	Method    string    `json:"claim_method"`
}

// POST /api/v1/admin/scans/manual-override
func (h *Handler) manualOverride(w http.ResponseWriter, r *http.Request) {
	var req domain.ManualOverrideRequest
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	adminID := claimsFrom(r.Context()).Subject

	res, err := h.Dashboard.ManualOverride(r.Context(), adminID, req)
	var conflict *domain.ClaimConflictError
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, claimConflictBody{
			Error:     "This delegate has already claimed this scenario",
			ClaimedBy: conflict.ClaimedBy,
			ClaimedAt: conflict.ClaimedAt,
			Method:    conflict.Method,
		})
	case err != nil:
		writeDomainError(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, res)
	}
}

// GET /api/v1/admin/devices/health
func (h *Handler) deviceHealth(w http.ResponseWriter, r *http.Request) {
	list, err := h.Dashboard.Devices(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":               list,
		"online_window_seconds": int(domain.DeviceOnlineWindow.Seconds()),
		"server_time":           time.Now().UTC(),
	})
}

// GET /api/v1/admin/venues
func (h *Handler) listVenues(w http.ResponseWriter, r *http.Request) {
	list, err := h.Dashboard.Venues(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /api/v1/admin/scans/recent?limit=50 (newest first)
func (h *Handler) recentScans(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := h.Dashboard.RecentScans(r.Context(), limit)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, list)
}

// streamKeepAlive is shorter than common proxy idle timeouts (60 s on many
// load balancers) so an idle feed isn't cut off between scans.
const streamKeepAlive = 20 * time.Second

// GET /api/v1/admin/scans/stream
//
// Server-sent events. Each new scan is one "scan" event whose data is a
// domain.ScanEvent. The dashboard reads this with fetch() rather than
// EventSource because EventSource can't send the Authorization header, and
// putting the JWT in the URL would leak it into logs.
func (h *Handler) scanStream(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// The server's WriteTimeout would otherwise end every stream after 60 s.
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	events, cancel := h.Feed.Subscribe()
	defer cancel()

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no") // stop nginx-style proxies buffering the stream
	w.WriteHeader(http.StatusOK)

	// Tell the client how long to wait before reconnecting.
	fmt.Fprint(w, "retry: 3000\n\n")
	if err := rc.Flush(); err != nil {
		return
	}

	keepAlive := time.NewTicker(streamKeepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepAlive.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
		case batch, ok := <-events:
			if !ok {
				// Dropped as too slow, or the server is shutting down. The
				// client reconnects and reloads its backlog.
				fmt.Fprint(w, "event: reset\ndata: {}\n\n")
				_ = rc.Flush()
				return
			}
			for _, e := range batch {
				data, err := json.Marshal(e)
				if err != nil {
					continue
				}
				if _, err := fmt.Fprintf(w, "id: %s\nevent: scan\ndata: %s\n\n", e.ID, data); err != nil {
					return
				}
			}
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
}

// POST /api/v1/devices/heartbeat (any signed-in scanner)
//
// Phones report their IndexedDB queue counts so the dashboard can see scans
// that haven't uploaded yet.
func (h *Handler) deviceHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb domain.DeviceHeartbeat
	if !decodeJSON(w, r, 2<<10, &hb) {
		return
	}
	hb.UserAgent = r.UserAgent()
	if err := h.Dashboard.RecordHeartbeat(r.Context(), claimsFrom(r.Context()).Subject, hb); err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
