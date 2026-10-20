package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// EventRepo stores events, shares, stars, opens and links (migration 008).
// Every query runs through withTenant; changes write their audit entry in
// the same transaction.
type EventRepo struct{ db *sql.DB }

func NewEventRepo(db *sql.DB) *EventRepo { return &EventRepo{db: db} }

const eventCols = `e.id, e.org_id, e.slug, e.name, e.type, e.status, e.description, e.starts_at, e.ends_at,
	e.time_zone, e.venue, e.online_url, e.capacity, e.visibility, e.language, e.tags, e.settings,
	e.owner_id, COALESCE(oa.name, ''), COALESCE(oa.email, ''), COALESCE(oa.avatar_url, ''),
	e.created_at, e.updated_at, e.archived_at, e.deleted_at`

// eventFrom joins the owner's account; it can be missing from view when the
// owner has left the org, hence the LEFT JOIN and COALESCEs above.
const eventFrom = ` FROM events e LEFT JOIN accounts oa ON oa.id = e.owner_id`

// eventDest lists scan targets in eventCols order; venue and settings are
// JSON, decoded by decodeEvent after the scan.
func eventDest(e *domain.Event, venue, settings *[]byte) []any {
	return []any{&e.ID, &e.OrgID, &e.Slug, &e.Name, &e.Type, &e.Status, &e.Description, &e.StartsAt, &e.EndsAt,
		&e.TimeZone, venue, &e.OnlineURL, &e.Capacity, &e.Visibility, &e.Language, textArray{&e.Tags}, settings,
		&e.OwnerID, &e.OwnerName, &e.OwnerEmail, &e.OwnerAvatar,
		&e.CreatedAt, &e.UpdatedAt, &e.ArchivedAt, &e.DeletedAt}
}

func decodeEvent(e *domain.Event, venue, settings []byte) error {
	if err := json.Unmarshal(venue, &e.Venue); err != nil {
		return err
	}
	return json.Unmarshal(settings, &e.Settings)
}

// textArray adapts []string to and from a Postgres text[] through the pgx
// database/sql driver, which reads arrays as their text form.
type textArray struct{ p *[]string }

func (a textArray) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case nil:
		*a.p = []string{}
		return nil
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("text[]: unexpected %T", src)
	}
	*a.p = parseTextArray(s)
	return nil
}

// parseTextArray reads {a,"b c"}; tags are normalized (no quotes or commas
// inside), so the simple form is enough, but quoted items are handled.
func parseTextArray(s string) []string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
	out := []string{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.Trim(part, `"`)
		out = append(out, strings.ReplaceAll(part, `\"`, `"`))
	}
	return out
}

func getEvent(ctx context.Context, tx *sql.Tx, orgID, eventID string) (*domain.Event, error) {
	var e domain.Event
	var venue, settings []byte
	err := tx.QueryRowContext(ctx, `SELECT `+eventCols+eventFrom+` WHERE e.org_id = $1 AND e.id = $2`,
		orgID, eventID).Scan(eventDest(&e, &venue, &settings)...)
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &e, decodeEvent(&e, venue, settings)
}

// EventAccess returns an event of the org and the caller's own share role.
// Trashed events are included: the service decides who may see them.
func (r *EventRepo) EventAccess(ctx context.Context, sc authz.OrgScope, eventID string) (*domain.EventAccess, error) {
	var acc domain.EventAccess
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		ev, err := getEvent(ctx, tx, sc.OrgID(), eventID)
		if err != nil {
			return err
		}
		acc.Event = ev
		err = tx.QueryRowContext(ctx, `SELECT role FROM event_shares WHERE event_id = $1 AND account_id = $2`,
			eventID, sc.AccountID()).Scan(&acc.ShareRole)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &acc, nil
}

