package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// WorkshopRepo stores workshops and sign-ups (migration 011). Each workshop
// has its own attendance checkpoint, created and renamed with it.
type WorkshopRepo struct{ db *sql.DB }

func NewWorkshopRepo(db *sql.DB) *WorkshopRepo { return &WorkshopRepo{db: db} }

// listWorkshops returns the event's workshops with counts; participantID,
// when set, fills MyStatus.
func listWorkshops(ctx context.Context, tx *sql.Tx, orgID, eventID, participantID string) ([]domain.Workshop, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT w.id, w.event_id, w.title, w.description, w.speakers, w.room, w.starts_at, w.ends_at, w.capacity,
		       w.signup_mode, w.waitlist, w.position, COALESCE(c.id::text, ''),
		       (SELECT count(*) FROM workshop_signups s WHERE s.workshop_id = w.id AND s.status = 'signed_up'),
		       (SELECT count(*) FROM workshop_signups s WHERE s.workshop_id = w.id AND s.status = 'waitlisted'),
		       COALESCE((SELECT count(DISTINCT k.participant_id) FROM checkins k WHERE k.checkpoint_id = c.id AND k.undone_at IS NULL), 0),
		       COALESCE((SELECT s.status FROM workshop_signups s WHERE s.workshop_id = w.id AND s.participant_id = NULLIF($3, '')::uuid), '')
		FROM workshops w
		LEFT JOIN checkpoints c ON c.workshop_id = w.id
		WHERE w.org_id = $1 AND w.event_id = $2
		ORDER BY w.starts_at NULLS LAST, w.position, w.title`, orgID, eventID, participantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Workshop{}
	for rows.Next() {
		var w domain.Workshop
		if err := rows.Scan(&w.ID, &w.EventID, &w.Title, &w.Description, &w.Speakers, &w.Room, &w.StartsAt, &w.EndsAt,
			&w.Capacity, &w.SignupMode, &w.Waitlist, &w.Position, &w.CheckpointID, &w.SignedUp, &w.Waitlisted, &w.Attended,
			&w.MyStatus); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *WorkshopRepo) List(ctx context.Context, es authz.EventScope) ([]domain.Workshop, error) {
	var out []domain.Workshop
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		out, err = listWorkshops(ctx, tx, es.OrgID(), es.EventID(), "")
		return err
	})
	return out, err
}

func getWorkshop(ctx context.Context, tx *sql.Tx, orgID, eventID, id, participantID string) (*domain.Workshop, error) {
	all, err := listWorkshops(ctx, tx, orgID, eventID, participantID)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, domain.ErrNotFound
}

func (r *WorkshopRepo) Get(ctx context.Context, es authz.EventScope, id string) (*domain.Workshop, error) {
	var w *domain.Workshop
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		w, err = getWorkshop(ctx, tx, es.OrgID(), es.EventID(), id, "")
		return err
	})
	return w, err
}

// Save creates (ID empty) or updates a workshop and keeps its attendance
// checkpoint's name in step.
func (r *WorkshopRepo) Save(ctx context.Context, es authz.EventScope, w domain.Workshop) (*domain.Workshop, error) {
	var out *domain.Workshop
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		action := "workshop.update"
		if w.ID == "" {
			action, w.ID = "workshop.add", newID()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workshops (id, org_id, event_id, title, description, speakers, room, starts_at, ends_at,
				                       capacity, signup_mode, waitlist, position)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
				        (SELECT COALESCE(MAX(position), 0) + 1 FROM workshops WHERE event_id = $3))`,
				w.ID, es.OrgID(), es.EventID(), w.Title, w.Description, w.Speakers, w.Room, w.StartsAt, w.EndsAt,
				w.Capacity, w.SignupMode, w.Waitlist); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO checkpoints (id, org_id, event_id, workshop_id, name, kind, rules, position)
				VALUES ($1, $2, $3, $4, $5, 'workshop', '{}', (SELECT COALESCE(MAX(position), 0) + 1 FROM checkpoints WHERE event_id = $3))`,
				newID(), es.OrgID(), es.EventID(), w.ID, w.Title); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, `
				UPDATE workshops SET title = $4, description = $5, speakers = $6, room = $7, starts_at = $8, ends_at = $9,
				       capacity = $10, signup_mode = $11, waitlist = $12, updated_at = NOW()
				WHERE org_id = $1 AND event_id = $2 AND id = $3`,
				es.OrgID(), es.EventID(), w.ID, w.Title, w.Description, w.Speakers, w.Room, w.StartsAt, w.EndsAt,
				w.Capacity, w.SignupMode, w.Waitlist)
			if pgCode(err) == pgInvalidTextRepr {
				return domain.ErrNotFound
			}
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.ErrNotFound
			}
			if _, err := tx.ExecContext(ctx, `UPDATE checkpoints SET name = $2, updated_at = NOW() WHERE workshop_id = $1`, w.ID, w.Title); err != nil {
				return err
			}
		}
		var err error
		if out, err = getWorkshop(ctx, tx, es.OrgID(), es.EventID(), w.ID, ""); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: action, TargetType: "workshop", TargetID: w.ID, Diff: map[string]any{"title": w.Title},
		})
	})
	return out, err
}

func (r *WorkshopRepo) Delete(ctx context.Context, es authz.EventScope, id string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var title string
		err := tx.QueryRowContext(ctx, `DELETE FROM workshops WHERE org_id = $1 AND event_id = $2 AND id = $3 RETURNING title`,
			es.OrgID(), es.EventID(), id).Scan(&title)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "workshop.remove", TargetType: "workshop", TargetID: id, Diff: map[string]any{"title": title},
		})
	})
}

// Signups lists a workshop's sign-ups: signed up first, then the waitlist
// in order.
func (r *WorkshopRepo) Signups(ctx context.Context, es authz.EventScope, workshopID string) ([]domain.WorkshopSignup, error) {
	out := []domain.WorkshopSignup{}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT s.workshop_id, s.participant_id, p.full_name, p.email, p.pass_id, s.status, s.created_at
			FROM workshop_signups s JOIN workshops w ON w.id = s.workshop_id JOIN event_participants p ON p.id = s.participant_id
			WHERE s.org_id = $1 AND w.event_id = $2 AND s.workshop_id = $3 AND s.status <> 'cancelled'
			ORDER BY s.status = 'waitlisted', s.position`, es.OrgID(), es.EventID(), workshopID)
		if pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s domain.WorkshopSignup
			if err := rows.Scan(&s.WorkshopID, &s.ParticipantID, &s.FullName, &s.Email, &s.PassID, &s.Status, &s.CreatedAt); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// SignupOptions say who is signing someone up.
