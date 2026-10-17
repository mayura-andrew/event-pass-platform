package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// EmailRepo is the pass email queue (migrations/006_email_queue_schema.sql).
type EmailRepo struct{ db *sql.DB }

func NewEmailRepo(db *sql.DB) *EmailRepo { return &EmailRepo{db: db} }

func truncateError(msg string) string {
	if utf8.RuneCountInString(msg) <= domain.MaxEmailErrorLen {
		return msg
	}
	r := []rune(msg)
	return string(r[:domain.MaxEmailErrorLen-1]) + "…"
}

// ---- Worker side -----------------------------------------------------------------

// FetchNextJob claims the oldest due pending job (status -> sending, attempts
// + 1) and returns it with the delegate's current details, or nil when none
// is due. SKIP LOCKED lets several workers claim different jobs without
// waiting on each other or taking the same one.
func (r *EmailRepo) FetchNextJob(ctx context.Context) (*domain.EmailJob, error) {
	var (
		j                 domain.EmailJob
		venue, hall, note sql.NullString
		team              sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		WITH next AS (
			SELECT id FROM email_jobs
			WHERE status = 'pending' AND scheduled_at <= NOW()
			ORDER BY scheduled_at, created_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		), claimed AS (
			UPDATE email_jobs e
			SET status = 'sending', attempts = e.attempts + 1, locked_at = NOW(), updated_at = NOW()
			FROM next
			WHERE e.id = next.id
			RETURNING e.id, e.participant_id, e.recipient_email, e.attempts, e.max_attempts
		)
		SELECT c.id, c.participant_id, c.recipient_email, c.attempts, c.max_attempts,
		       p.full_name, p.membership_id, p.email, p.team_name, p.qr_signature, p.is_confirmed,
		       v.name, v.hall_section, v.description
		FROM claimed c
		JOIN participants p ON p.id = c.participant_id
		LEFT JOIN venues v ON v.id = p.assigned_venue_id`,
	).Scan(&j.ID, &j.ParticipantID, &j.RecipientEmail, &j.Attempts, &j.MaxAttempts,
		&j.FullName, &j.MembershipID, &j.Email, &team, &j.QRSignature, &j.IsConfirmed,
		&venue, &hall, &note)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetch email job: %w", err)
	}
	j.TeamName, j.VenueName, j.HallSection, j.VenueNote = team.String, venue.String, hall.String, note.String
	return &j, nil
}

// MarkSent records that the mail server accepted the message.
func (r *EmailRepo) MarkSent(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE email_jobs
		SET status = 'sent', sent_at = NOW(), locked_at = NULL, last_error = NULL, updated_at = NOW()
		WHERE id = $1 AND status = 'sending'`, id)
	if err != nil {
		return fmt.Errorf("mark email sent: %w", err)
	}
	return nil
}

// MarkFailed records a failed attempt. A permanent failure, or one that used
// the last attempt, ends the job as failed; otherwise it goes back to pending
// and is retried after retryIn. Returns the job's new status.
func (r *EmailRepo) MarkFailed(ctx context.Context, id, errMsg string, permanent bool, retryIn time.Duration) (string, error) {
	var status string
	err := r.db.QueryRowContext(ctx, `
		UPDATE email_jobs
		SET status       = CASE WHEN $3 OR attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		    scheduled_at = CASE WHEN $3 OR attempts >= max_attempts THEN scheduled_at
		                        ELSE NOW() + make_interval(secs => $4) END,
		    last_error = $2, locked_at = NULL, updated_at = NOW()
		WHERE id = $1 AND status = 'sending'
		RETURNING status`, id, truncateError(errMsg), permanent, retryIn.Seconds()).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // reclaimed meanwhile; nothing to record
	}
	if err != nil {
		return "", fmt.Errorf("mark email failed: %w", err)
	}
	return status, nil
}

// Release puts a claimed job back without counting the attempt: the failure
// was the sender's (wrong password, daily cap), not this recipient's.
func (r *EmailRepo) Release(ctx context.Context, id, errMsg string, delay time.Duration) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE email_jobs
		SET status = 'pending', attempts = GREATEST(attempts - 1, 0), locked_at = NULL,
		    scheduled_at = NOW() + make_interval(secs => $3), last_error = $2, updated_at = NOW()
		WHERE id = $1 AND status = 'sending'`, id, truncateError(errMsg), delay.Seconds())
	if err != nil {
		return fmt.Errorf("release email job: %w", err)
	}
	return nil
}

// Cancel ends a claimed job without sending (the delegate is no longer
// confirmed).
func (r *EmailRepo) Cancel(ctx context.Context, id, reason string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE email_jobs
		SET status = 'cancelled', last_error = $2, locked_at = NULL, updated_at = NOW()
		WHERE id = $1 AND status = 'sending'`, id, truncateError(reason))
	if err != nil {
		return fmt.Errorf("cancel email job: %w", err)
	}
	return nil
}

