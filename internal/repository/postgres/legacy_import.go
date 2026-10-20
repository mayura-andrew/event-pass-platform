package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// LegacyImport is what ImportLegacy did.
type LegacyImport struct {
	EventID   string
	EventSlug string
	Created   bool // a new event was made
	Imported  int  // participants copied this run
	Skipped   int  // already there (re-run) or clashing
	Fields    int  // questions created
}

// ImportLegacy copies the current build's confirmed delegates into an event
// of an org, as migration step "existing participants, passes, check-ins ->
// first event of the default org". Each delegate keeps their UUID, their
// membership ID (as the pass ID) and their QR signature, so the QR codes
// already sent keep working. Custom fields become questions. Run by an
// operator (cmd/legacy-import), so it works outside Row Level Security; it
// is safe to run again.
func ImportLegacy(ctx context.Context, db *sql.DB, orgSlug, ownerEmail, eventName, eventID string) (*LegacyImport, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	res := &LegacyImport{}

	var orgID, ownerID, role string
	err = tx.QueryRowContext(ctx, `
		SELECT o.id, a.id, m.role FROM orgs o
		JOIN org_members m ON m.org_id = o.id
		JOIN accounts a ON a.id = m.account_id
		WHERE o.slug = $1 AND a.email = $2`, orgSlug, strings.ToLower(ownerEmail)).Scan(&orgID, &ownerID, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%s is not a member of an org with the address %q (sign in once and create or join it first)", ownerEmail, orgSlug)
	}
	if err != nil {
		return nil, err
	}
	if role != domain.OrgRoleOwner && role != domain.OrgRoleAdmin {
		return nil, fmt.Errorf("%s must be an owner or admin of %s to import into it", ownerEmail, orgSlug)
	}

	if eventID != "" {
		err := tx.QueryRowContext(ctx, `SELECT slug FROM events WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, eventID).Scan(&res.EventSlug)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return nil, fmt.Errorf("event %s is not in %s", eventID, orgSlug)
		}
		if err != nil {
			return nil, err
		}
		res.EventID = eventID
	} else {
		res.EventID, res.Created = newID(), true
		base := strings.Trim(nonSlugChars(strings.ToLower(eventName)), "-")
		if base == "" {
			base = "imported-event"
		}
		if res.EventSlug, err = freeSlug(ctx, tx, orgID, base); err != nil {
			return nil, err
		}
		// Published and private: the passes are already out, and nobody
		// new registers through the platform for this event.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events (id, org_id, slug, name, type, status, published_at, time_zone, visibility,
			                    owner_id, created_by, reg_mode, waitlist, pass_prefix)
			VALUES ($1, $2, $3, $4, 'hackathon', 'published', NOW(), 'Asia/Colombo', 'private', $5, $5, 'open', false, 'NSAC')`,
			res.EventID, orgID, res.EventSlug, eventName, ownerID); err != nil {
			return nil, err
		}
	}

	// Questions from the custom field definitions, plus the team.
	r, err := tx.ExecContext(ctx, `
		INSERT INTO form_fields (id, org_id, event_id, key, type, config, position)
		SELECT gen_random_uuid(), $1::uuid, $2::uuid, d.field_key,
		       CASE d.data_type WHEN 'number' THEN 'number' WHEN 'boolean' THEN 'checkbox' WHEN 'date' THEN 'date' ELSE 'text' END,
		       jsonb_build_object('label', d.field_label, 'required', false,
		                          'visibility', CASE WHEN d.show_in_scanner_popup THEN 'staff' ELSE 'admin' END),
		       d.sort_order
		FROM participant_field_definitions d
		WHERE d.field_key ~ '^[a-z][a-z0-9_]{0,59}$' AND d.field_key NOT IN ('full_name', 'email', 'name')
		UNION ALL
		SELECT gen_random_uuid(), $1::uuid, $2::uuid, 'team_name', 'text', '{"label": "Team", "required": false, "visibility": "pass"}', 0
		ON CONFLICT (event_id, key) WHERE workshop_id IS NULL DO NOTHING`, orgID, res.EventID)
	if err != nil {
		return nil, err
	}
	n, _ := r.RowsAffected()
	res.Fields = int(n)

	var total int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE is_confirmed`).Scan(&total); err != nil {
		return nil, err
	}
	r, err = tx.ExecContext(ctx, `
		INSERT INTO event_participants (id, org_id, event_id, account_id, email, full_name, status, pass_id, answers,
		                                source, legacy_membership_id, legacy_qr_signature, registered_at)
		SELECT p.id, $1, $2, (SELECT a.id FROM accounts a WHERE a.email = lower(p.email)), lower(p.email), p.full_name,
		       'registered', p.membership_id,
		       p.custom_attributes || CASE WHEN COALESCE(p.team_name, '') = '' THEN '{}'::jsonb
		                                   ELSE jsonb_build_object('team_name', p.team_name) END,
		       'import', p.membership_id, p.qr_signature, p.created_at
		FROM participants p
		WHERE p.is_confirmed AND length(p.membership_id) <= 20
		ON CONFLICT DO NOTHING`, orgID, res.EventID)
	if err != nil {
		return nil, err
	}
	n, _ = r.RowsAffected()
	res.Imported, res.Skipped = int(n), total-int(n)

	if err := insertAudit(ctx, tx, orgID, ownerID, domain.Audit{
		EventID: res.EventID, Action: "event.import", TargetType: "event", TargetID: res.EventID,
		Diff: map[string]any{"imported": res.Imported, "skipped": res.Skipped, "questions": res.Fields},
	}); err != nil {
		return nil, err
	}
	return res, tx.Commit()
}

func nonSlugChars(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := b.String()
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

// CheckParticipant returns a participant of the event with their stored
// legacy QR signature ("" for platform passes), for checking a scanned pass.
func (r *RegistrationRepo) CheckParticipant(ctx context.Context, orgID, accountID, eventID, participantID string) (*domain.EventParticipant, string, error) {
	var p domain.EventParticipant
	var legacy sql.NullString
	err := withTenant(ctx, r.db, orgID, accountID, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT `+participantCols+`, p.legacy_qr_signature FROM event_participants p
			WHERE p.org_id = $1 AND p.event_id = $2 AND p.id = $3`, orgID, eventID, participantID)
		var answers []byte
		if err := row.Scan(&p.ID, &p.EventID, &p.AccountID, &p.Email, &p.FullName, &p.Status,
			&p.PassID, &answers, &p.Source, &p.RegisteredAt, &p.UpdatedAt, &legacy); err != nil {
			return err
		}
		return json.Unmarshal(answers, &p.Answers)
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, "", domain.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return &p, legacy.String, nil
}
