package httphandler

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// GET /api/v1/participants/manifest
//
// Returns the full confirmed directory. Supports If-None-Match so scanners
// that already hold the current manifest get a 304, and gzips the body because
// a few thousand delegates is ~1-2 MB of JSON over venue Wi-Fi.
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request) {
	data, err := h.Sync.GetConfirmedManifest(r.Context())
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	body, err := json.Marshal(data)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Accept-Encoding, Authorization")
	w.Header().Set("X-Manifest-Count", strconv.Itoa(len(data)))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		_, _ = gz.Write(body)
		_ = gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// GET /api/v1/participants/pass?id={membership_id}&email={email}
//
// Public endpoint. Both fields are required, and a wrong email returns the
// same 404 as an unknown ID so the endpoint can't be used to enumerate.
func (h *Handler) pass(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := h.Participants.GetPass(r.Context(), q.Get("id"), q.Get("email"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, p)
}

// GET /api/v1/config/public: browser-safe settings for the portal.
func (h *Handler) publicConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"google_maps_api_key": h.MapsAPIKey,
		"google_maps_map_id":  h.MapsMapID,
	})
}
