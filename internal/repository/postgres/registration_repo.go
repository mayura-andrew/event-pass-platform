package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// RegistrationRepo stores registration forms and event participants
// (migration 009).
type RegistrationRepo struct{ db *sql.DB }

func NewRegistrationRepo(db *sql.DB) *RegistrationRepo { return &RegistrationRepo{db: db} }

func (r *RegistrationRepo) CheckParticipant(ctx context.Context, orgID, accountID, eventID, participantID string) (*domain.EventParticipant, error) {
	var p *domain.EventParticipant
	err := withTenant(ctx, r.db, orgID, accountID, func(tx *sql.Tx) error {
		participant, err := scanParticipant(tx.QueryRowContext(ctx, `SELECT `+participantCols+`
			FROM event_participants
			WHERE org_id = $1 AND event_id = $2 AND id = $3`, orgID, eventID, participantID))
		p = &participant
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	return p, err
}

// ---- Form fields -------------------------------------------------------------

func listFormFields(ctx context.Context, tx *sql.Tx, orgID, eventID string) ([]domain.FormField, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, key, type, config, position FROM form_fields
		WHERE org_id = $1 AND event_id = $2 AND workshop_id IS NULL
		ORDER BY position, created_at`, orgID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.FormField{}
	for rows.Next() {
		var f domain.FormField
		var cfg []byte
		if err := rows.Scan(&f.ID, &f.Key, &f.Type, &cfg, &f.Position); err != nil {
			return nil, err
		}
		var c domain.FieldConfig
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, err
		}
		f.Label, f.Help, f.Required, f.Options, f.Visibility = c.Label, c.Help, c.Required, c.Options, c.Visibility
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *RegistrationRepo) ListFields(ctx context.Context, es authz.EventScope) ([]domain.FormField, error) {
	var out []domain.FormField
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		out, err = listFormFields(ctx, tx, es.OrgID(), es.EventID())
		return err
	})
	return out, err
}

func fieldConfig(f domain.FormField) []byte {
	b, _ := json.Marshal(domain.FieldConfig{Label: f.Label, Help: f.Help, Required: f.Required,
		Options: f.Options, Visibility: f.Visibility})
	return b
}

// SaveField creates f (ID empty, added last) or updates it. A key used by
// another field of the event is a conflict.
func (r *RegistrationRepo) SaveField(ctx context.Context, es authz.EventScope, f domain.FormField) (*domain.FormField, error) {
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		action := "form.field.update"
		if f.ID == "" {
			action, f.ID = "form.field.add", newID()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO form_fields (id, org_id, event_id, key, type, config, position)
				VALUES ($1, $2, $3, $4, $5, $6,
				        (SELECT COALESCE(MAX(position), 0) + 1 FROM form_fields WHERE event_id = $3 AND workshop_id IS NULL))`,
				f.ID, es.OrgID(), es.EventID(), f.Key, f.Type, fieldConfig(f)); err != nil {
				if pgCode(err) == pgUniqueViolation && strings.Contains(pgConstraint(err), "birth_date") {
					return errOneBirthDate
				}
				if pgCode(err) == pgUniqueViolation {
					return fmt.Errorf("%w: another question already uses the key %q", domain.ErrConflict, f.Key)
				}
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, `
				UPDATE form_fields SET type = $4, config = $5, updated_at = NOW()
				WHERE org_id = $1 AND event_id = $2 AND id = $3`,
				es.OrgID(), es.EventID(), f.ID, f.Type, fieldConfig(f))
			if pgCode(err) == pgInvalidTextRepr {
				return domain.ErrNotFound
			}
			if pgCode(err) == pgUniqueViolation && strings.Contains(pgConstraint(err), "birth_date") {
				return errOneBirthDate
			}
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return domain.ErrNotFound
			}
		}
		if err := syncBirthDates(ctx, tx, es.OrgID(), es.EventID()); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: action, TargetType: "field", TargetID: f.ID,
			Diff: map[string]any{"key": f.Key, "label": f.Label, "type": f.Type},
		})
	})
	if err != nil {
		return nil, err
	}
	return &f, nil
}

var errOneBirthDate = fmt.Errorf("%w: this event already asks for a birth date; one birth date question is enough", domain.ErrConflict)

