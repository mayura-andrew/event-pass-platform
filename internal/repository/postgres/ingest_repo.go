package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// IngestRepo adds delegates with membership IDs from participant_membership_seq
// (migrations/004_auto_membership_id.sql).
type IngestRepo struct{ db *sql.DB }

func NewIngestRepo(db *sql.DB) *IngestRepo { return &IngestRepo{db: db} }

const (
	// Unique constraint PostgreSQL named for participants.membership_id (001).
	membershipIDConstraint = "participants_membership_id_key"
	// nextval() past the sequence's MAXVALUE.
	pgSequenceLimitExceeded = "2200H"
	pgCheckViolation        = "23514"
	// A hand-typed ID can still take a number the sequence hands out later;
	// each retry draws fresh numbers past it.
	insertAttempts = 3
)

// GetNextMembershipIDs reserves count membership IDs (NSAC-001000, ...) in one
// round trip. Numbers reserved but never inserted are skipped for good,
// leaving gaps; that's how sequences work and it is harmless.
func (r *IngestRepo) GetNextMembershipIDs(ctx context.Context, count int) ([]string, error) {
	return nextMembershipIDs(ctx, r.db, count)
}

func nextMembershipIDs(ctx context.Context, q queryer, count int) ([]string, error) {
	if count <= 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT 'NSAC-' || LPAD(nextval('participant_membership_seq')::TEXT, 6, '0')
		FROM generate_series(1, $1)`, count)
	if pgCode(err) == pgSequenceLimitExceeded {
		return nil, fmt.Errorf("%w: membership IDs have run out (NSAC-999999 reached)", domain.ErrConflict)
	}
	if err != nil {
		return nil, fmt.Errorf("reserve membership IDs: %w", err)
	}
	defer rows.Close()

	ids := make([]string, 0, count)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		if pgCode(err) == pgSequenceLimitExceeded {
			return nil, fmt.Errorf("%w: membership IDs have run out (NSAC-999999 reached)", domain.ErrConflict)
		}
		return nil, fmt.Errorf("reserve membership IDs: %w", err)
	}
	return ids, nil
}

// ExistingEmails returns which of emails (lower-cased) are already
// registered, mapped to their membership IDs.
func (r *IngestRepo) ExistingEmails(ctx context.Context, emails []string) (map[string]string, error) {
	found := make(map[string]string)
	if len(emails) == 0 {
		return found, nil
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT email, membership_id FROM participants WHERE email = ANY($1::text[])`, emails)
	if err != nil {
		return nil, fmt.Errorf("look up emails: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var email, memberID string
		if err := rows.Scan(&email, &memberID); err != nil {
			return nil, err
		}
		found[email] = memberID
	}
	return found, rows.Err()
}

// FieldDefinitions returns every custom field definition.
func (r *IngestRepo) FieldDefinitions(ctx context.Context) ([]domain.FieldDefinition, error) {
	return listFields(ctx, r.db)
}

