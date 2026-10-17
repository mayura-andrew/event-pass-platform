package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ScenarioRepo struct{ db *sql.DB }

func NewScenarioRepo(db *sql.DB) *ScenarioRepo { return &ScenarioRepo{db: db} }

const scenarioCols = `id, name, code, COALESCE(description, ''), is_active, allow_duplicates, created_at`

func scanScenario(row interface{ Scan(...any) error }) (domain.Scenario, error) {
	var s domain.Scenario
	err := row.Scan(&s.ID, &s.Name, &s.Code, &s.Description, &s.IsActive, &s.AllowDuplicates, &s.CreatedAt)
	return s, err
}

// List returns all scenarios, or only active ones when activeOnly is set.
func (r *ScenarioRepo) List(ctx context.Context, activeOnly bool) ([]domain.Scenario, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+scenarioCols+` FROM scan_scenarios
		 WHERE ($1 = false OR is_active) ORDER BY created_at`, activeOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.Scenario, 0, 8)
	for rows.Next() {
		s, err := scanScenario(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListActiveGranted returns active scenarios granted to a scanner account.
func (r *ScenarioRepo) ListActiveGranted(ctx context.Context, adminID string) ([]domain.Scenario, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+scenarioCols+` FROM scan_scenarios
		 WHERE is_active
		   AND id IN (SELECT scenario_id FROM staff_scenario_grants WHERE admin_id = $1)
		 ORDER BY created_at`, adminID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]domain.Scenario, 0, 8)
	for rows.Next() {
		s, err := scanScenario(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *ScenarioRepo) Create(ctx context.Context, in domain.ScenarioInput) (*domain.Scenario, error) {
	s, err := scanScenario(r.db.QueryRowContext(ctx, `
		INSERT INTO scan_scenarios (name, code, description, is_active, allow_duplicates)
		VALUES ($1, $2, NULLIF($3, ''), COALESCE($4, true), COALESCE($5, false))
		RETURNING `+scenarioCols,
		*in.Name, *in.Code, deref(in.Description), in.IsActive, in.AllowDuplicates))
	if pgCode(err) == pgUniqueViolation {
		return nil, domain.ErrConflict
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// Update applies only the fields present in the input (PATCH semantics).
func (r *ScenarioRepo) Update(ctx context.Context, id string, in domain.ScenarioInput) (*domain.Scenario, error) {
	s, err := scanScenario(r.db.QueryRowContext(ctx, `
		UPDATE scan_scenarios SET
			name             = COALESCE($2, name),
			code             = COALESCE($3, code),
			description      = COALESCE($4, description),
			is_active        = COALESCE($5, is_active),
			allow_duplicates = COALESCE($6, allow_duplicates)
		WHERE id = $1
		RETURNING `+scenarioCols,
		id, in.Name, in.Code, in.Description, in.IsActive, in.AllowDuplicates))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, domain.ErrNotFound
	case pgCode(err) == pgUniqueViolation:
		return nil, domain.ErrConflict
	case err != nil:
		return nil, err
	}
	return &s, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
