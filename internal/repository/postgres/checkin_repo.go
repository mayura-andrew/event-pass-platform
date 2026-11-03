package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// CheckinRepo stores checkpoints and check-ins (migration 011).
type CheckinRepo struct{ db *sql.DB }

func NewCheckinRepo(db *sql.DB) *CheckinRepo { return &CheckinRepo{db: db} }

// ---- Checkpoints -------------------------------------------------------------

const checkpointCols = `c.id, c.event_id, COALESCE(c.workshop_id::text, ''), c.name, c.kind, c.rules, c.position, c.archived_at,
	(SELECT count(DISTINCT k.participant_id) FROM checkins k WHERE k.checkpoint_id = c.id AND k.undone_at IS NULL)`

func scanCheckpoint(row interface{ Scan(...any) error }) (domain.Checkpoint, error) {
	var c domain.Checkpoint
	var rules []byte
	err := row.Scan(&c.ID, &c.EventID, &c.WorkshopID, &c.Name, &c.Kind, &rules, &c.Position, &c.ArchivedAt, &c.Recorded)
	if err == nil {
		err = json.Unmarshal(rules, &c.Rules)
	}
	return c, err
}

func listCheckpoints(ctx context.Context, tx *sql.Tx, orgID, eventID string, archived bool) ([]domain.Checkpoint, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+checkpointCols+` FROM checkpoints c
		WHERE c.org_id = $1 AND c.event_id = $2 AND ($3 OR c.archived_at IS NULL)
		ORDER BY c.archived_at IS NOT NULL, c.position, c.created_at`, orgID, eventID, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Checkpoint{}
	for rows.Next() {
		c, err := scanCheckpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Checkpoints lists the event's checkpoints; archived ones last, if asked.
func (r *CheckinRepo) Checkpoints(ctx context.Context, es authz.EventScope, archived bool) ([]domain.Checkpoint, error) {
	var out []domain.Checkpoint
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		out, err = listCheckpoints(ctx, tx, es.OrgID(), es.EventID(), archived)
		return err
	})
	return out, err
}

