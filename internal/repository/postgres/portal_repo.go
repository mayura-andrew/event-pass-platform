package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
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
		&p.RegisteredAt, &p.UpdatedAt, &p.BirthDate, &p.Age, &p.PassGeneration}
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

// PendingClaims lists registrations made with the account's email that are
// not linked to any account yet and that the account has not declined.
// Someone else may have typed this address into a public form, so only the
// event, org and date are returned, never the answers. The rows belong to
// other orgs, so this is a system read, keyed by the signed-in account's own
// (verified) email.
func (r *PortalRepo) PendingClaims(ctx context.Context, as authz.AccountScope) ([]domain.PassClaim, error) {
	out := []domain.PassClaim{}
	err := withSystem(ctx, r.db, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT p.id, e.name, o.name, p.registered_at
			FROM event_participants p
			JOIN accounts a ON a.id = $1 AND a.email = p.email
			JOIN events e ON e.id = p.event_id AND e.deleted_at IS NULL
			JOIN orgs o ON o.id = p.org_id AND o.status = 'active'
			WHERE p.account_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM pass_claim_declines d WHERE d.participant_id = p.id AND d.account_id = $1)
			ORDER BY p.registered_at DESC
			LIMIT 50`, as.AccountID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c domain.PassClaim
			if err := rows.Scan(&c.ParticipantID, &c.EventName, &c.OrgName, &c.RegisteredAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// ClaimPass links one registration to the account after its holder confirmed
// it on "My passes". Only an unlinked row with the account's own email
// qualifies; anything else is ErrNotFound.
func (r *PortalRepo) ClaimPass(ctx context.Context, as authz.AccountScope, participantID string) error {
	err := withSystem(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE event_participants p SET account_id = a.id, updated_at = NOW()
			FROM accounts a
			WHERE a.id = $1 AND p.id = $2 AND p.account_id IS NULL AND p.email = a.email`,
			as.AccountID(), participantID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM pass_claim_declines WHERE participant_id = $1 AND account_id = $2`,
			participantID, as.AccountID())
		return err
	})
	if pgCode(err) == pgInvalidTextRepr {
		return domain.ErrNotFound
	}
	return err
}

// DeclineClaim hides a registration from the account's claims ("Not me").
// The registration itself is left alone for its organizer.
func (r *PortalRepo) DeclineClaim(ctx context.Context, as authz.AccountScope, participantID string) error {
	err := withSystem(ctx, r.db, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO pass_claim_declines (participant_id, account_id)
			SELECT p.id, a.id FROM event_participants p JOIN accounts a ON a.id = $1 AND a.email = p.email
			WHERE p.id = $2 AND p.account_id IS NULL
			ON CONFLICT DO NOTHING`, as.AccountID(), participantID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if pgCode(err) == pgInvalidTextRepr {
		return domain.ErrNotFound
	}
	return err
}

// MyPasses lists the registrations linked to the account, upcoming events
// first. Registrations made with its email but not yet confirmed are
// PendingClaims, not passes.
func (r *PortalRepo) MyPasses(ctx context.Context, as authz.AccountScope) ([]domain.PassContext, error) {
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
