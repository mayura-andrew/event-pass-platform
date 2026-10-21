package postgres

import (
	"context"
	"database/sql"
	"time"
)

// HealthRepo answers the status check: is the database up, and are emails
// going out.
type HealthRepo struct{ db *sql.DB }

func NewHealthRepo(db *sql.DB) *HealthRepo { return &HealthRepo{db: db} }

// EmailDelay is how long the most overdue participant email has waited
// past its send time (0 when none is waiting).
func (r *HealthRepo) EmailDelay(ctx context.Context) (time.Duration, error) {
	var secs float64
	err := r.db.QueryRowContext(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(next_attempt_at)), 0)
		FROM outbox WHERE status IN ('queued', 'sending') AND next_attempt_at < NOW()`).Scan(&secs)
	return time.Duration(max(secs, 0) * float64(time.Second)), err
}
