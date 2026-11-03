package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// OutboxRepo is the worker's view of the email outbox. The worker serves
// every org, so it runs as the connecting user (outside Row Level Security)
// and only ever touches rows it has claimed.
type OutboxRepo struct{ db *sql.DB }

func NewOutboxRepo(db *sql.DB) *OutboxRepo { return &OutboxRepo{db: db} }

// Claim leases up to limit due emails. A row stays "sending" until marked;
// if the worker dies, the lease runs out and another claim takes it.
func (r *OutboxRepo) Claim(ctx context.Context, limit int, lease time.Duration) ([]domain.OutboxJob, error) {
	rows, err := r.db.QueryContext(ctx, `
		UPDATE outbox o SET status = 'sending', locked_until = NOW() + $2::interval, attempts = o.attempts + 1
		FROM (SELECT id FROM outbox
		      WHERE (status = 'queued' AND next_attempt_at <= NOW())
		         OR (status = 'sending' AND locked_until < NOW())
		      ORDER BY next_attempt_at
		      LIMIT $1
		      FOR UPDATE SKIP LOCKED) due
		WHERE o.id = due.id
		RETURNING o.id, o.org_id, COALESCE(o.participant_id::text, ''), o.to_email, o.template_key, o.attempts`,
		limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OutboxJob
	for rows.Next() {
		var j domain.OutboxJob
		if err := rows.Scan(&j.ID, &j.OrgID, &j.ParticipantID, &j.ToEmail, &j.Template, &j.Attempts); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *OutboxRepo) MarkSent(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE outbox SET status = 'sent', sent_at = NOW(), locked_until = NULL, last_error = NULL WHERE id = $1`, id)
	return err
}

// MarkFailed records an error; with retryAt the email goes back in the
// queue, without it the email is given up.
func (r *OutboxRepo) MarkFailed(ctx context.Context, id, msg string, retryAt *time.Time) error {
	if retryAt != nil {
		_, err := r.db.ExecContext(ctx, `UPDATE outbox SET status = 'queued', next_attempt_at = $2, locked_until = NULL, last_error = $3 WHERE id = $1`,
			id, *retryAt, msg)
		return err
	}
	_, err := r.db.ExecContext(ctx, `UPDATE outbox SET status = 'failed', locked_until = NULL, last_error = $2 WHERE id = $1`, id, msg)
	return err
}

// PassContext loads a participant with their event, org and pass-visible
// questions. System path, for the worker and for pass downloads after the
// caller's access was checked.
func (r *OutboxRepo) PassContext(ctx context.Context, orgID, participantID string) (*domain.PassContext, error) {
	return passContext(ctx, r.db, orgID, participantID)
}

func passContext(ctx context.Context, db *sql.DB, orgID, participantID string) (*domain.PassContext, error) {
	pc := &domain.PassContext{}
	var venue, settings []byte
	dest := append(eventDest(&pc.Event, &venue, &settings), &pc.OrgName, &pc.OrgSlug)
	p, err := scanParticipant(db.QueryRowContext(ctx, `SELECT `+participantCols+` FROM event_participants p
		WHERE p.org_id = $1 AND p.id = $2`, orgID, participantID))
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	pc.Participant = p
	err = db.QueryRowContext(ctx, `SELECT `+eventCols+`, o.name, o.slug`+eventFrom+`
		JOIN orgs o ON o.id = e.org_id
		JOIN event_participants p ON p.event_id = e.id AND p.id = $2
		WHERE e.org_id = $1`, orgID, participantID).Scan(dest...)
	if err != nil {
		return nil, err
	}
	if err := decodeEvent(&pc.Event, venue, settings); err != nil {
		return nil, err
	}
	err = withTenant(ctx, db, orgID, "", func(tx *sql.Tx) error {
		var err error
		if pc.Fields, err = listFormFields(ctx, tx, orgID, pc.Event.ID); err != nil {
			return err
		}
		if pc.Event.PassTemplateID == nil {
			return nil
		}
		var design []byte
		err = tx.QueryRowContext(ctx, `SELECT design FROM pass_template_versions WHERE template_id = $1 AND version = $2`,
			*pc.Event.PassTemplateID, *pc.Event.PassTemplateVersion).Scan(&design)
		pc.Design = design
		return err
	})
	return pc, err
}