// ReclaimStale returns jobs stuck in "sending" (the process stopped mid-send)
// to pending, or to failed when that was their last attempt. The message may
// or may not have gone out; sending it again is the lesser evil.
func (r *EmailRepo) ReclaimStale(ctx context.Context, olderThan time.Duration) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		UPDATE email_jobs
		SET status = CASE WHEN attempts >= max_attempts THEN 'failed' ELSE 'pending' END,
		    last_error = 'interrupted while sending (server restarted); it may already have been delivered',
		    locked_at = NULL, scheduled_at = NOW(), updated_at = NOW()
		WHERE status = 'sending' AND locked_at < NOW() - make_interval(secs => $1)`, olderThan.Seconds())
	if err != nil {
		return 0, fmt.Errorf("reclaim email jobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SentSince counts messages sent after t, for the daily cap.
func (r *EmailRepo) SentSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `
		SELECT count(*) FROM email_jobs WHERE status = 'sent' AND sent_at > $1`, t).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count sent emails: %w", err)
	}
	return n, nil
}

// OldestSentSince returns when the oldest message sent after t went out, so
// the worker knows when the 24-hour window frees a slot.
func (r *EmailRepo) OldestSentSince(ctx context.Context, t time.Time) (time.Time, error) {
	var at sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT min(sent_at) FROM email_jobs WHERE status = 'sent' AND sent_at > $1`, t).Scan(&at)
	if err != nil {
		return time.Time{}, fmt.Errorf("oldest sent email: %w", err)
	}
	return at.Time, nil
}

// ---- Pause switch ---------------------------------------------------------------

func (r *EmailRepo) State(ctx context.Context) (domain.EmailQueueState, error) {
	var (
		s      domain.EmailQueueState
		reason sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		SELECT paused, pause_reason, updated_at FROM email_queue_state WHERE id`).Scan(&s.Paused, &reason, &s.UpdatedAt)
	if err != nil {
		return s, fmt.Errorf("read email queue state: %w", err)
	}
	s.PauseReason = reason.String
	return s, nil
}

// SetPaused stores the switch. reason is empty when an organizer flips it,
// and says why when the worker pauses itself. actorID may be empty.
func (r *EmailRepo) SetPaused(ctx context.Context, paused bool, reason, actorID string) (domain.EmailQueueState, error) {
	var (
		s   domain.EmailQueueState
		rsn sql.NullString
	)
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO email_queue_state (id, paused, pause_reason, updated_by, updated_at)
		VALUES (true, $1, NULLIF($2, ''), $3, NOW())
		ON CONFLICT (id) DO UPDATE SET
			paused = EXCLUDED.paused, pause_reason = EXCLUDED.pause_reason,
			updated_by = EXCLUDED.updated_by, updated_at = NOW()
		RETURNING paused, pause_reason, updated_at`,
		paused, truncateError(reason), nullIfEmpty(actorID)).Scan(&s.Paused, &rsn, &s.UpdatedAt)
	if err != nil {
		return s, fmt.Errorf("set email queue state: %w", err)
	}
	s.PauseReason = rsn.String
	return s, nil
}

// ---- Enqueueing ------------------------------------------------------------------