type SignupOptions struct {
	ByStaff      bool // staff adding at the door: no time-clash check, never waitlisted, only with room
	ActorAccount string
}

// SignUp signs a participant up for a workshop of their event. The workshop
// row is locked so the last place can't go twice; when it's full the
// participant joins the waitlist (or it's refused without one). A
// participant's own sign-up is refused when it clashes with another
// workshop they're signed up for. Returns the resulting status.
func SignUp(ctx context.Context, tx *sql.Tx, orgID, eventID, workshopID, participantID string, opt SignupOptions) (string, error) {
	var capacity sql.NullInt64
	var waitlist bool
	var mode string
	err := tx.QueryRowContext(ctx, `SELECT capacity, waitlist, signup_mode FROM workshops
		WHERE org_id = $1 AND event_id = $2 AND id = $3 FOR UPDATE`, orgID, eventID, workshopID).Scan(&capacity, &waitlist, &mode)
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return "", domain.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	var pstatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM event_participants WHERE org_id = $1 AND event_id = $2 AND id = $3`,
		orgID, eventID, participantID).Scan(&pstatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return "", domain.ErrNotFound
		}
		return "", err
	}
	if pstatus != domain.PartRegistered {
		return "", fmt.Errorf("%w: only registered participants can sign up for workshops", domain.ErrConflict)
	}
	if !opt.ByStaff {
		if mode == domain.SignupInvite {
			return "", fmt.Errorf("%w: this workshop is by invitation; ask the organizers", domain.ErrForbidden)
		}
		var clash string
		err := tx.QueryRowContext(ctx, `
			SELECT o.title FROM workshops o
			JOIN workshop_signups s ON s.workshop_id = o.id AND s.participant_id = $3 AND s.status = 'signed_up'
			JOIN workshops w ON w.id = $2
			WHERE o.event_id = $1 AND o.id <> w.id AND o.starts_at < w.ends_at AND w.starts_at < o.ends_at
			LIMIT 1`, eventID, workshopID, participantID).Scan(&clash)
		if err == nil {
			return "", fmt.Errorf("%w: this clashes with %s, which you're signed up for", domain.ErrConflict, clash)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT status FROM workshop_signups WHERE workshop_id = $1 AND participant_id = $2`,
		workshopID, participantID).Scan(&existing)
	if err == nil && existing != domain.SignupCancelled && !(opt.ByStaff && existing == domain.SignupWaitlisted) {
		return existing, nil // already signed up or waiting
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	status := domain.SignupSignedUp
	if capacity.Valid {
		var taken int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM workshop_signups WHERE workshop_id = $1 AND status = 'signed_up'`,
			workshopID).Scan(&taken); err != nil {
			return "", err
		}
		if taken >= capacity.Int64 {
			if opt.ByStaff || !waitlist { // staff add at the door only if there is room (spec)
				return "", fmt.Errorf("%w: this workshop is full", domain.ErrConflict)
			}
			status = domain.SignupWaitlisted
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workshop_signups (org_id, workshop_id, participant_id, status, added_by)
		VALUES ($1, $2, $3, $4, NULLIF($5, '')::uuid)
		ON CONFLICT (workshop_id, participant_id) DO UPDATE SET status = EXCLUDED.status, updated_at = NOW(),
		       position = (extract(epoch FROM clock_timestamp()) * 1000000)::bigint, added_by = EXCLUDED.added_by`,
		orgID, workshopID, participantID, status, opt.ActorAccount); err != nil {
		return "", err
	}
	return status, nil
}