// syncBirthDates makes event_participants.birth_date follow the event's
// birth date question: the answers to it after one is added (or a date
// question becomes one), nothing once it's removed.
func syncBirthDates(ctx context.Context, tx *sql.Tx, orgID, eventID string) error {
	var key sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT key FROM form_fields
		WHERE org_id = $1 AND event_id = $2 AND type = 'birth_date' AND workshop_id IS NULL`, orgID, eventID).Scan(&key)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Answers were checked as dates when given; anything else counts as none.
	_, err = tx.ExecContext(ctx, `
		UPDATE event_participants SET birth_date = d.value
		FROM (SELECT id, CASE WHEN $3::text IS NOT NULL AND answers->>$3 ~ '^\d{4}-\d{2}-\d{2}$' AND answers->>$3 >= '1900-01-01'
		                      THEN (answers->>$3)::date END AS value
		      FROM event_participants WHERE org_id = $1 AND event_id = $2) d
		WHERE event_participants.id = d.id AND event_participants.birth_date IS DISTINCT FROM d.value`,
		orgID, eventID, key)
	return err
}

func (r *RegistrationRepo) DeleteField(ctx context.Context, es authz.EventScope, fieldID string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var key string
		err := tx.QueryRowContext(ctx, `DELETE FROM form_fields WHERE org_id = $1 AND event_id = $2 AND id = $3 RETURNING key`,
			es.OrgID(), es.EventID(), fieldID).Scan(&key)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := syncBirthDates(ctx, tx, es.OrgID(), es.EventID()); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "form.field.remove", TargetType: "field", TargetID: fieldID,
			Diff: map[string]any{"key": key},
		})
	})
}

// MoveField swaps a field with its neighbour above (up) or below.
func (r *RegistrationRepo) MoveField(ctx context.Context, es authz.EventScope, fieldID string, up bool) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		fields, err := listFormFields(ctx, tx, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		i := -1
		for k, f := range fields {
			if f.ID == fieldID {
				i = k
			}
		}
		if i < 0 {
			return domain.ErrNotFound
		}
		j := i + 1
		if up {
			j = i - 1
		}
		if j < 0 || j >= len(fields) {
			return nil
		}
		fields[i], fields[j] = fields[j], fields[i]
		for pos, f := range fields { // renumber, which also repairs gaps
			if _, err := tx.ExecContext(ctx, `UPDATE form_fields SET position = $2 WHERE id = $1`, f.ID, pos+1); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- Public page and registering ----------------------------------------------

// PublicEvent finds a published, non-private event by its org and event
// slugs, with its form and how many places are taken. Anyone may call it,
// so it runs outside withTenant and only returns public events.
func (r *RegistrationRepo) PublicEvent(ctx context.Context, orgSlug, eventSlug string) (*domain.PublicEvent, error) {
	var e domain.Event
	var venue, settings []byte
	p := &domain.PublicEvent{Event: &e}
	dest := append(eventDest(&e, &venue, &settings), &p.OrgName, &p.OrgSlug)
	err := r.db.QueryRowContext(ctx, `SELECT `+eventCols+`, o.name, o.slug`+eventFrom+`
		JOIN orgs o ON o.id = e.org_id AND o.status = 'active'
		WHERE o.slug = $1 AND e.slug = $2 AND e.status = 'published'
		  AND e.visibility <> 'private' AND e.archived_at IS NULL AND e.deleted_at IS NULL`,
		orgSlug, eventSlug).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := decodeEvent(&e, venue, settings); err != nil {
		return nil, err
	}
	err = withTenant(ctx, r.db, e.OrgID, "", func(tx *sql.Tx) error {
		var err error
		if p.Fields, err = listFormFields(ctx, tx, e.OrgID, e.ID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM event_participants
			WHERE event_id = $1 AND status IN ('registered', 'pending')`, e.ID).Scan(&p.Taken)
	})
	return p, err
}

// eventAgeOn is the day an event counts ages on: its "age is counted on"
// date, else its start date in its own time zone, else today. The alias is
// the events row; see participantAge.
func eventAgeOn(e string) string {
	return `COALESCE(` + e + `.age_on, (` + e + `.starts_at AT TIME ZONE ` + e + `.time_zone)::date, CURRENT_DATE)`
}

// participantAge is p's age in whole years on its event's cut-off date
// (NULL without a birth date). Postgres's age() counts a birthday as
// reached on the same month and day, so 18 on the day itself is 18.
var participantAge = `CASE WHEN p.birth_date IS NULL THEN NULL ELSE date_part('year', age((SELECT ` + eventAgeOn("ae") +
	` FROM events ae WHERE ae.id = p.event_id), p.birth_date))::int END`