// freeSlug returns base, or base-2, base-3... whichever is free in the org.
func freeSlug(ctx context.Context, tx *sql.Tx, orgID, base string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slug FROM events WHERE org_id = $1 AND (slug = $2 OR slug LIKE $3)`,
		orgID, base, escapeLike(base)+"-%")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
		taken[s] = true
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	slug := base
	for i := 2; taken[slug]; i++ {
		suffix := fmt.Sprintf("-%d", i)
		b := base
		if len(b)+len(suffix) > 60 {
			b = strings.TrimRight(b[:60-len(suffix)], "-")
		}
		slug = b + suffix
	}
	return slug, nil
}

// CreateEvent inserts e (owned by the caller) with a free slug based on
// e.Slug. With copySharesFrom set, that event's people (not its link) get the
// same roles on the new one: "duplicate keeps the team".
func (r *EventRepo) CreateEvent(ctx context.Context, sc authz.OrgScope, e *domain.Event, copySharesFrom string, a domain.Audit) (*domain.Event, error) {
	var out *domain.Event
	venue, _ := json.Marshal(e.Venue)
	settings, _ := json.Marshal(e.Settings)
	id := newID()
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		slug, err := freeSlug(ctx, tx, sc.OrgID(), e.Slug)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO events (id, org_id, slug, name, type, description, starts_at, ends_at, time_zone, venue,
			                    online_url, capacity, visibility, language, tags, settings, owner_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::text[], $16, $17, $17)`,
			id, sc.OrgID(), slug, e.Name, e.Type, e.Description, e.StartsAt, e.EndsAt, e.TimeZone, venue,
			e.OnlineURL, e.Capacity, e.Visibility, e.Language, pgTextArray(e.Tags), settings, sc.AccountID()); err != nil {
			if pgCode(err) == pgUniqueViolation {
				return fmt.Errorf("%w: another event was just created with this address; try again", domain.ErrConflict)
			}
			return err
		}
		if copySharesFrom != "" {
			rows, err := tx.QueryContext(ctx, `
				SELECT account_id, role FROM event_shares
				WHERE org_id = $1 AND event_id = $2 AND account_id IS NOT NULL AND account_id <> $3`,
				sc.OrgID(), copySharesFrom, sc.AccountID())
			if err != nil {
				return err
			}
			type share struct{ account, role string }
			var shares []share
			for rows.Next() {
				var s share
				if err := rows.Scan(&s.account, &s.role); err != nil {
					rows.Close()
					return err
				}
				shares = append(shares, s)
			}
			rows.Close()
			// The source's owner, if not the caller, keeps editing the copy.
			var srcOwner string
			if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM events WHERE org_id = $1 AND id = $2`,
				sc.OrgID(), copySharesFrom).Scan(&srcOwner); err != nil {
				return err
			}
			if srcOwner != sc.AccountID() {
				shares = append(shares, share{srcOwner, domain.EventRoleEditor})
			}
			for _, s := range shares {
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO event_shares (id, org_id, event_id, account_id, role, granted_by)
					VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (event_id, account_id) DO NOTHING`,
					newID(), sc.OrgID(), id, s.account, s.role, sc.AccountID()); err != nil {
					return err
				}
			}
		}
		a.EventID, a.TargetID = id, id
		if err := insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), a); err != nil {
			return err
		}
		out, err = getEvent(ctx, tx, sc.OrgID(), id)
		return err
	})
	return out, err
}

// pgTextArray formats tags as a text[] literal. Tags are normalized to plain
// words, but quote each anyway.
func pgTextArray(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return "{" + strings.Join(q, ",") + "}"
}

// UpdateEvent writes the editable fields of e.
func (r *EventRepo) UpdateEvent(ctx context.Context, es authz.EventScope, e *domain.Event, a domain.Audit) (*domain.Event, error) {
	var out *domain.Event
	venue, _ := json.Marshal(e.Venue)
	err := r.inEvent(ctx, es, a, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE events SET name = $3, type = $4, description = $5, starts_at = $6, ends_at = $7, time_zone = $8,
			       venue = $9, online_url = $10, capacity = $11, visibility = $12, language = $13, tags = $14::text[],
			       updated_at = NOW()
			WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
			es.OrgID(), es.EventID(), e.Name, e.Type, e.Description, e.StartsAt, e.EndsAt, e.TimeZone,
			venue, e.OnlineURL, e.Capacity, e.Visibility, e.Language, pgTextArray(e.Tags))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		out, err = getEvent(ctx, tx, es.OrgID(), es.EventID())
		return err
	})
	return out, err
}

