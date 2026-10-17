package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type StaffRepo struct{ db *sql.DB }

func NewStaffRepo(db *sql.DB) *StaffRepo { return &StaffRepo{db: db} }

// queryer is satisfied by both *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ---- Create --------------------------------------------------------------------

// CreateStaff inserts the account and its grants, then calls beforeCommit
// (which emails the credentials) while the transaction is still open. If the
// email can't be sent, everything rolls back, so there is never an account
// whose owner was never told its password.
func (r *StaffRepo) CreateStaff(ctx context.Context, n domain.NewStaff, beforeCommit func(*domain.StaffMember) error) (*domain.StaffMember, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin staff tx: %w", err)
	}
	defer tx.Rollback()

	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO admins (email, password_hash, full_name, role, is_active, force_password_change, created_by)
		VALUES ($1, $2, $3, $4, true, true, $5)
		RETURNING id`,
		n.Email, n.PasswordHash, n.FullName, n.Role, nullIfEmpty(n.CreatedBy),
	).Scan(&id)
	if pgCode(err) == pgUniqueViolation {
		return nil, fmt.Errorf("%w: an account with email %s already exists", domain.ErrConflict, n.Email)
	}
	if err != nil {
		return nil, fmt.Errorf("insert staff: %w", err)
	}

	if err := replaceGrants(ctx, tx, id, n.ScenarioIDs, n.CreatedBy); err != nil {
		return nil, err
	}

	member, err := getStaff(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := beforeCommit(member); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit staff: %w", err)
	}
	return member, nil
}

// replaceGrants sets an account's checkpoints to exactly scenarioIDs. Unknown
// scenario IDs are a validation error rather than silently dropped.
func replaceGrants(ctx context.Context, tx *sql.Tx, adminID string, scenarioIDs []string, assignedBy string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM staff_scenario_grants WHERE admin_id = $1`, adminID); err != nil {
		return fmt.Errorf("clear grants: %w", err)
	}
	if len(scenarioIDs) == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO staff_scenario_grants (admin_id, scenario_id, assigned_by)
		SELECT $1, s.id, $3
		FROM scan_scenarios s
		WHERE s.id = ANY($2::uuid[])`,
		adminID, scenarioIDs, nullIfEmpty(assignedBy))
	if err != nil {
		return fmt.Errorf("insert grants: %w", err)
	}
	if n, _ := res.RowsAffected(); int(n) != len(scenarioIDs) {
		return fmt.Errorf("%w: one or more checkpoints no longer exist; reload and try again", domain.ErrValidation)
	}
	return nil
}

// ---- Read ----------------------------------------------------------------------

const staffSelect = `
	SELECT a.id, a.full_name, a.email, a.role, a.is_active, a.force_password_change,
	       a.last_login_at, a.created_at, COALESCE(c.full_name, '')
	FROM admins a
	LEFT JOIN admins c ON c.id = a.created_by`

func scanStaffRows(rows *sql.Rows) ([]domain.StaffMember, error) {
	defer rows.Close()
	out := make([]domain.StaffMember, 0, 32)
	for rows.Next() {
		var (
			m         domain.StaffMember
			lastLogin sql.NullTime
		)
		if err := rows.Scan(&m.ID, &m.FullName, &m.Email, &m.Role, &m.IsActive, &m.ForcePasswordChange,
			&lastLogin, &m.CreatedAt, &m.CreatedByName); err != nil {
			return nil, err
		}
		if lastLogin.Valid {
			t := lastLogin.Time
			m.LastLoginAt = &t
		}
		m.Grants = []domain.StaffGrant{}
		out = append(out, m)
	}
	return out, rows.Err()
}

// attachGrants loads grants for the given members in one query.
func attachGrants(ctx context.Context, q queryer, members []domain.StaffMember) error {
	if len(members) == 0 {
		return nil
	}
	ids := make([]string, len(members))
	idx := make(map[string]int, len(members))
	for i, m := range members {
		ids[i] = m.ID
		idx[m.ID] = i
	}
	rows, err := q.QueryContext(ctx, `
		SELECT g.admin_id, s.id, s.name, s.code, s.is_active
		FROM staff_scenario_grants g
		JOIN scan_scenarios s ON s.id = g.scenario_id
		WHERE g.admin_id = ANY($1::uuid[])
		ORDER BY s.created_at, s.name`, ids)
	if err != nil {
		return fmt.Errorf("load grants: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var adminID string
		var g domain.StaffGrant
		if err := rows.Scan(&adminID, &g.ScenarioID, &g.Name, &g.Code, &g.IsActive); err != nil {
			return err
		}
		if i, ok := idx[adminID]; ok {
			members[i].Grants = append(members[i].Grants, g)
		}
	}
	return rows.Err()
}

func getStaff(ctx context.Context, q queryer, id string) (*domain.StaffMember, error) {
	rows, err := q.QueryContext(ctx, staffSelect+` WHERE a.id = $1`, id)
	if err != nil {
		return nil, err
	}
	list, err := scanStaffRows(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, domain.ErrNotFound
	}
	if err := attachGrants(ctx, q, list); err != nil {
		return nil, err
	}
	return &list[0], nil
}

// ListStaff returns every account with its grants: superadmins first, then
// admins, then scanners, each by name.
func (r *StaffRepo) ListStaff(ctx context.Context) ([]domain.StaffMember, error) {
	rows, err := r.db.QueryContext(ctx, staffSelect+`
	ORDER BY CASE a.role WHEN 'superadmin' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, a.full_name, a.email`)
	if err != nil {
		return nil, err
	}
	list, err := scanStaffRows(rows)
	if err != nil {
		return nil, err
	}
	if err := attachGrants(ctx, r.db, list); err != nil {
		return nil, err
	}
	return list, nil
}

func (r *StaffRepo) GetStaff(ctx context.Context, id string) (*domain.StaffMember, error) {
	return getStaff(ctx, r.db, id)
}

// ---- Update --------------------------------------------------------------------

// UpdateStaff applies the non-nil fields. Moving an account to admin or
// superadmin removes its grants, which only apply to scanners.
func (r *StaffRepo) UpdateStaff(ctx context.Context, id string, u domain.StaffUpdateRequest, actorID string) (*domain.StaffMember, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin staff update: %w", err)
	}
	defer tx.Rollback()

	var role string
	err = tx.QueryRowContext(ctx, `
		UPDATE admins SET
			full_name = COALESCE($2, full_name),
			role      = COALESCE($3, role),
			is_active = COALESCE($4, is_active)
		WHERE id = $1
		RETURNING role`,
		id, u.FullName, u.Role, u.IsActive,
	).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update staff: %w", err)
	}

	switch {
	case domain.IsManager(role):
		if err := replaceGrants(ctx, tx, id, nil, actorID); err != nil {
			return nil, err
		}
	case u.ScenarioIDs != nil:
		if err := replaceGrants(ctx, tx, id, *u.ScenarioIDs, actorID); err != nil {
			return nil, err
		}
	}

	member, err := getStaff(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit staff update: %w", err)
	}
	return member, nil
}

// ResetTemporaryPassword replaces the password with a new temporary one and
// emails it via beforeCommit, all or nothing like CreateStaff.
func (r *StaffRepo) ResetTemporaryPassword(ctx context.Context, id, hash string, beforeCommit func(*domain.StaffMember) error) (*domain.StaffMember, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin reset: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE admins SET password_hash = $2, force_password_change = true
		WHERE id = $1`, id, hash)
	if err != nil {
		return nil, fmt.Errorf("reset password: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, domain.ErrNotFound
	}
	member, err := getStaff(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := beforeCommit(member); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reset: %w", err)
	}
	return member, nil
}

// ---- Grant checks --------------------------------------------------------------

// GrantedScenarioIDs returns the scenarios an account may scan.
func (r *StaffRepo) GrantedScenarioIDs(ctx context.Context, adminID string) (map[string]struct{}, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT scenario_id FROM staff_scenario_grants WHERE admin_id = $1`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{}, 8)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