// participantIsMinor: under 18 on the cut-off date (false without a birth date).
var participantIsMinor = `(p.birth_date IS NOT NULL AND ` + participantAge + ` < 18)`

var participantCols = `p.id, p.event_id, COALESCE(p.account_id::text, ''), p.email, p.full_name, p.status,
	p.pass_id, p.answers, p.source, p.registered_at, p.updated_at,
	COALESCE(to_char(p.birth_date, 'YYYY-MM-DD'), ''), ` + participantAge

func scanParticipant(row interface{ Scan(...any) error }) (domain.EventParticipant, error) {
	var p domain.EventParticipant
	var answers []byte
	err := row.Scan(&p.ID, &p.EventID, &p.AccountID, &p.Email, &p.FullName, &p.Status,
		&p.PassID, &answers, &p.Source, &p.RegisteredAt, &p.UpdatedAt, &p.BirthDate, &p.Age)
	if err == nil {
		err = json.Unmarshal(answers, &p.Answers)
	}
	return p, err
}

// Register adds a participant to a public event. The event row is locked so
// two people can't both take the last place: with capacity reached the new
// registration is waitlisted (or refused when the waitlist is off). passID
// is called again if its value is already taken in the event.
func (r *RegistrationRepo) Register(ctx context.Context, ev *domain.Event, p domain.EventParticipant, passID func() string) (*domain.EventParticipant, error) {
	answers, _ := json.Marshal(p.Answers)
	var out domain.EventParticipant
	for attempt := 0; attempt < 5; attempt++ {
		pid := passID()
		err := withTenant(ctx, r.db, ev.OrgID, "", func(tx *sql.Tx) error {
			var capacity sql.NullInt64
			var waitlist bool
			var mode string
			if err := tx.QueryRowContext(ctx, `SELECT capacity, waitlist, reg_mode FROM events WHERE org_id = $1 AND id = $2 FOR UPDATE`,
				ev.OrgID, ev.ID).Scan(&capacity, &waitlist, &mode); err != nil {
				return err
			}
			existing, err := scanParticipant(tx.QueryRowContext(ctx, `SELECT `+participantCols+` FROM event_participants p
				WHERE p.event_id = $1 AND p.email = $2`, ev.ID, p.Email))
			if err == nil && existing.Status != domain.PartCancelled {
				return &domain.AlreadyRegisteredError{Participant: existing}
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			status := domain.PartRegistered
			if mode == domain.RegApproval {
				status = domain.PartPending
			}
			if capacity.Valid {
				var taken int64
				if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM event_participants
					WHERE event_id = $1 AND status IN ('registered', 'pending')`, ev.ID).Scan(&taken); err != nil {
					return err
				}
				if taken >= capacity.Int64 {
					if !waitlist {
						return fmt.Errorf("%w: this event is full", domain.ErrConflict)
					}
					status = domain.PartWaitlisted
				}
			}
			// A cancelled registration is replaced: the person signed up again.
			if _, err := tx.ExecContext(ctx, `DELETE FROM event_participants WHERE event_id = $1 AND email = $2 AND status = 'cancelled'`,
				ev.ID, p.Email); err != nil {
				return err
			}
			id := newID()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event_participants (id, org_id, event_id, email, full_name, status, pass_id, answers, source, external_ref, birth_date)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), NULLIF($11, '')::date)`,
				id, ev.OrgID, ev.ID, p.Email, p.FullName, status, pid, answers, orDefault(p.Source, "form"), p.ExternalRef, p.BirthDate); err != nil {
				if pgCode(err) == pgUniqueViolation && strings.Contains(pgConstraint(err), "external") {
					return ErrAlreadyImported
				}
				return err
			}
			if out, err = scanParticipant(tx.QueryRowContext(ctx, `SELECT `+participantCols+` FROM event_participants p WHERE p.id = $1`, id)); err != nil {
				return err
			}
			return enqueueMail(ctx, tx, ev.OrgID, ev.ID, out.ID, out.Email, domain.MailForStatus[status])
		})
		if pgCode(err) == pgUniqueViolation && strings.Contains(pgConstraint(err), "pass_id") {
			continue // pass ID collision: draw another
		}
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	return nil, errors.New("could not find a free pass ID")
}

// ---- Organizer's participant list --------------------------------------------

func (r *RegistrationRepo) ListParticipants(ctx context.Context, es authz.EventScope, q domain.ParticipantQuery) ([]domain.EventParticipant, domain.ParticipantCounts, error) {
	out := []domain.EventParticipant{}
	counts := domain.ParticipantCounts{}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		args := []any{es.OrgID(), es.EventID()}
		where := "p.org_id = $1 AND p.event_id = $2"
		if q.Status != "" {
			args = append(args, q.Status)
			where += fmt.Sprintf(" AND p.status = $%d", len(args))
		}
		if q.Q != "" {
			args = append(args, "%"+escapeLike(q.Q)+"%")
			n := len(args)
			where += fmt.Sprintf(" AND (p.full_name ILIKE $%d OR p.email ILIKE $%d OR p.pass_id ILIKE $%d)", n, n, n)
		}
		if q.Minors {
			where += " AND " + participantIsMinor
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+participantCols+` FROM event_participants p WHERE `+where+`
			ORDER BY p.registered_at DESC LIMIT 500`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			p, err := scanParticipant(rows)
			if err != nil {
				rows.Close()
				return err
			}
			out = append(out, p)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		crows, err := tx.QueryContext(ctx, `SELECT status, count(*) FROM event_participants
			WHERE org_id = $1 AND event_id = $2 GROUP BY status`, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		defer crows.Close()
		for crows.Next() {
			var s string
			var n int
			if err := crows.Scan(&s, &n); err != nil {
				return err
			}
			counts[s] = n
		}
		if err := crows.Err(); err != nil {
			return err
		}
		var minors int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM event_participants p
			WHERE p.org_id = $1 AND p.event_id = $2 AND p.status <> 'cancelled' AND `+participantIsMinor,
			es.OrgID(), es.EventID()).Scan(&minors); err != nil {
			return err
		}
		counts[domain.CountMinors] = minors
		return nil
	})
	return out, counts, err
}

func (r *RegistrationRepo) GetParticipant(ctx context.Context, es authz.EventScope, id string) (*domain.EventParticipant, error) {
	var p domain.EventParticipant
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		p, err = scanParticipant(tx.QueryRowContext(ctx, `SELECT `+participantCols+` FROM event_participants p
			WHERE p.org_id = $1 AND p.event_id = $2 AND p.id = $3`, es.OrgID(), es.EventID(), id))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetStatus changes a participant's status (approve, cancel, move off the
// waitlist).
func (r *RegistrationRepo) SetStatus(ctx context.Context, es authz.EventScope, id, status string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var email, prev string
		err := tx.QueryRowContext(ctx, `
			UPDATE event_participants p SET status = $4, updated_at = NOW()
			FROM (SELECT status FROM event_participants WHERE org_id = $1 AND event_id = $2 AND id = $3) old
			WHERE p.org_id = $1 AND p.event_id = $2 AND p.id = $3
			RETURNING p.email, old.status`, es.OrgID(), es.EventID(), id, status).Scan(&email, &prev)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil || prev == status {
			return err
		}
		if err := enqueueMail(ctx, tx, es.OrgID(), es.EventID(), id, email, domain.MailForStatus[status]); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "participant.status", TargetType: "participant", TargetID: id,
			Diff: map[string]any{"email": email, "from": prev, "to": status},
		})
	})
}

// enqueueMail queues an email to a participant in the caller's transaction.
func enqueueMail(ctx context.Context, tx *sql.Tx, orgID, eventID, participantID, email, template string) error {
	if template == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox (id, org_id, event_id, participant_id, to_email, template_key) VALUES ($1, $2, $3, $4, $5, $6)`,
		newID(), orgID, eventID, participantID, email, template)
	return err
}

// ResendPass queues the pass again for someone who registered before, unless
// one was queued in the last few minutes (so the form can't be used to flood
// an inbox). It reports whether an email was queued.
func (r *RegistrationRepo) ResendPass(ctx context.Context, p *domain.EventParticipant, orgID string) (bool, error) {
	template := domain.MailForStatus[p.Status]
	if template == "" {
		return false, nil
	}
	queued := false
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		var recent bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM outbox WHERE participant_id = $1 AND created_at > $2)`,
			p.ID, time.Now().Add(-domain.MailResendMinimum)).Scan(&recent); err != nil {
			return err
		}
		if recent {
			return nil
		}
		queued = true
		return enqueueMail(ctx, tx, orgID, p.EventID, p.ID, p.Email, template)
	})
	return queued, err
}

// ErrAlreadyImported: a participant with this external reference (form
// response) already exists in the event.
var ErrAlreadyImported = fmt.Errorf("%w: already imported", domain.ErrConflict)
