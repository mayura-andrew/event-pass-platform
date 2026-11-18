package postgres

import (
	"context"
	"database/sql"
	"time"
)

// RetentionRepo deletes personal data the platform no longer needs
// (security review F6). It works across every org, so it runs as
// event_pass_system.
type RetentionRepo struct{ db *sql.DB }

func NewRetentionRepo(db *sql.DB) *RetentionRepo { return &RetentionRepo{db: db} }

// ClearBirthDates removes birth dates, the column and the answer to the
// event's birth date question, from events that ended more than after ago
// (an event without an end counts from its start; undated events keep
// theirs). It returns how many registrations it cleared.
func (r *RetentionRepo) ClearBirthDates(ctx context.Context, after time.Duration) (int, error) {
	var n int64
	err := withSystem(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE event_participants p
			SET birth_date = NULL,
			    answers = p.answers - COALESCE((SELECT array_agg(f.key) FROM form_fields f
			                                    WHERE f.event_id = p.event_id AND f.type = 'birth_date'), '{}'::text[]),
			    updated_at = NOW()
			FROM events e
			WHERE e.id = p.event_id AND p.birth_date IS NOT NULL
			  AND COALESCE(e.ends_at, e.starts_at) < NOW() - $1 * interval '1 second'`, after.Seconds())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// ExpireLocations deletes shared map locations older than olderThan, in
// every event (the map shows only the last few minutes anyway).
func (r *RetentionRepo) ExpireLocations(ctx context.Context, olderThan time.Duration) (int, error) {
	var n int64
	err := withSystem(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM participant_locations WHERE updated_at < NOW() - $1 * interval '1 second'`,
			olderThan.Seconds())
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}
