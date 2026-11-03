package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// PortalRepo is the participant's side: their own passes across orgs. It
// runs as the account with no org set, so Row Level Security returns only
// rows whose account_id is theirs.
type PortalRepo struct{ db *sql.DB }

func NewPortalRepo(db *sql.DB) *PortalRepo { return &PortalRepo{db: db} }

var portalQuery = `SELECT ` + participantCols + `, ` + eventCols + `, o.name, o.slug
	FROM event_participants p
	JOIN events e ON e.id = p.event_id
	LEFT JOIN accounts oa ON oa.id = e.owner_id
	JOIN orgs o ON o.id = p.org_id`

func scanPortal(row interface{ Scan(...any) error }) (domain.PassContext, error) {
	var pc domain.PassContext
	var answers, venue, settings []byte
	p := &pc.Participant
	dest := []any{&p.ID, &p.EventID, &p.AccountID, &p.Email, &p.FullName, &p.Status, &p.PassID, &answers, &p.Source,
		&p.RegisteredAt, &p.UpdatedAt, &p.BirthDate, &p.Age}
	dest = append(dest, eventDest(&pc.Event, &venue, &settings)...)
	dest = append(dest, &pc.OrgName, &pc.OrgSlug)
	if err := row.Scan(dest...); err != nil {
		return pc, err
	}
	if err := decodeEvent(&pc.Event, venue, settings); err != nil {
		return pc, err
	}
	return pc, json.Unmarshal(answers, &p.Answers)
}

// MyPasses lists the account's registrations, upcoming events first. It
// first links registrations made with the account's email since it signed
// in: the email was proven by signing in, so they are theirs.
func (r *PortalRepo) MyPasses(ctx context.Context, as authz.AccountScope) ([]domain.PassContext, error) {
	if _, err := r.db.ExecContext(ctx, `
		UPDATE event_participants SET account_id = $1
		WHERE account_id IS NULL AND email = (SELECT email FROM accounts WHERE id = $1)`, as.AccountID()); err != nil {
		return nil, err
	}
	out := []domain.PassContext{}
	err := withTenant(ctx, r.db, "", as.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, portalQuery+`
			WHERE p.account_id = $1 AND e.deleted_at IS NULL
			ORDER BY (e.ends_at IS NOT NULL AND e.ends_at < NOW()), e.starts_at NULLS LAST, p.registered_at DESC`, as.AccountID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			pc, err := scanPortal(rows)
			if err != nil {
				return err
			}
			out = append(out, pc)
		}
		return rows.Err()
	})
	return out, err
}

// MyPass returns one of the account's passes, or ErrNotFound for anyone
// else's.
func (r *PortalRepo) MyPass(ctx context.Context, as authz.AccountScope, participantID string) (*domain.PassContext, error) {
	var pc domain.PassContext
	err := withTenant(ctx, r.db, "", as.AccountID(), func(tx *sql.Tx) error {
		var err error
		pc, err = scanPortal(tx.QueryRowContext(ctx, portalQuery+`
			WHERE p.account_id = $1 AND p.id = $2 AND e.deleted_at IS NULL`, as.AccountID(), participantID))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// The full context (questions, legacy QR) comes from the system path,
	// now that ownership is established.
	return passContext(ctx, r.db, pc.Event.OrgID, participantID)
}
