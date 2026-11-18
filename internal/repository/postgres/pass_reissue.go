package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// ReissuePass bumps a participant's pass generation (security review F4):
// every QR code issued before stops scanning ("This pass was replaced"), and
// the pass the participant downloads next carries the new one.
func (r *RegistrationRepo) ReissuePass(ctx context.Context, es authz.EventScope, participantID string) (int, error) {
	var gen int
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var email string
		err := tx.QueryRowContext(ctx, `
			UPDATE event_participants SET pass_generation = pass_generation + 1, updated_at = NOW()
			WHERE org_id = $1 AND event_id = $2 AND id = $3
			RETURNING pass_generation, email`, es.OrgID(), es.EventID(), participantID).Scan(&gen, &email)
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "participant.pass.reissue", TargetType: "participant", TargetID: participantID,
			Diff: map[string]any{"email": email, "generation": gen},
		})
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return 0, domain.ErrNotFound
	}
	return gen, err
}

// ReissueMyPass is the participant's own "my badge was lost" action. Only
// the account the registration is linked to may do it.
func (r *PortalRepo) ReissueMyPass(ctx context.Context, as authz.AccountScope, orgID, participantID string) error {
	err := withTenant(ctx, r.db, orgID, as.AccountID(), func(tx *sql.Tx) error {
		var gen int
		var eventID string
		err := tx.QueryRowContext(ctx, `
			UPDATE event_participants SET pass_generation = pass_generation + 1, updated_at = NOW()
			WHERE org_id = $1 AND id = $2 AND account_id = $3
			RETURNING pass_generation, event_id`, orgID, participantID, as.AccountID()).Scan(&gen, &eventID)
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, orgID, as.AccountID(), domain.Audit{
			EventID: eventID, Action: "participant.pass.reissue", TargetType: "participant", TargetID: participantID,
			Diff: map[string]any{"by": "participant", "generation": gen},
		})
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return domain.ErrNotFound
	}
	return err
}
