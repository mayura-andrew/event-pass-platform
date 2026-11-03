package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

// OrgRepo stores organizations, their members and invites. Every query runs
// through withTenant, so Row Level Security backs up the explicit org_id
// filters below. Changes write their audit entry in the same transaction.
type OrgRepo struct{ db *sql.DB }

func NewOrgRepo(db *sql.DB) *OrgRepo { return &OrgRepo{db: db} }

// mapOwnerRule turns the last-owner trigger (migration 007) into a conflict.
func mapOwnerRule(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation && pgErr.ConstraintName == "org_needs_owner" {
		return fmt.Errorf("%w: an organization needs at least one owner; make someone else an owner first", domain.ErrConflict)
	}
	return err
}

// CreateOrg creates an org with the caller as its owner.
func (r *OrgRepo) CreateOrg(ctx context.Context, as authz.AccountScope, name, slug string) (*domain.OrgMembership, error) {
	var m domain.OrgMembership
	orgID := newID()
	err := withTenant(ctx, r.db, orgID, as.AccountID(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO orgs (id, slug, name, created_by) VALUES ($1, $2, $3, $4)`,
			orgID, slug, name, as.AccountID()); err != nil {
			if pgCode(err) == pgUniqueViolation {
				return fmt.Errorf("%w: the address /o/%s is taken; choose another", domain.ErrConflict, slug)
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO org_members (org_id, account_id, role) VALUES ($1, $2, 'owner')`, orgID, as.AccountID()); err != nil {
			return err
		}
		var err error
		if m, err = scanMembership(tx.QueryRowContext(ctx, membershipQuery+` WHERE o.id = $1`, orgID, as.AccountID())); err != nil {
			return err
		}
		return insertAudit(ctx, tx, orgID, as.AccountID(), domain.Audit{
			Action: "org.create", TargetType: "org", TargetID: orgID, Diff: map[string]any{"name": name, "slug": slug},
		})
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// membershipQuery selects an org with the caller's role: their org_members
// role, or "guest" when they only hold event shares there. $2 is the caller.
const membershipQuery = `
	SELECT o.id, o.slug, o.name, COALESCE(o.brand_primary, ''), o.plan, o.settings, o.created_at,
	       COALESCE(m.role, 'guest')
	FROM orgs o
	LEFT JOIN org_members m ON m.org_id = o.id AND m.account_id = $2`

// accessible keeps orgs the caller is a member or a guest of.
const accessible = ` AND o.status = 'active'
	AND (m.account_id IS NOT NULL
	     OR EXISTS (SELECT 1 FROM event_shares s WHERE s.org_id = o.id AND s.account_id = $2))`

func scanMembership(row interface{ Scan(...any) error }) (domain.OrgMembership, error) {
	var m domain.OrgMembership
	var settings []byte
	err := row.Scan(&m.ID, &m.Slug, &m.Name, &m.BrandPrimary, &m.Plan, &settings, &m.CreatedAt, &m.Role)
	if err == nil {
		err = json.Unmarshal(settings, &m.Settings)
	}
	return m, err
}

// ListMyOrgs returns every org the account belongs to or is a guest of.
func (r *OrgRepo) ListMyOrgs(ctx context.Context, as authz.AccountScope) ([]domain.OrgMembership, error) {
	out := []domain.OrgMembership{}
	err := withTenant(ctx, r.db, "", as.AccountID(), func(tx *sql.Tx) error {
		// $1 is unused but keeps membershipQuery's $2 numbering.
		rows, err := tx.QueryContext(ctx, membershipQuery+` WHERE $1::text IS NOT NULL`+accessible+`
			ORDER BY m.account_id IS NULL, lower(o.name), o.slug`, "", as.AccountID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMembership(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

// Membership returns the account's membership of the org with this slug
// (role "guest" for event shares only). Anyone else gets ErrNotFound, the
// same as for a slug that doesn't exist.
func (r *OrgRepo) Membership(ctx context.Context, as authz.AccountScope, slug string) (*domain.OrgMembership, error) {
	var m domain.OrgMembership
	err := withTenant(ctx, r.db, "", as.AccountID(), func(tx *sql.Tx) error {
		var err error
		m, err = scanMembership(tx.QueryRowContext(ctx, membershipQuery+` WHERE o.slug = $1`+accessible, slug, as.AccountID()))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// UpdateSettings replaces the org's settings.
func (r *OrgRepo) UpdateSettings(ctx context.Context, sc authz.OrgScope, st domain.OrgSettings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE orgs SET settings = $2, updated_at = NOW() WHERE id = $1`, sc.OrgID(), b); err != nil {
			return err
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: "org.settings", TargetType: "org", TargetID: sc.OrgID(),
			Diff: map[string]any{"external_sharing": st.AllowsExternalSharing(), "checklist_hidden": st.ChecklistHidden},
		})
	})
}

// Onboarding reads the setup checklist's progress. Steps link to the
// caller's most recent event, or else the org's.
func (r *OrgRepo) Onboarding(ctx context.Context, sc authz.OrgScope) (*domain.Onboarding, error) {
	var o domain.Onboarding
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT COALESCE((SELECT id::text FROM events WHERE org_id = $1 AND deleted_at IS NULL
			                 ORDER BY (owner_id = $2) DESC, updated_at DESC LIMIT 1), ''),
			       EXISTS (SELECT 1 FROM events WHERE org_id = $1 AND deleted_at IS NULL),
			       EXISTS (SELECT 1 FROM form_fields WHERE org_id = $1),
			       EXISTS (SELECT 1 FROM pass_templates WHERE org_id = $1)
			         OR EXISTS (SELECT 1 FROM events WHERE org_id = $1 AND pass_template_id IS NOT NULL),
			       EXISTS (SELECT 1 FROM events WHERE org_id = $1 AND status = 'published' AND deleted_at IS NULL),
			       EXISTS (SELECT 1 FROM event_shares WHERE org_id = $1 AND role = 'checkin' AND account_id IS NOT NULL),
			       EXISTS (SELECT 1 FROM checkins WHERE org_id = $1)`,
			sc.OrgID(), sc.AccountID()).Scan(&o.EventID, &o.HasEvent, &o.HasQuestions, &o.HasDesign, &o.Published, &o.SharedCheckin, &o.Scanned)
	})
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ---- Members -----------------------------------------------------------------

const memberCols = `a.id, a.email, a.name, COALESCE(a.avatar_url, ''), m.role, m.created_at`

func scanMember(row interface{ Scan(...any) error }) (domain.OrgMember, error) {
	var m domain.OrgMember
	err := row.Scan(&m.AccountID, &m.Email, &m.Name, &m.AvatarURL, &m.Role, &m.JoinedAt)
	return m, err
}

func (r *OrgRepo) ListMembers(ctx context.Context, sc authz.OrgScope) ([]domain.OrgMember, error) {
	out := []domain.OrgMember{}
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+memberCols+`
			FROM org_members m JOIN accounts a ON a.id = m.account_id
			WHERE m.org_id = $1
			ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END,
			         lower(COALESCE(NULLIF(a.name, ''), a.email))`, sc.OrgID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanMember(rows)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	return out, err
}

func (r *OrgRepo) GetMember(ctx context.Context, sc authz.OrgScope, accountID string) (*domain.OrgMember, error) {
	var m domain.OrgMember
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		var err error
		m, err = getMember(ctx, tx, sc.OrgID(), accountID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func getMember(ctx context.Context, tx *sql.Tx, orgID, accountID string) (domain.OrgMember, error) {
	m, err := scanMember(tx.QueryRowContext(ctx, `
		SELECT `+memberCols+`
		FROM org_members m JOIN accounts a ON a.id = m.account_id
		WHERE m.org_id = $1 AND m.account_id = $2`, orgID, accountID))
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return m, domain.ErrNotFound
	}
	return m, err
}

func (r *OrgRepo) SetMemberRole(ctx context.Context, sc authz.OrgScope, accountID, role string) error {
	return withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		before, err := getMember(ctx, tx, sc.OrgID(), accountID)
		if err != nil {
			return err
		}
		if before.Role == role {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_members SET role = $3 WHERE org_id = $1 AND account_id = $2`, sc.OrgID(), accountID, role); err != nil {
			return mapOwnerRule(err)
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: "member.role", TargetType: "member", TargetID: accountID,
			Diff: map[string]any{"email": before.Email, "from": before.Role, "to": role},
		})
	})
}

func (r *OrgRepo) RemoveMember(ctx context.Context, sc authz.OrgScope, accountID string) error {
	return withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		before, err := getMember(ctx, tx, sc.OrgID(), accountID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM org_members WHERE org_id = $1 AND account_id = $2`, sc.OrgID(), accountID); err != nil {
			return mapOwnerRule(err)
		}
		action := "member.remove"
		if accountID == sc.AccountID() {
			action = "member.leave"
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: action, TargetType: "member", TargetID: accountID,
			Diff: map[string]any{"email": before.Email, "role": before.Role},
		})
	})
}

// ---- Invites -----------------------------------------------------------------

const inviteCols = `i.id, i.org_id, COALESCE(i.event_id::text, ''), i.email, i.role,
	COALESCE(NULLIF(a.name, ''), a.email, ''), i.created_at, i.expires_at`

func scanInvite(row interface{ Scan(...any) error }) (domain.Invite, error) {
	var i domain.Invite
	err := row.Scan(&i.ID, &i.OrgID, &i.EventID, &i.Email, &i.Role, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt)
	return i, err
}

// CreateInvite stores an open org invite, replacing the token, role and
// expiry of an earlier open invite to the same address. Inviting a member is
// a conflict.
func (r *OrgRepo) CreateInvite(ctx context.Context, sc authz.OrgScope, email, role string, tokenHash []byte, expires time.Time) (*domain.Invite, error) {
	var inv domain.Invite
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM org_members m JOIN accounts a ON a.id = m.account_id
			               WHERE m.org_id = $1 AND a.email = $2)`, sc.OrgID(), email).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: %s is already a member", domain.ErrConflict, email)
		}
		var id string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO invites (id, org_id, email, role, token_hash, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (org_id, email) WHERE event_id IS NULL AND accepted_at IS NULL AND revoked_at IS NULL
			DO UPDATE SET role = EXCLUDED.role, token_hash = EXCLUDED.token_hash,
			              invited_by = EXCLUDED.invited_by, expires_at = EXCLUDED.expires_at, created_at = NOW()
			RETURNING id`,
			newID(), sc.OrgID(), email, role, tokenHash, sc.AccountID(), expires).Scan(&id); err != nil {
			return err
		}
		var err error
		if inv, err = getInvite(ctx, tx, id); err != nil {
			return err
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: "invite.create", TargetType: "invite", TargetID: id, Diff: map[string]any{"email": email, "role": role},
		})
	})
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func getInvite(ctx context.Context, tx *sql.Tx, id string) (domain.Invite, error) {
	return scanInvite(tx.QueryRowContext(ctx, `
		SELECT `+inviteCols+` FROM invites i LEFT JOIN accounts a ON a.id = i.invited_by
		WHERE i.id = $1`, id))
}

// ListInvites returns open org invites, newest first; expired ones are
// included so they can be sent again.
func (r *OrgRepo) ListInvites(ctx context.Context, sc authz.OrgScope) ([]domain.Invite, error) {
	return listInvites(ctx, r.db, sc.OrgID(), sc.AccountID(), "")
}

// listInvites lists open invites to the org (eventID "") or to one event.
func listInvites(ctx context.Context, db *sql.DB, orgID, accountID, eventID string) ([]domain.Invite, error) {
	out := []domain.Invite{}
	err := withTenant(ctx, db, orgID, accountID, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+inviteCols+` FROM invites i LEFT JOIN accounts a ON a.id = i.invited_by
			WHERE i.org_id = $1 AND i.event_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid
			  AND i.accepted_at IS NULL AND i.revoked_at IS NULL
			ORDER BY i.created_at DESC`, orgID, eventID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			inv, err := scanInvite(rows)
			if err != nil {
				return err
			}
			out = append(out, inv)
		}
		return rows.Err()
	})
	return out, err
}

