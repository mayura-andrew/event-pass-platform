package postgres

import (
	"context"
	"database/sql"
	"time"
)

// RateLimitRepo keeps fixed-window rate limit counters in Postgres, so every
// API replica shares one limit per client. The table is UNLOGGED (counters
// may be lost in a crash, which only resets the windows) and holds no tenant
// data, so the API login uses it directly (migration 018).
type RateLimitRepo struct{ db *sql.DB }

func NewRateLimitRepo(db *sql.DB) *RateLimitRepo { return &RateLimitRepo{db: db} }

// Hit counts one request for key in bucket's current window and returns the
// count so far, this one included.
func (r *RateLimitRepo) Hit(ctx context.Context, bucket, key string, window time.Duration) (int, error) {
	secs := int64(window / time.Second)
	if secs < 1 {
		secs = 1
	}
	var n int
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO rate_limits (bucket, key, window_start, hits)
		VALUES ($1, $2, to_timestamp(floor(extract(epoch FROM clock_timestamp()) / $3) * $3), 1)
		ON CONFLICT (bucket, key, window_start) DO UPDATE SET hits = rate_limits.hits + 1
		RETURNING hits`, bucket, key, secs).Scan(&n)
	return n, err
}

// Sweep deletes windows that ended more than an hour ago.
func (r *RateLimitRepo) Sweep(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM rate_limits WHERE window_start < NOW() - INTERVAL '1 hour'`)
	return err
}
