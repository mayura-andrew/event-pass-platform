package httphandler

// "Where you're signed in" (security review F7): the account's live
// sessions, signing out one device, and signing out everywhere else.

import (
	"net/http"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

func (h *Handler) registerSessions(mux *http.ServeMux) {
	mux.Handle("GET /account/sessions", h.pageAuthed((*Handler).sessionsPage))
	mux.Handle("POST /account/sessions/{id}/revoke", h.pageAuthed((*Handler).sessionRevokePost))
	mux.Handle("POST /account/sessions/revoke-others", h.pageAuthed((*Handler).sessionsRevokeOthersPost))
	mux.Handle("GET /api/v1/me/sessions", h.apiAuthed((*Handler).apiSessions))
	mux.Handle("DELETE /api/v1/me/sessions/{id}", h.apiAuthed((*Handler).apiRevokeSession))
	mux.Handle("POST /api/v1/me/sessions/revoke-others", h.apiAuthed((*Handler).apiRevokeOtherSessions))
}

type sessionRow struct {
	domain.Session
	Current bool `json:"current"`
}

type sessionsData struct {
	Sessions []sessionRow
	Now      time.Time
}

var sessionNotices = map[string]string{
	"revoked": "That device is signed out.",
	"others":  "Signed out everywhere else.",
}

func (h *Handler) sessionRows(r *http.Request, p *principal) ([]sessionRow, error) {
	list, err := h.SignIn.Sessions(r.Context(), p.account.ID)
	if err != nil {
		return nil, err
	}
	out := make([]sessionRow, 0, len(list))
	for _, s := range list {
		out = append(out, sessionRow{Session: s, Current: s.ID == p.session.ID})
	}
	return out, nil
}

func (h *Handler) sessionsPage(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := h.sessionRows(r, p)
	if err != nil {
		h.renderError(w, r, p, err)
		return
	}
	h.render(w, http.StatusOK, "sessions", p, view{Title: "Where you're signed in",
		Notice: sessionNotices[r.URL.Query().Get("notice")], Data: sessionsData{Sessions: rows, Now: time.Now()}})
}

func (h *Handler) sessionRevokePost(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	if err := h.SignIn.RevokeSession(r.Context(), p.account.ID, id); err != nil {
		h.renderError(w, r, p, err)
		return
	}
	if id == p.session.ID {
		h.clearCookie(w, h.sessionCookie(), "/")
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/account/sessions?notice=revoked", http.StatusSeeOther)
}

func (h *Handler) sessionsRevokeOthersPost(w http.ResponseWriter, r *http.Request, p *principal) {
	if _, err := h.SignIn.RevokeOtherSessions(r.Context(), p.account.ID, p.session.ID); err != nil {
		h.renderError(w, r, p, err)
		return
	}
	http.Redirect(w, r, "/account/sessions?notice=others", http.StatusSeeOther)
}

func (h *Handler) apiSessions(w http.ResponseWriter, r *http.Request, p *principal) {
	rows, err := h.sessionRows(r, p)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, rows)
}

func (h *Handler) apiRevokeSession(w http.ResponseWriter, r *http.Request, p *principal) {
	id := r.PathValue("id")
	if err := h.SignIn.RevokeSession(r.Context(), p.account.ID, id); err != nil {
		writeDomainError(w, r, err)
		return
	}
	if id == p.session.ID {
		h.clearCookie(w, h.sessionCookie(), "/")
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) apiRevokeOtherSessions(w http.ResponseWriter, r *http.Request, p *principal) {
	n, err := h.SignIn.RevokeOtherSessions(r.Context(), p.account.ID, p.session.ID)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"revoked": n})
}