// RevokeInvite withdraws an open org invite.
func (r *OrgRepo) RevokeInvite(ctx context.Context, sc authz.OrgScope, inviteID string) error {
	return revokeInvite(ctx, r.db, sc.OrgID(), sc.AccountID(), "", inviteID)
}

// revokeInvite withdraws an open invite to the org (eventID "") or event.
func revokeInvite(ctx context.Context, db *sql.DB, orgID, accountID, eventID, inviteID string) error {
	return withTenant(ctx, db, orgID, accountID, func(tx *sql.Tx) error {
		var email string
		err := tx.QueryRowContext(ctx, `
			UPDATE invites SET revoked_at = NOW()
			WHERE org_id = $1 AND id = $2 AND event_id IS NOT DISTINCT FROM NULLIF($3, '')::uuid
			  AND accepted_at IS NULL AND revoked_at IS NULL
			RETURNING email`, orgID, inviteID, eventID).Scan(&email)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, orgID, accountID, domain.Audit{
			EventID: eventID, Action: "invite.revoke", TargetType: "invite", TargetID: inviteID,
			Diff: map[string]any{"email": email},
		})
	})
}

// InviteByToken finds an invite by its emailed token. The token is the
// credential, so this runs before any org is known, outside withTenant.
func (r *OrgRepo) InviteByToken(ctx context.Context, tokenHash []byte) (*domain.InviteDetails, error) {
	var d domain.InviteDetails
	err := r.db.QueryRowContext(ctx, `
		SELECT `+inviteCols+`, o.slug, o.name, COALESCE(e.name, ''),
		       i.accepted_at IS NOT NULL, i.revoked_at IS NOT NULL OR e.deleted_at IS NOT NULL
		FROM invites i
		JOIN orgs o ON o.id = i.org_id
		LEFT JOIN events e ON e.id = i.event_id
		LEFT JOIN accounts a ON a.id = i.invited_by
		WHERE i.token_hash = $1 AND o.status = 'active'`, tokenHash,
	).Scan(&d.ID, &d.OrgID, &d.EventID, &d.Email, &d.Role, &d.InvitedBy, &d.CreatedAt, &d.ExpiresAt,
		&d.OrgSlug, &d.OrgName, &d.EventName, &d.Accepted, &d.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// AcceptInvite closes the invite and grants what it offered: org membership,
// or a share of one event. The invite is locked so it can only be used once.
// Someone who already has access keeps their current role.
func (r *OrgRepo) AcceptInvite(ctx context.Context, as authz.AccountScope, inv *domain.InviteDetails) (*domain.OrgMembership, error) {
	var m domain.OrgMembership
	err := withTenant(ctx, r.db, inv.OrgID, as.AccountID(), func(tx *sql.Tx) error {
		var open bool
		if err := tx.QueryRowContext(ctx, `
			SELECT accepted_at IS NULL AND revoked_at IS NULL AND expires_at > NOW()
			FROM invites WHERE id = $1 AND org_id = $2 FOR UPDATE`, inv.ID, inv.OrgID).Scan(&open); err != nil {
			return err
		}
		if !open {
			return domain.ErrNotFound
		}
		audit := domain.Audit{Action: "invite.accept", TargetType: "invite", TargetID: inv.ID,
			Diff: map[string]any{"email": inv.Email, "role": inv.Role}}
		if inv.EventID == "" {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_members (org_id, account_id, role) VALUES ($1, $2, $3)
				ON CONFLICT (org_id, account_id) DO NOTHING`, inv.OrgID, as.AccountID(), inv.Role); err != nil {
				return err
			}
		} else {
			audit.EventID = inv.EventID
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event_shares (id, org_id, event_id, account_id, role, granted_by)
				SELECT $1, $2, $3, $4, $5, i.invited_by FROM invites i WHERE i.id = $6
				ON CONFLICT (event_id, account_id) DO NOTHING`,
				newID(), inv.OrgID, inv.EventID, as.AccountID(), inv.Role, inv.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE invites SET accepted_at = NOW(), accepted_by = $2 WHERE id = $1`, inv.ID, as.AccountID()); err != nil {
			return err
		}
		if err := insertAudit(ctx, tx, inv.OrgID, as.AccountID(), audit); err != nil {
			return err
		}
		var err error
		m, err = scanMembership(tx.QueryRowContext(ctx, membershipQuery+` WHERE o.id = $1`, inv.OrgID, as.AccountID()))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ---- Audit log -----------------------------------------------------------------

// ListAudit returns the newest entries for the org, or for one event when
// eventID is set.
func (r *OrgRepo) ListAudit(ctx context.Context, sc authz.OrgScope, eventID string, limit int) ([]domain.AuditEntry, error) {
	out := []domain.AuditEntry{}
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT l.id, COALESCE(l.event_id::text, ''), COALESCE(e.name, ''), COALESCE(l.actor_account_id::text, ''),
			       COALESCE(NULLIF(a.name, ''), a.email, 'Someone'), l.action, l.target_type, l.target_id, l.diff, l.created_at
			FROM audit_log l
			LEFT JOIN accounts a ON a.id = l.actor_account_id
			LEFT JOIN events e ON e.id = l.event_id
			WHERE l.org_id = $1 AND ($2 = '' OR l.event_id = NULLIF($2, '')::uuid)
			ORDER BY l.created_at DESC, l.id DESC
			LIMIT $3`, sc.OrgID(), eventID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.AuditEntry
			var diff []byte
			if err := rows.Scan(&e.ID, &e.EventID, &e.EventName, &e.ActorID, &e.ActorName, &e.Action,
				&e.TargetType, &e.TargetID, &diff, &e.CreatedAt); err != nil {
				return err
			}
			if err := json.Unmarshal(diff, &e.Diff); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}