// CancelSignup cancels a sign-up and gives the place to the first person on
// the waitlist.
func CancelSignup(ctx context.Context, tx *sql.Tx, orgID, eventID, workshopID, participantID string) error {
	var capacity sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT capacity FROM workshops WHERE org_id = $1 AND event_id = $2 AND id = $3 FOR UPDATE`,
		orgID, eventID, workshopID).Scan(&capacity)
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return domain.ErrNotFound
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE workshop_signups SET status = 'cancelled', updated_at = NOW()
		WHERE workshop_id = $1 AND participant_id = $2 AND status <> 'cancelled'`, workshopID, participantID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	// Promote as many as there are free places (usually one).
	_, err = tx.ExecContext(ctx, `
		UPDATE workshop_signups SET status = 'signed_up', updated_at = NOW()
		WHERE workshop_id = $1 AND participant_id IN (
			SELECT participant_id FROM workshop_signups WHERE workshop_id = $1 AND status = 'waitlisted'
			ORDER BY position
			LIMIT CASE WHEN $2::int IS NULL THEN 1000000
			           ELSE GREATEST($2::int - (SELECT count(*) FROM workshop_signups WHERE workshop_id = $1 AND status = 'signed_up'), 0) END)`,
		workshopID, capacity)
	return err
}

// StaffSignUp signs a participant up as staff (door or organizer).
func (r *WorkshopRepo) StaffSignUp(ctx context.Context, es authz.EventScope, workshopID, participantID string) (string, error) {
	var status string
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		if status, err = SignUp(ctx, tx, es.OrgID(), es.EventID(), workshopID, participantID,
			SignupOptions{ByStaff: true, ActorAccount: es.AccountID()}); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "workshop.signup.add", TargetType: "signup", TargetID: participantID,
			Diff: map[string]any{"workshop": workshopID, "status": status},
		})
	})
	return status, err
}

func (r *WorkshopRepo) StaffCancel(ctx context.Context, es authz.EventScope, workshopID, participantID string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		if err := CancelSignup(ctx, tx, es.OrgID(), es.EventID(), workshopID, participantID); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "workshop.signup.cancel", TargetType: "signup", TargetID: participantID,
			Diff: map[string]any{"workshop": workshopID},
		})
	})
}

// SignupStatus is the participant's status for one workshop ("" if none).
func (r *WorkshopRepo) SignupStatus(ctx context.Context, es authz.EventScope, workshopID, participantID string) (string, error) {
	var s string
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT status FROM workshop_signups WHERE workshop_id = $1 AND participant_id = $2`,
			workshopID, participantID).Scan(&s)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return s, err
}

// ---- Portal: the participant's own sign-ups ------------------------------------

// MyWorkshops lists the workshops of a participant's event with their own
// status. The caller has already established that participantID is theirs.
func (r *WorkshopRepo) MyWorkshops(ctx context.Context, orgID, eventID, accountID, participantID string) ([]domain.Workshop, error) {
	var out []domain.Workshop
	err := withTenant(ctx, r.db, orgID, accountID, func(tx *sql.Tx) error {
		var err error
		out, err = listWorkshops(ctx, tx, orgID, eventID, participantID)
		return err
	})
	return out, err
}

// MySignUp and MyCancel are the participant's own actions.
func (r *WorkshopRepo) MySignUp(ctx context.Context, orgID, eventID, accountID, workshopID, participantID string) (string, error) {
	var status string
	err := withTenant(ctx, r.db, orgID, accountID, func(tx *sql.Tx) error {
		var err error
		status, err = SignUp(ctx, tx, orgID, eventID, workshopID, participantID, SignupOptions{})
		return err
	})
	return status, err
}

func (r *WorkshopRepo) MyCancel(ctx context.Context, orgID, eventID, accountID, workshopID, participantID string) error {
	return withTenant(ctx, r.db, orgID, accountID, func(tx *sql.Tx) error {
		return CancelSignup(ctx, tx, orgID, eventID, workshopID, participantID)
	})
}