// inEvent runs fn in the event's org and writes the audit entry after it.
func (r *EventRepo) inEvent(ctx context.Context, es authz.EventScope, a domain.Audit, fn func(*sql.Tx) error) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if a.Action == "" {
			return nil
		}
		a.EventID = es.EventID()
		if a.TargetID == "" {
			a.TargetType, a.TargetID = "event", es.EventID()
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), a)
	})
}

// SetArchived archives or unarchives. Already in that state is a no-op
// without an audit entry.
func (r *EventRepo) SetArchived(ctx context.Context, es authz.EventScope, archived bool) error {
	action := "event.unarchive"
	cond := "archived_at IS NOT NULL"
	set := "archived_at = NULL"
	if archived {
		action, cond, set = "event.archive", "archived_at IS NULL", "archived_at = NOW()"
	}
	return r.toggle(ctx, es, action, set, cond)
}

// SetDeleted moves to Trash or restores (within the retention period).
func (r *EventRepo) SetDeleted(ctx context.Context, es authz.EventScope, deleted bool) error {
	if deleted {
		return r.toggle(ctx, es, "event.delete", "deleted_at = NOW()", "deleted_at IS NULL")
	}
	return r.toggle(ctx, es, "event.restore", "deleted_at = NULL",
		fmt.Sprintf("deleted_at > NOW() - INTERVAL '%d hours'", int(domain.TrashRetention.Hours())))
}

func (r *EventRepo) toggle(ctx context.Context, es authz.EventScope, action, set, cond string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE events SET `+set+`, updated_at = NOW()
			WHERE org_id = $1 AND id = $2 AND `+cond, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // already in that state
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: action, TargetType: "event", TargetID: es.EventID(),
		})
	})
}

func (r *EventRepo) UpdateSettings(ctx context.Context, es authz.EventScope, st domain.EventSettings) error {
	b, _ := json.Marshal(st)
	return r.inEvent(ctx, es, domain.Audit{Action: "event.settings",
		Diff: map[string]any{"editors_can_share": st.EditorsCanShare, "viewers_can_export": st.ViewersCanExport}},
		func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE events SET settings = $3, updated_at = NOW() WHERE org_id = $1 AND id = $2`,
				es.OrgID(), es.EventID(), b)
			return err
		})
}

// TransferOwnership makes newOwnerID (an org member) the owner. The previous
// owner keeps editor access; the new owner's own share is no longer needed.
func (r *EventRepo) TransferOwnership(ctx context.Context, es authz.EventScope, newOwnerID string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		target, err := getMember(ctx, tx, es.OrgID(), newOwnerID)
		if errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("%w: the new owner must be a member of the organization", domain.ErrValidation)
		}
		if err != nil {
			return err
		}
		var prev string
		if err := tx.QueryRowContext(ctx, `SELECT owner_id FROM events WHERE org_id = $1 AND id = $2 FOR UPDATE`,
			es.OrgID(), es.EventID()).Scan(&prev); err != nil {
			return err
		}
		if prev == newOwnerID {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET owner_id = $3, updated_at = NOW() WHERE org_id = $1 AND id = $2`,
			es.OrgID(), es.EventID(), newOwnerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM event_shares WHERE event_id = $1 AND account_id = $2`,
			es.EventID(), newOwnerID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event_shares (id, org_id, event_id, account_id, role, granted_by) VALUES ($1, $2, $3, $4, 'editor', $5)
			ON CONFLICT (event_id, account_id) DO UPDATE SET role = 'editor'`,
			newID(), es.OrgID(), es.EventID(), prev, es.AccountID()); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.transfer", TargetType: "event", TargetID: es.EventID(),
			Diff: map[string]any{"from": prev, "to": newOwnerID, "to_email": target.Email},
		})
	})
}

// SetStarred stars or unstars for the caller (personal, not audited).
func (r *EventRepo) SetStarred(ctx context.Context, es authz.EventScope, starred bool) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		if starred {
			_, err = tx.ExecContext(ctx, `INSERT INTO event_stars (org_id, event_id, account_id) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, es.OrgID(), es.EventID(), es.AccountID())
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM event_stars WHERE event_id = $1 AND account_id = $2`,
				es.EventID(), es.AccountID())
		}
		return err
	})
}

// TouchOpened records that the caller opened the event (for Recent).
func (r *EventRepo) TouchOpened(ctx context.Context, es authz.EventScope) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO event_opens (org_id, event_id, account_id) VALUES ($1, $2, $3)
			ON CONFLICT (account_id, event_id) DO UPDATE SET opened_at = NOW()`,
			es.OrgID(), es.EventID(), es.AccountID())
		return err
	})
}