// EnqueueSingle queues a pass email for one delegate, found by participant
// ID or, when that is empty, by membership ID. It may be someone who was
// emailed before (a resend); it may not be someone already queued.
func (r *EmailRepo) EnqueueSingle(ctx context.Context, participantID, membershipID, actorID string) (*domain.EmailEnqueueResult, error) {
	var (
		id, email, name, memberID string
		confirmed                 bool
	)
	q := `SELECT id, email, full_name, membership_id, is_confirmed FROM participants WHERE `
	arg := participantID
	if participantID != "" {
		q += `id = $1::uuid`
	} else {
		q += `membership_id = $1`
		arg = membershipID
	}
	err := r.db.QueryRowContext(ctx, q, arg).Scan(&id, &email, &name, &memberID, &confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find delegate: %w", err)
	}
	if !confirmed {
		return nil, fmt.Errorf("%w: %s (%s) is not confirmed; only confirmed delegates get a pass", domain.ErrValidation, name, memberID)
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO email_jobs (participant_id, recipient_email, kind, created_by)
		VALUES ($1, $2, 'pass_confirmation', $3)
		ON CONFLICT (participant_id, kind) WHERE status IN ('pending', 'sending') DO NOTHING`,
		id, email, nullIfEmpty(actorID))
	if err != nil {
		return nil, fmt.Errorf("queue email: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("%w: a pass email to %s (%s) is already waiting to be sent", domain.ErrConflict, name, memberID)
	}
	return &domain.EmailEnqueueResult{Queued: 1, Message: fmt.Sprintf("Pass email for %s (%s) queued to %s.", name, memberID, email)}, nil
}

// EnqueueBulk queues a pass email for every confirmed delegate who has no
// sent or queued one (those whose earlier attempts failed are included), in
// membership ID order. Returns how many were queued.
func (r *EmailRepo) EnqueueBulk(ctx context.Context, actorID string) (int, error) {
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO email_jobs (participant_id, recipient_email, kind, created_by, scheduled_at)
		SELECT p.id, p.email, 'pass_confirmation', $1,
		       -- Keep the order stable: one microsecond apart in ID order.
		       NOW() + (row_number() OVER (ORDER BY p.membership_id)) * INTERVAL '1 microsecond'
		FROM participants p
		WHERE p.is_confirmed
		  AND NOT EXISTS (
			SELECT 1 FROM email_jobs j
			WHERE j.participant_id = p.id AND j.kind = 'pass_confirmation'
			  AND j.status IN ('pending', 'sending', 'sent'))
		ON CONFLICT (participant_id, kind) WHERE status IN ('pending', 'sending') DO NOTHING`,
		nullIfEmpty(actorID))
	if err != nil {
		return 0, fmt.Errorf("queue emails: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---- Dashboard ----------------------------------------------------------------------

// Stats counts jobs by status and lists recent failures. The worker-side
// fields (enabled, interval, estimate) are filled in by the service.
func (r *EmailRepo) Stats(ctx context.Context, since time.Time) (*domain.EmailQueueStats, error) {
	s := &domain.EmailQueueStats{RecentFailures: []domain.EmailFailure{}}
	var lastSent sql.NullTime
	err := r.db.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'pending'),
			count(*) FILTER (WHERE status = 'pending' AND attempts > 0),
			count(*) FILTER (WHERE status = 'sending'),
			count(*) FILTER (WHERE status = 'sent'),
			count(*) FILTER (WHERE status = 'failed'),
			count(*) FILTER (WHERE status = 'cancelled'),
			count(*) FILTER (WHERE status = 'sent' AND sent_at > $1),
			max(sent_at),
			(SELECT count(*) FROM participants WHERE is_confirmed),
			(SELECT count(*) FROM participants p
			 WHERE p.is_confirmed AND NOT EXISTS (
				SELECT 1 FROM email_jobs j
				WHERE j.participant_id = p.id AND j.kind = 'pass_confirmation'
				  AND j.status IN ('pending', 'sending', 'sent')))
		FROM email_jobs`, since).Scan(
		&s.Pending, &s.Retrying, &s.Sending, &s.Sent, &s.Failed, &s.Cancelled,
		&s.SentLast24h, &lastSent, &s.ConfirmedDelegates, &s.NotYetEmailed)
	if err != nil {
		return nil, fmt.Errorf("email stats: %w", err)
	}
	if lastSent.Valid {
		t := lastSent.Time
		s.LastSentAt = &t
	}

	// A delegate whose email later went through drops off the failure list.
	rows, err := r.db.QueryContext(ctx, `
		SELECT j.id, j.participant_id, p.membership_id, p.full_name, j.recipient_email,
		       j.attempts, COALESCE(j.last_error, ''), j.updated_at
		FROM email_jobs j
		JOIN participants p ON p.id = j.participant_id
		WHERE j.status = 'failed'
		  AND NOT EXISTS (
			SELECT 1 FROM email_jobs k
			WHERE k.participant_id = j.participant_id AND k.kind = j.kind
			  AND k.status IN ('pending', 'sending', 'sent') AND k.created_at > j.created_at)
		ORDER BY j.updated_at DESC
		LIMIT $1`, domain.EmailRecentFailures)
	if err != nil {
		return nil, fmt.Errorf("email failures: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f domain.EmailFailure
		if err := rows.Scan(&f.JobID, &f.ParticipantID, &f.MembershipID, &f.FullName, &f.RecipientEmail,
			&f.Attempts, &f.LastError, &f.UpdatedAt); err != nil {
			return nil, err
		}
		s.RecentFailures = append(s.RecentFailures, f)
	}
	return s, rows.Err()
}
