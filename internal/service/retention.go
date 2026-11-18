package service

import (
	"context"
	"log/slog"
	"time"
)

// RetentionStore is postgres.RetentionRepo.
type RetentionStore interface {
	ClearBirthDates(ctx context.Context, after time.Duration) (int, error)
	ExpireLocations(ctx context.Context, olderThan time.Duration) (int, error)
}

// Retention deletes personal data on a schedule (security review F6): map
// locations a few minutes after they stop being shown, and birth dates a
// while after the event ends (they are collected to count minors at the
// event, not to keep).
type Retention struct {
	store RetentionStore
	// BirthDatesAfter is how long after an event ends birth dates are kept
	// (0: forever).
	BirthDatesAfter time.Duration
	Interval        time.Duration
}

func NewRetention(store RetentionStore, birthDatesAfter time.Duration) *Retention {
	return &Retention{store: store, BirthDatesAfter: birthDatesAfter, Interval: 10 * time.Minute}
}

// Run sweeps until ctx ends.
func (r *Retention) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		r.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep runs one pass.
func (r *Retention) Sweep(ctx context.Context) {
	if n, err := r.store.ExpireLocations(ctx, 3*LocationFresh); err != nil && ctx.Err() == nil {
		slog.Warn("retention: expire locations", "err", err)
	} else if n > 0 {
		slog.Info("retention: expired map locations", "count", n)
	}
	if r.BirthDatesAfter <= 0 {
		return
	}
	if n, err := r.store.ClearBirthDates(ctx, r.BirthDatesAfter); err != nil && ctx.Err() == nil {
		slog.Warn("retention: clear birth dates", "err", err)
	} else if n > 0 {
		slog.Info("retention: cleared birth dates after events ended", "count", n)
	}
}