// VenueIDs returns the ID of every venue, for validating venue_id.
func (r *IngestRepo) VenueIDs(ctx context.Context) (map[string]bool, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id::text FROM venues`)
	if err != nil {
		return nil, fmt.Errorf("list venues: %w", err)
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// BulkInsertWithAutoID inserts confirmed delegates in one statement, giving
// each a membership ID from the sequence and the QR signature sign returns
// for it. The rows must already be validated, with distinct emails.
//
// newFields (custom fields first seen in this upload) are registered in the
// same transaction, so a delegate never holds a value for an unknown field.
//
// A row whose email was registered after the caller checked (another upload
// running at the same time) is not inserted; it comes back in skipped.
// created and skipped keep the order of rows.
func (r *IngestRepo) BulkInsertWithAutoID(ctx context.Context, rows []domain.NewParticipant, newFields []domain.NewField, sign func(membershipID, email string) string) (created []domain.CreatedParticipant, skipped []domain.NewParticipant, err error) {
	if len(rows) == 0 {
		return nil, nil, nil
	}
	for attempt := 1; ; attempt++ {
		created, skipped, err = r.insertBatch(ctx, rows, newFields, sign)
		var pgErr *pgconn.PgError
		if attempt < insertAttempts && errors.As(err, &pgErr) &&
			pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == membershipIDConstraint {
			continue
		}
		return created, skipped, err
	}
}

func (r *IngestRepo) insertBatch(ctx context.Context, rows []domain.NewParticipant, newFields []domain.NewField, sign func(membershipID, email string) string) ([]domain.CreatedParticipant, []domain.NewParticipant, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin import tx: %w", err)
	}
	defer tx.Rollback()

	if err := registerFields(ctx, tx, newFields); err != nil {
		return nil, nil, err
	}

	ids, err := nextMembershipIDs(ctx, tx, len(rows))
	if err != nil {
		return nil, nil, err
	}

	// Column arrays for unnest(): one parameter per column however many rows,
	// so the batch size never runs into PostgreSQL's 65535-parameter limit.
	n := len(rows)
	var (
		memberIDs = make([]string, n)
		emails    = make([]string, n)
		names     = make([]string, n)
		teams     = make([]string, n)
		sigs      = make([]string, n)
		venues    = make([]string, n)
		dobs      = make([]string, n)
		customs   = make([]string, n) // JSON objects
	)
	// Insert in email order. Two uploads sharing emails then take the unique
	// index's locks in the same order and one waits for the other, instead
	// of each holding a lock the other needs (deadlock).
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return rows[order[a]].Email < rows[order[b]].Email })
	for k, i := range order {
		p := rows[i]
		memberIDs[k] = ids[i]
		emails[k] = p.Email
		names[k] = p.FullName
		teams[k] = p.TeamName
		sigs[k] = sign(ids[i], p.Email)
		venues[k] = p.VenueID
		dobs[k] = p.DateOfBirth
		customs[k] = "{}"
		if len(p.CustomAttributes) > 0 {
			doc, err := json.Marshal(p.CustomAttributes)
			if err != nil {
				return nil, nil, fmt.Errorf("encode custom attributes: %w", err)
			}
			customs[k] = string(doc)
		}
	}

	res, err := tx.QueryContext(ctx, `
		INSERT INTO participants
			(membership_id, email, full_name, team_name, is_confirmed, qr_signature,
			 assigned_venue_id, date_of_birth, custom_attributes)
		SELECT m, e, n, NULLIF(t, ''), true, s, NULLIF(v, '')::uuid, NULLIF(d, '')::date, c::jsonb
		FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::text[])
		     AS x(m, e, n, t, s, v, d, c)
		ON CONFLICT (email) DO NOTHING
		RETURNING id, email`,
		memberIDs, emails, names, teams, sigs, venues, dobs, customs)
	if err != nil {
		return nil, nil, wrapInsertErr(err)
	}
	inserted := make(map[string]string, n) // email -> participant id
	for res.Next() {
		var id, email string
		if err := res.Scan(&id, &email); err != nil {
			res.Close()
			return nil, nil, err
		}
		inserted[email] = id
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, nil, wrapInsertErr(err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit import: %w", err)
	}

	created := make([]domain.CreatedParticipant, 0, len(inserted))
	var skipped []domain.NewParticipant
	for i, p := range rows {
		id, ok := inserted[p.Email]
		if !ok {
			skipped = append(skipped, p)
			continue
		}
		created = append(created, domain.CreatedParticipant{
			Line:         p.Line,
			ID:           id,
			MembershipID: ids[i],
			FullName:     p.FullName,
			Email:        p.Email,
			TeamName:     p.TeamName,
			DateOfBirth:  p.DateOfBirth,
			VenueID:      p.VenueID,
			QRSignature:  sign(ids[i], p.Email),

			CustomAttributes: p.CustomAttributes,
		})
	}
	return created, skipped, nil
}

// wrapInsertErr turns constraint failures the service can't fully rule out
// in advance into messages an organizer can act on. A membership ID clash is
// returned as is, so BulkInsertWithAutoID can retry it.
func wrapInsertErr(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("insert participants: %w", err)
	}
	switch {
	case pgErr.Code == pgUniqueViolation && pgErr.ConstraintName == membershipIDConstraint:
		return err
	case pgErr.Code == pgForeignKeyViolation:
		return fmt.Errorf("%w: a venue was deleted during the import; reload the page and try again", domain.ErrValidation)
	case pgErr.Code == pgSequenceLimitExceeded:
		return fmt.Errorf("%w: membership IDs have run out (NSAC-999999 reached)", domain.ErrConflict)
	case pgErr.Code == pgCheckViolation && pgErr.ConstraintName == "chk_participants_custom_attrs":
		return fmt.Errorf("%w: a delegate has too much data in extra columns (16 KB at most); shorten or remove some columns", domain.ErrValidation)
	}
	return fmt.Errorf("insert participants: %w", err)
}

// ---- Update mode -----------------------------------------------------------------

// storedParticipant is the part of a delegate an upload can change.
type storedParticipant struct {
	ID, MembershipID, Email string
	FullName, TeamName      string
	VenueID, DateOfBirth    string // "" when unset
	Custom                  map[string]any
}

// mergeUpload applies one upload row to a stored delegate: non-blank values
// overwrite, blank ones keep what is stored, custom fields are merged key by
// key. It returns the merged delegate and the names of what changed (core
// column names, then custom field keys in sorted order); none means the row
// adds nothing.
func mergeUpload(old storedParticipant, p domain.NewParticipant) (storedParticipant, []string) {
	m := old
	var changed []string
	set := func(name string, dst *string, v string) {
		if v != "" && v != *dst {
			*dst = v
			changed = append(changed, name)
		}
	}
	set("full_name", &m.FullName, p.FullName)
	set("team_name", &m.TeamName, p.TeamName)
	set("date_of_birth", &m.DateOfBirth, p.DateOfBirth)
	set("venue_id", &m.VenueID, p.VenueID)

	keys := make([]string, 0, len(p.CustomAttributes))
	for k := range p.CustomAttributes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	copied := false // m.Custom still shares old's map until the first write
	for _, k := range keys {
		v := p.CustomAttributes[k]
		if cur, ok := old.Custom[k]; ok && reflect.DeepEqual(cur, v) {
			continue
		}
		if !copied {
			cp := make(map[string]any, len(old.Custom)+len(keys))
			for ok, ov := range old.Custom {
				cp[ok] = ov
			}
			m.Custom, copied = cp, true
		}
		m.Custom[k] = v
		changed = append(changed, k)
	}
	return m, changed
}

// BulkUpdateByEmail applies update-mode rows to the delegates registered with
// their emails, in one transaction; newFields are registered in it first.
// Email and membership ID are never changed, so QR signatures stay valid.
//
// Returns the delegates that changed, the rows that changed nothing, and the
// rows whose email isn't registered (any longer). Each list keeps row order.
func (r *IngestRepo) BulkUpdateByEmail(ctx context.Context, rows []domain.NewParticipant, newFields []domain.NewField) (updated []domain.UpdatedParticipant, unchanged, missing []domain.NewParticipant, err error) {
	if len(rows) == 0 {
		return nil, nil, nil, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin update tx: %w", err)
	}
	defer tx.Rollback()

	if err := registerFields(ctx, tx, newFields); err != nil {
		return nil, nil, nil, err
	}

	emails := make([]string, len(rows))
	for i, p := range rows {
		emails[i] = p.Email
	}
	// Locked in email order, like the insert path, so two overlapping
	// uploads queue instead of deadlocking.
	res, err := tx.QueryContext(ctx, `
		SELECT id, membership_id, email, full_name, COALESCE(team_name, ''),
		       COALESCE(assigned_venue_id::text, ''), COALESCE(to_char(date_of_birth, 'YYYY-MM-DD'), ''),
		       custom_attributes
		FROM participants
		WHERE email = ANY($1::text[])
		ORDER BY email
		FOR UPDATE`, emails)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("lock delegates: %w", err)
	}
	stored := make(map[string]storedParticipant, len(rows))
	for res.Next() {
		var s storedParticipant
		var custom []byte
		if err := res.Scan(&s.ID, &s.MembershipID, &s.Email, &s.FullName, &s.TeamName,
			&s.VenueID, &s.DateOfBirth, &custom); err != nil {
			res.Close()
			return nil, nil, nil, err
		}
		if err := json.Unmarshal(custom, &s.Custom); err != nil {
			res.Close()
			return nil, nil, nil, fmt.Errorf("decode custom attributes of %s: %w", s.Email, err)
		}
		stored[s.Email] = s
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, nil, nil, fmt.Errorf("lock delegates: %w", err)
	}

	var ids, names, teams, venues, dobs, customs []string
	for _, p := range rows {
		old, ok := stored[p.Email]
		if !ok {
			missing = append(missing, p)
			continue
		}
		merged, changed := mergeUpload(old, p)
		if len(changed) == 0 {
			unchanged = append(unchanged, p)
			continue
		}
		doc := []byte("{}")
		if len(merged.Custom) > 0 {
			if doc, err = json.Marshal(merged.Custom); err != nil {
				return nil, nil, nil, fmt.Errorf("encode custom attributes: %w", err)
			}
		}
		ids = append(ids, merged.ID)
		names = append(names, merged.FullName)
		teams = append(teams, merged.TeamName)
		venues = append(venues, merged.VenueID)
		dobs = append(dobs, merged.DateOfBirth)
		customs = append(customs, string(doc))
		updated = append(updated, domain.UpdatedParticipant{
			Line: p.Line, ID: merged.ID, MembershipID: merged.MembershipID,
			FullName: merged.FullName, Email: merged.Email, Changed: changed,
		})
	}

	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE participants p SET
				full_name         = x.n,
				team_name         = NULLIF(x.t, ''),
				assigned_venue_id = NULLIF(x.v, '')::uuid,
				date_of_birth     = NULLIF(x.d, '')::date,
				custom_attributes = x.c::jsonb
			FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[])
			     AS x(id, n, t, v, d, c)
			WHERE p.id = x.id`,
			ids, names, teams, venues, dobs, customs); err != nil {
			return nil, nil, nil, wrapInsertErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, nil, fmt.Errorf("commit update: %w", err)
	}
	return updated, unchanged, missing, nil
}
