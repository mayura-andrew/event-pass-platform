package httphandler

import "net/http"

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// POST /api/v1/auth/login
func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "email and password are required")
		return
	}
	res, err := h.Auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// POST /api/v1/auth/change-password
//
// Works with a token from a temporary-password login (the only endpoint that
// does). Returns a fresh session in the same shape as /auth/login.
func (h *Handler) changePassword(w http.ResponseWriter, r *http.Request) {
	var req changePasswordRequest
	if !decodeJSON(w, r, 4<<10, &req) {
		return
	}
	if req.CurrentPassword == "" || req.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "current_password and new_password are required")
		return
	}
	id := claimsFrom(r.Context()).Subject
	res, err := h.Auth.ChangePassword(r.Context(), id, req.CurrentPassword, req.NewPassword)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	h.accounts.forget(id)
	writeJSON(w, http.StatusOK, res)
}
