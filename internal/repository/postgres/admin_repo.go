package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type AdminRepo struct{ db *sql.DB }

func NewAdminRepo(db *sql.DB) *AdminRepo { return &AdminRepo{db: db} }

const adminCols = `id, email, password_hash, full_name, role, is_active, force_password_change`

func scanAdmin(row *sql.Row) (*domain.Admin, error) {
	var a domain.Admin
	err := row.Scan(&a.ID, &a.Email, &a.PasswordHash, &a.FullName, &a.Role, &a.IsActive, &a.ForcePasswordChange)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetByEmail expects an already lower-cased email.
func (r *AdminRepo) GetByEmail(ctx context.Context, email string) (*domain.Admin, error) {
	return scanAdmin(r.db.QueryRowContext(ctx, `SELECT `+adminCols+` FROM admins WHERE email = $1`, email))
}

func (r *AdminRepo) GetByID(ctx context.Context, id string) (*domain.Admin, error) {
	return scanAdmin(r.db.QueryRowContext(ctx, `SELECT `+adminCols+` FROM admins WHERE id = $1`, id))
}

// SetPassword stores a password the user chose and clears the temporary
// password flag.
func (r *AdminRepo) SetPassword(ctx context.Context, id, hash string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE admins
		SET password_hash = $2, force_password_change = false, password_changed_at = NOW()
		WHERE id = $1`, id, hash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// TouchLogin records a successful sign-in for the staff table.
func (r *AdminRepo) TouchLogin(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE admins SET last_login_at = NOW() WHERE id = $1`, id)
	return err
}

// AccountState returns the live role and status checked on each request.
func (r *AdminRepo) AccountState(ctx context.Context, id string) (*domain.AccountState, error) {
	var s domain.AccountState
	err := r.db.QueryRowContext(ctx,
		`SELECT id, role, is_active, force_password_change FROM admins WHERE id = $1`, id,
	).Scan(&s.ID, &s.Role, &s.IsActive, &s.ForcePasswordChange)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}
