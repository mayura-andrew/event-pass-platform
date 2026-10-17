package httphandler

import (
	"net/http"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// GET /api/v1/scenarios/active
//
// What a scanner downloads: every active scenario for admins and superadmins,
// only the ones granted in staff_scenario_grants for scanner accounts.
func (h *Handler) activeScenarios(w http.ResponseWriter, r *http.Request) {
	c := claimsFrom(r.Context())
	list, err := h.Scenarios.ListActiveForAccount(r.Context(), c.Subject, c.Role)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// GET /api/v1/scenarios (admin dashboard: includes inactive)
func (h *Handler) allScenarios(w http.ResponseWriter, r *http.Request) {
	list, err := h.Scenarios.List(r.Context(), false)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// POST /api/v1/scenarios
func (h *Handler) createScenario(w http.ResponseWriter, r *http.Request) {
	var in domain.ScenarioInput
	if !decodeJSON(w, r, 8<<10, &in) {
		return
	}
	s, err := h.Scenarios.Create(r.Context(), in)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, s)
}

// PATCH /api/v1/scenarios/{id}
func (h *Handler) updateScenario(w http.ResponseWriter, r *http.Request) {
	var in domain.ScenarioInput
	if !decodeJSON(w, r, 8<<10, &in) {
		return
	}
	s, err := h.Scenarios.Update(r.Context(), r.PathValue("id"), in)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}