// ---- Events home -------------------------------------------------------------

// ListEvents returns the events of one view that the caller can open. With
// all set (org owners and admins) every event of the org counts as openable.
func (r *EventRepo) ListEvents(ctx context.Context, sc authz.OrgScope, q domain.EventListQuery, all bool) ([]domain.EventRow, error) {
	where := []string{"e.org_id = $1", "($3 OR e.owner_id = $2 OR s.account_id IS NOT NULL)"}
	live := "e.archived_at IS NULL AND e.deleted_at IS NULL"
	if q.Q != "" {
		live = "e.deleted_at IS NULL" // archived events stay searchable (spec)
	}
	switch q.View {
	case domain.ViewMine:
		where = append(where, "e.owner_id = $2", live)
	case domain.ViewShared:
		where = append(where, "s.account_id IS NOT NULL", "e.owner_id <> $2", live)
	case domain.ViewRecent:
		where = append(where, "op.opened_at IS NOT NULL", "e.deleted_at IS NULL")
	case domain.ViewStarred:
		where = append(where, "st.account_id IS NOT NULL", "e.deleted_at IS NULL")
	case domain.ViewArchived:
		where = append(where, "e.archived_at IS NOT NULL", "e.deleted_at IS NULL")
	case domain.ViewTrash:
		where = append(where, fmt.Sprintf("e.deleted_at > NOW() - INTERVAL '%d hours'", int(domain.TrashRetention.Hours())))
	default: // all
		where = append(where, live)
	}
	args := []any{sc.OrgID(), sc.AccountID(), all}
	if q.Q != "" {
		args = append(args, "%"+escapeLike(q.Q)+"%", strings.ToLower(q.Q))
		where = append(where, `(e.name ILIKE $4 OR e.venue->>'name' ILIKE $4 OR e.venue->>'address' ILIKE $4 OR $5 = ANY (e.tags))`)
	}
	order := map[string]string{
		domain.SortOpened:   "op.opened_at DESC NULLS LAST, e.updated_at DESC",
		domain.SortModified: "e.updated_at DESC",
		domain.SortDate:     "e.starts_at ASC NULLS LAST, lower(e.name)",
		domain.SortName:     "lower(e.name), e.created_at",
	}[q.Sort]
	if order == "" {
		order = "e.updated_at DESC"
	}

	out := []domain.EventRow{}
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+eventCols+`, COALESCE(s.role, ''), st.account_id IS NOT NULL, op.opened_at,
			       (SELECT count(*) FROM event_shares x WHERE x.event_id = e.id AND x.account_id IS NOT NULL),
			       EXISTS (SELECT 1 FROM event_shares x WHERE x.event_id = e.id AND x.link_token_hash IS NOT NULL
			               AND (x.expires_at IS NULL OR x.expires_at > NOW()))
			`+eventFrom+`
			LEFT JOIN event_shares s ON s.event_id = e.id AND s.account_id = $2
			LEFT JOIN event_stars st ON st.event_id = e.id AND st.account_id = $2
			LEFT JOIN event_opens op ON op.event_id = e.id AND op.account_id = $2
			WHERE `+strings.Join(where, " AND ")+`
			ORDER BY `+order+`
			LIMIT 200`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row domain.EventRow
			var venue, settings []byte
			dest := append(eventDest(&row.Event, &venue, &settings), &row.ShareRole, &row.Starred, &row.LastOpenedAt, &row.SharedCount, &row.LinkShared)
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			if err := decodeEvent(&row.Event, venue, settings); err != nil {
				return err
			}
			out = append(out, row)
		}
		return rows.Err()
	})
	return out, err
}

// ---- Sharing -----------------------------------------------------------------

const shareCols = `s.id, a.id, a.email, a.name, COALESCE(a.avatar_url, ''), s.role,
	NOT EXISTS (SELECT 1 FROM org_members m WHERE m.org_id = s.org_id AND m.account_id = s.account_id),
	COALESCE(NULLIF(g.name, ''), g.email, ''), s.created_at`