func (r *CheckinRepo) Checkpoint(ctx context.Context, es authz.EventScope, id string) (*domain.Checkpoint, error) {
	var c domain.Checkpoint
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		c, err = scanCheckpoint(tx.QueryRowContext(ctx, `SELECT `+checkpointCols+` FROM checkpoints c
			WHERE c.org_id = $1 AND c.event_id = $2 AND c.id = $3`, es.OrgID(), es.EventID(), id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveCheckpoint creates a custom checkpoint (ID empty) or updates the name
// and rules of any checkpoint.
func (r *CheckinRepo) SaveCheckpoint(ctx context.Context, es authz.EventScope, c domain.Checkpoint) (*domain.Checkpoint, error) {
	rules, _ := json.Marshal(c.Rules)
	var out domain.Checkpoint
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		action := "checkpoint.update"
		if c.ID == "" {
			action, c.ID = "checkpoint.add", newID()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO checkpoints (id, org_id, event_id, name, kind, rules, position)
				VALUES ($1, $2, $3, $4, 'custom', $5, (SELECT COALESCE(MAX(position), 0) + 1 FROM checkpoints WHERE event_id = $3))`,
				c.ID, es.OrgID(), es.EventID(), c.Name, rules); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, `UPDATE checkpoints SET name = $4, rules = $5, updated_at = NOW()
				WHERE org_id = $1 AND event_id = $2 AND id = $3`, es.OrgID(), es.EventID(), c.ID, c.Name, rules)
			if pgCode(err) == pgInvalidTextRepr {
				return domain.ErrNotFound
			}
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.ErrNotFound
			}
		}
		var err error
		if out, err = scanCheckpoint(tx.QueryRowContext(ctx, `SELECT `+checkpointCols+` FROM checkpoints c WHERE c.id = $1`, c.ID)); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: action, TargetType: "checkpoint", TargetID: c.ID, Diff: map[string]any{"name": c.Name},
		})
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// SetCheckpointArchived hides a checkpoint from scanners (or brings it back).
// Its check-ins stay.
func (r *CheckinRepo) SetCheckpointArchived(ctx context.Context, es authz.EventScope, id string, archived bool) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var name string
		err := tx.QueryRowContext(ctx, `
			UPDATE checkpoints SET archived_at = CASE WHEN $4 THEN NOW() ELSE NULL END, updated_at = NOW()
			WHERE org_id = $1 AND event_id = $2 AND id = $3 RETURNING name`, es.OrgID(), es.EventID(), id, archived).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		action := "checkpoint.restore"
		if archived {
			action = "checkpoint.archive"
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: action, TargetType: "checkpoint", TargetID: id, Diff: map[string]any{"name": name},
		})
	})
}

// ---- Check-ins ---------------------------------------------------------------

const checkinCols = `k.id, k.participant_id, k.checkpoint_id, COALESCE(k.staff_account_id::text, ''),
	COALESCE(NULLIF(a.name, ''), a.email, 'Staff'), k.device_id, k.created_at, k.undone_at`

const checkinFrom = ` FROM checkins k LEFT JOIN accounts a ON a.id = k.staff_account_id`

func scanCheckin(row interface{ Scan(...any) error }) (domain.Checkin, error) {
	var c domain.Checkin
	err := row.Scan(&c.ID, &c.ParticipantID, &c.CheckpointID, &c.StaffID, &c.StaffName, &c.DeviceID, &c.CreatedAt, &c.UndoneAt)
	return c, err
}

// ParticipantCheckins returns the participant's active check-ins, newest
// first.
func (r *CheckinRepo) ParticipantCheckins(ctx context.Context, es authz.EventScope, participantID string) ([]domain.Checkin, error) {
	out := []domain.Checkin{}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+checkinCols+checkinFrom+`
			WHERE k.org_id = $1 AND k.event_id = $2 AND k.participant_id = $3 AND k.undone_at IS NULL
			ORDER BY k.created_at DESC`, es.OrgID(), es.EventID(), participantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCheckin(rows)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// Record writes a check-in. A retry with the same idempotency key returns
// the first check-in (replay). For a once-only checkpoint that someone else
// already recorded, duplicate is that earlier check-in and nothing is
// written: the partial unique index decides, so two phones at once can't
// both record it.
func (r *CheckinRepo) Record(ctx context.Context, es authz.EventScope, in domain.RecordInput) (rec *domain.Checkin, duplicate *domain.Checkin, replay bool, err error) {
	err = withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		byKey := func() (*domain.Checkin, error) {
			c, err := scanCheckin(tx.QueryRowContext(ctx, `SELECT `+checkinCols+checkinFrom+` WHERE k.event_id = $1 AND k.idempotency_key = $2`,
				es.EventID(), in.IdempotencyKey))
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return &c, err
		}
		if prev, err := byKey(); err != nil || prev != nil {
			rec, replay = prev, prev != nil
			return err
		}
		id := newID()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO checkins (id, org_id, event_id, participant_id, checkpoint_id, idempotency_key,
			                      staff_account_id, device_id, client_time, once)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			ON CONFLICT DO NOTHING`,
			id, es.OrgID(), es.EventID(), in.ParticipantID, in.CheckpointID, in.IdempotencyKey,
			es.AccountID(), in.DeviceID, in.ClientTime, in.Once)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			c, err := scanCheckin(tx.QueryRowContext(ctx, `SELECT `+checkinCols+checkinFrom+` WHERE k.id = $1`, id))
			rec = &c
			return err
		}
		// Not written: the same key from a concurrent retry, or a duplicate.
		if prev, err := byKey(); err != nil || prev != nil {
			rec, replay = prev, prev != nil
			return err
		}
		c, err := scanCheckin(tx.QueryRowContext(ctx, `SELECT `+checkinCols+checkinFrom+`
			WHERE k.checkpoint_id = $1 AND k.participant_id = $2 AND k.once AND k.undone_at IS NULL`,
			in.CheckpointID, in.ParticipantID))
		if err != nil {
			return err
		}
		duplicate = &c
		return nil
	})
	return rec, duplicate, replay, err
}

func (r *CheckinRepo) GetCheckin(ctx context.Context, es authz.EventScope, id string) (*domain.Checkin, error) {
	var c domain.Checkin
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		c, err = scanCheckin(tx.QueryRowContext(ctx, `SELECT `+checkinCols+checkinFrom+`
			WHERE k.org_id = $1 AND k.event_id = $2 AND k.id = $3`, es.OrgID(), es.EventID(), id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Undo marks a check-in undone (it stays in the table). Already undone is a
// conflict.
func (r *CheckinRepo) Undo(ctx context.Context, es authz.EventScope, id string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var participant, checkpoint string
		err := tx.QueryRowContext(ctx, `
			UPDATE checkins k SET undone_at = NOW(), undone_by = $4
			WHERE k.org_id = $1 AND k.event_id = $2 AND k.id = $3 AND k.undone_at IS NULL
			RETURNING (SELECT full_name FROM event_participants WHERE id = k.participant_id),
			          (SELECT name FROM checkpoints WHERE id = k.checkpoint_id)`,
			es.OrgID(), es.EventID(), id, es.AccountID()).Scan(&participant, &checkpoint)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "checkin.undo", TargetType: "checkin", TargetID: id,
			Diff: map[string]any{"participant": participant, "checkpoint": checkpoint},
		})
	})
}

