package service

import (
	"context"
	"fmt"
	"time"
)

// HealthStore is postgres.HealthRepo.
type HealthStore interface {
	EmailDelay(ctx context.Context) (time.Duration, error)
}

// Health is the deep status check an uptime monitor polls (GET
// /api/v1/health). It says only up, slow or down: no tenant data.
type Health struct {
	store        HealthStore
	emailEnabled bool          // the outbox worker runs (SMTP configured)
	EmailLate    time.Duration // emails this overdue make the status "degraded"
}

func NewHealth(store HealthStore, emailEnabled bool) *Health {
	return &Health{store: store, emailEnabled: emailEnabled, EmailLate: 10 * time.Minute}
}

// HealthReport: Status is "ok", "degraded" (working, but emails are late)
// or "down" (the database can't be reached).
type HealthReport struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (h *Health) Check(ctx context.Context) HealthReport {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	rep := HealthReport{Status: "ok", Checks: map[string]string{"database": "ok", "email": "ok"}}
	delay, err := h.store.EmailDelay(ctx)
	switch {
	case err != nil:
		rep.Status, rep.Checks["database"] = "down", "unreachable"
		delete(rep.Checks, "email")
	case !h.emailEnabled:
		rep.Checks["email"] = "off"
	case delay > h.EmailLate:
		rep.Status, rep.Checks["email"] = "degraded", fmt.Sprintf("delayed %d min", int(delay.Minutes()))
	}
	return rep
}