const shareFrom = ` FROM event_shares s
	JOIN accounts a ON a.id = s.account_id
	LEFT JOIN accounts g ON g.id = s.granted_by`

func scanShare(row interface{ Scan(...any) error }) (domain.EventShare, error) {
	var s domain.EventShare
	err := row.Scan(&s.ID, &s.AccountID, &s.Email, &s.Name, &s.AvatarURL, &s.Role, &s.Guest, &s.GrantedBy, &s.CreatedAt)
	return s, err
}

// Sharing returns the owner, the people with shares, open invites and the
// link's state.
func (r *EventRepo) Sharing(ctx context.Context, es authz.EventScope) (*domain.SharingState, error) {
	st := &domain.SharingState{Shares: []domain.EventShare{}}
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT a.id, a.email, a.name, COALESCE(a.avatar_url, ''), 'owner', e.created_at
			FROM events e JOIN accounts a ON a.id = e.owner_id WHERE e.org_id = $1 AND e.id = $2`,
			es.OrgID(), es.EventID()).Scan(&st.Owner.AccountID, &st.Owner.Email, &st.Owner.Name,
			&st.Owner.AvatarURL, &st.Owner.Role, &st.Owner.JoinedAt); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT `+shareCols+shareFrom+`
			WHERE s.org_id = $1 AND s.event_id = $2 AND s.account_id IS NOT NULL
			ORDER BY CASE s.role WHEN 'editor' THEN 0 WHEN 'checkin' THEN 1 ELSE 2 END,
			         lower(COALESCE(NULLIF(a.name, ''), a.email))`, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		for rows.Next() {
			s, err := scanShare(rows)
			if err != nil {
				rows.Close()
				return err
			}
			st.Shares = append(st.Shares, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var created time.Time
		var expires *time.Time
		err = tx.QueryRowContext(ctx, `
			SELECT created_at, expires_at FROM event_shares
			WHERE event_id = $1 AND link_token_hash IS NOT NULL AND (expires_at IS NULL OR expires_at > NOW())`,
			es.EventID()).Scan(&created, &expires)
		if err == nil {
			st.Link = domain.EventLink{Enabled: true, CreatedAt: &created, ExpiresAt: expires}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	invites, err := listInvites(ctx, r.db, es.OrgID(), es.AccountID(), es.EventID())
	if err != nil {
		return nil, err
	}
	st.Invites = invites
	return st, nil
}

// GetShare returns one share of the event.
func (r *EventRepo) GetShare(ctx context.Context, es authz.EventScope, shareID string) (*domain.EventShare, error) {
	var s domain.EventShare
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		s, err = scanShare(tx.QueryRowContext(ctx, `SELECT `+shareCols+shareFrom+`
			WHERE s.org_id = $1 AND s.event_id = $2 AND s.id = $3`, es.OrgID(), es.EventID(), shareID))
		return err
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// IsMember reports whether the account belongs to the event's org.
func (r *EventRepo) IsMember(ctx context.Context, sc authz.OrgScope, accountID string) (bool, error) {
	var ok bool
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM org_members WHERE org_id = $1 AND account_id = $2)`,
			sc.OrgID(), accountID).Scan(&ok)
	})
	return ok, err
}

// PutShare gives an account a role on the event, or changes it.
func (r *EventRepo) PutShare(ctx context.Context, es authz.EventScope, accountID, role string) (*domain.EventShare, error) {
	var s domain.EventShare
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var prev string
		err := tx.QueryRowContext(ctx, `SELECT role FROM event_shares WHERE event_id = $1 AND account_id = $2`,
			es.EventID(), accountID).Scan(&prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var id string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO event_shares (id, org_id, event_id, account_id, role, granted_by) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (event_id, account_id) DO UPDATE SET role = EXCLUDED.role
			RETURNING id`, newID(), es.OrgID(), es.EventID(), accountID, role, es.AccountID()).Scan(&id); err != nil {
			return err
		}
		if s, err = scanShare(tx.QueryRowContext(ctx, `SELECT `+shareCols+shareFrom+` WHERE s.id = $1`, id)); err != nil {
			return err
		}
		if prev == role {
			return nil
		}
		action, diff := "event.share.add", map[string]any{"email": s.Email, "role": role}
		if prev != "" {
			action, diff = "event.share.role", map[string]any{"email": s.Email, "from": prev, "to": role}
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: action, TargetType: "share", TargetID: id, Diff: diff,
		})
	})
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *EventRepo) RemoveShare(ctx context.Context, es authz.EventScope, shareID string) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var email, role string
		err := tx.QueryRowContext(ctx, `
			DELETE FROM event_shares s USING accounts a
			WHERE a.id = s.account_id AND s.org_id = $1 AND s.event_id = $2 AND s.id = $3
			RETURNING a.email, s.role`, es.OrgID(), es.EventID(), shareID).Scan(&email, &role)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.share.remove", TargetType: "share", TargetID: shareID,
			Diff: map[string]any{"email": email, "role": role},
		})
	})
}