// Recent returns the latest check-ins of the event, newest first.
func (r *CheckinRepo) Recent(ctx context.Context, es authz.EventScope, limit int) ([]domain.CheckinRow, error) {
	out := []domain.CheckinRow{}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+checkinCols+`, p.full_name, p.pass_id, c.name`+checkinFrom+`
			JOIN event_participants p ON p.id = k.participant_id
			JOIN checkpoints c ON c.id = k.checkpoint_id
			WHERE k.org_id = $1 AND k.event_id = $2
			ORDER BY k.created_at DESC LIMIT $3`, es.OrgID(), es.EventID(), limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row domain.CheckinRow
			c := &row.Checkin
			if err := rows.Scan(&c.ID, &c.ParticipantID, &c.CheckpointID, &c.StaffID, &c.StaffName, &c.DeviceID,
				&c.CreatedAt, &c.UndoneAt, &row.FullName, &row.PassID, &row.Checkpoint); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

// Stats are the live dashboard's numbers.
func (r *CheckinRepo) Stats(ctx context.Context, es authz.EventScope, now time.Time) (*domain.EventStats, error) {
	st := &domain.EventStats{Participants: domain.ParticipantCounts{}, At: now}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT status, count(*) FROM event_participants
			WHERE org_id = $1 AND event_id = $2 GROUP BY status`, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			var n int
			if err := rows.Scan(&s, &n); err != nil {
				rows.Close()
				return err
			}
			st.Participants[s] = n
		}
		rows.Close()
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM form_fields WHERE org_id = $1 AND event_id = $2 AND type = 'birth_date' AND workshop_id IS NULL),
			(SELECT count(*) FROM event_participants p WHERE p.org_id = $1 AND p.event_id = $2 AND p.status = 'registered' AND `+participantIsMinor+`)`,
			es.OrgID(), es.EventID()).Scan(&st.AsksBirthDate, &st.Minors); err != nil {
			return err
		}
		if st.Checkpoints, err = listCheckpoints(ctx, tx, es.OrgID(), es.EventID(), false); err != nil {
			return err
		}
		for _, c := range st.Checkpoints {
			if c.Kind == domain.KindCheckIn {
				st.CheckedIn += c.Recorded
			}
		}
		if st.Workshops, err = listWorkshops(ctx, tx, es.OrgID(), es.EventID(), ""); err != nil {
			return err
		}
		if st.PerTenMinutes, err = buckets(ctx, tx, `
			SELECT to_timestamp(floor(extract(epoch FROM created_at) / 600) * 600) AS b, count(*)
			FROM checkins WHERE org_id = $1 AND event_id = $2 AND undone_at IS NULL AND created_at > $3
			GROUP BY b ORDER BY b`, es.OrgID(), es.EventID(), now.Add(-12*time.Hour)); err != nil {
			return err
		}
		st.RegistrationsPerDay, err = buckets(ctx, tx, `
			SELECT date_trunc('day', registered_at) AS b, count(*)
			FROM event_participants WHERE org_id = $1 AND event_id = $2 AND registered_at > $3
			GROUP BY b ORDER BY b`, es.OrgID(), es.EventID(), now.Add(-30*24*time.Hour))
		return err
	})
	return st, err
}

func buckets(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]domain.Bucket, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Bucket{}
	for rows.Next() {
		var b domain.Bucket
		if err := rows.Scan(&b.Start, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ActiveEvents returns the events with check-ins, undos or registrations
// since the given time: the live feed's poll. System path: one query serves
// every connected dashboard.
func (r *CheckinRepo) ActiveEvents(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT event_id FROM checkins WHERE created_at > $1 OR undone_at > $1
		UNION
		SELECT event_id FROM event_participants WHERE updated_at > $1 OR registered_at > $1
		UNION
		SELECT w.event_id FROM workshop_signups s JOIN workshops w ON w.id = s.workshop_id WHERE s.updated_at > $1`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
