// Package httphandler exposes the REST API and serves the static PWA.
package httphandler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"` // machine-readable, for cases clients branch on
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg})
}

// writeDomainError maps service errors to HTTP status codes without leaking
// internal details for unexpected failures.
func writeDomainError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, domain.ErrInvalidCredentials):
		writeError(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, domain.ErrPasswordChangeRequired):
		writeJSON(w, http.StatusForbidden, errorBody{
			Error: "Set a new password before continuing",
			Code:  "PASSWORD_CHANGE_REQUIRED",
		})
	case errors.Is(err, domain.ErrEmailUnavailable):
		writeError(w, http.StatusServiceUnavailable, strings.TrimPrefix(err.Error(), domain.ErrEmailUnavailable.Error()+": "))
	case errors.Is(err, domain.ErrUnauthenticated):
		writeError(w, http.StatusUnauthorized, domain.ErrUnauthenticated.Error())
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, http.StatusForbidden, strings.TrimPrefix(err.Error(), domain.ErrForbidden.Error()+": "))
	case errors.Is(err, domain.ErrRateLimited):
		w.Header().Set("Retry-After", "900")
		writeError(w, http.StatusTooManyRequests, strings.TrimPrefix(err.Error(), domain.ErrRateLimited.Error()+": "))
	case errors.Is(err, domain.ErrConflict) && err != domain.ErrConflict:
		// Wrapped with a specific message (e.g. duplicate staff email).
		writeError(w, http.StatusConflict, strings.TrimPrefix(err.Error(), domain.ErrConflict.Error()+": "))
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "a record with that code already exists")
	default:
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

// decodeJSON reads a size-limited JSON body.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return false
	}
	return true
}
