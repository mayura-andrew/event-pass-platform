package httphandler

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// importReadWindow is how long a CSV upload may take to arrive. The server's
// ReadTimeout (30 s) suits small JSON bodies; a 5 MB file over venue Wi-Fi
// needs longer.
const importReadWindow = 50 * time.Second

// ingestOff answers when HMAC_SECRET_KEY isn't set: without it the server
// can't sign passes, so it must not create delegates.
func ingestOff(w http.ResponseWriter) {
	writeError(w, http.StatusServiceUnavailable,
		"Adding delegates is turned off on this server: HMAC_SECRET_KEY is not set.")
}

// POST /api/v1/admin/participants/create (admin, superadmin)
// Body: {"full_name", "email", "team_name", "date_of_birth", "venue_id"}.
// The membership ID and QR signature are generated.
func (h *Handler) createParticipant(w http.ResponseWriter, r *http.Request) {
	if h.Ingest == nil {
		ingestOff(w)
		return
	}
	var req domain.ParticipantCreateRequest
	if !decodeJSON(w, r, 8<<10, &req) {
		return
	}
	p, err := h.Ingest.Create(r.Context(), claimsFrom(r.Context()).Subject, req)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// POST /api/v1/admin/participants/upload-csv (admin, superadmin)
//
// Body: multipart/form-data with the file in the "file" field, or the CSV
// itself as text/csv. The file is parsed as it streams in, never written to
// disk. Returns 200 with the import report even when some rows failed; 4xx
// means the file as a whole was rejected and nothing was added.
func (h *Handler) uploadParticipantsCSV(w http.ResponseWriter, r *http.Request) {
	if h.Ingest == nil {
		ingestOff(w)
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(importReadWindow))
	// Room for the multipart boundary and part headers on top of the file.
	r.Body = http.MaxBytesReader(w, r.Body, domain.MaxImportBytes+64<<10)

	src, status, msg := csvSource(r)
	if src == nil {
		writeError(w, status, msg)
		return
	}
	res, err := h.Ingest.ImportCSV(r.Context(), claimsFrom(r.Context()).Subject, src)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("The file is larger than %d MB. Split it and upload each part.", domain.MaxImportBytes>>20))
		return
	}
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}

// csvSource finds the CSV in the request body. On failure it returns a nil
// reader with the status and message to send.
func csvSource(r *http.Request) (io.Reader, int, string) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "multipart/form-data":
		mr, err := r.MultipartReader()
		if err != nil {
			return nil, http.StatusBadRequest, "invalid multipart body"
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil, http.StatusBadRequest, `no file in the "file" field`
			}
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return nil, http.StatusRequestEntityTooLarge, "request body too large"
			}
			if err != nil {
				return nil, http.StatusBadRequest, "invalid multipart body"
			}
			if part.FormName() == "file" {
				return part, 0, ""
			}
		}
	case "text/csv", "text/plain", "application/csv", "application/vnd.ms-excel":
		return r.Body, 0, ""
	}
	return nil, http.StatusUnsupportedMediaType, "send the file as multipart/form-data (field \"file\") or text/csv"
}