// CreateInvite stores an open invite to the event for an address with no
// account, replacing an earlier open one to the same address.
func (r *EventRepo) CreateInvite(ctx context.Context, es authz.EventScope, email, role string, tokenHash []byte, expires time.Time) (*domain.Invite, error) {
	var inv domain.Invite
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO invites (id, org_id, event_id, email, role, token_hash, invited_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (event_id, email) WHERE event_id IS NOT NULL AND accepted_at IS NULL AND revoked_at IS NULL
			DO UPDATE SET role = EXCLUDED.role, token_hash = EXCLUDED.token_hash,
			              invited_by = EXCLUDED.invited_by, expires_at = EXCLUDED.expires_at, created_at = NOW()
			RETURNING id`,
			newID(), es.OrgID(), es.EventID(), email, role, tokenHash, es.AccountID(), expires).Scan(&id); err != nil {
			return err
		}
		var err error
		if inv, err = getInvite(ctx, tx, id); err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.invite.create", TargetType: "invite", TargetID: id,
			Diff: map[string]any{"email": email, "role": role},
		})
	})
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (r *EventRepo) RevokeInvite(ctx context.Context, es authz.EventScope, inviteID string) error {
	return revokeInvite(ctx, r.db, es.OrgID(), es.AccountID(), es.EventID(), inviteID)
}

// SetLink turns on the view-only link with a new token (replacing any old
// link, so "reset" invalidates the previous URL).
func (r *EventRepo) SetLink(ctx context.Context, es authz.EventScope, tokenHash []byte, expires *time.Time) error {
	diff := map[string]any{}
	if expires != nil {
		diff["expires_at"] = expires.UTC().Format(time.RFC3339)
	}
	return r.inEvent(ctx, es, domain.Audit{Action: "event.link.on", TargetType: "link", TargetID: es.EventID(), Diff: diff},
		func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM event_shares WHERE event_id = $1 AND link_token_hash IS NOT NULL`,
				es.EventID()); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO event_shares (id, org_id, event_id, link_token_hash, role, granted_by, expires_at)
				VALUES ($1, $2, $3, $4, 'viewer', $5, $6)`,
				newID(), es.OrgID(), es.EventID(), tokenHash, es.AccountID(), expires)
			return err
		})
}

// RemoveLink turns the link off. Already off is a no-op.
func (r *EventRepo) RemoveLink(ctx context.Context, es authz.EventScope) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM event_shares WHERE event_id = $1 AND link_token_hash IS NOT NULL`,
			es.EventID())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.link.off", TargetType: "link", TargetID: es.EventID(),
		})
	})
}

// EventByLink returns the event behind a view-only link token and its org's
// name. The token is the credential, so this runs outside withTenant.
func (r *EventRepo) EventByLink(ctx context.Context, tokenHash []byte) (*domain.Event, string, error) {
	var e domain.Event
	var venue, settings []byte
	var orgName string
	dest := append(eventDest(&e, &venue, &settings), &orgName)
	err := r.db.QueryRowContext(ctx, `SELECT `+eventCols+`, o.name`+eventFrom+`
		JOIN event_shares s ON s.event_id = e.id AND s.org_id = e.org_id
		JOIN orgs o ON o.id = e.org_id AND o.status = 'active'
		WHERE s.link_token_hash = $1 AND (s.expires_at IS NULL OR s.expires_at > NOW()) AND e.deleted_at IS NULL`,
		tokenHash).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", domain.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	return &e, orgName, decodeEvent(&e, venue, settings)
}
