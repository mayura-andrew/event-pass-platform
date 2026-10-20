package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// PassTemplateRepo stores an org's pass templates and their versions
// (migration 012). Versions are append-only.
type PassTemplateRepo struct{ db *sql.DB }

func NewPassTemplateRepo(db *sql.DB) *PassTemplateRepo { return &PassTemplateRepo{db: db} }

const templateCols = `t.id, t.name, t.current_version, v.design, t.created_at, t.updated_at, t.archived_at,
	(SELECT count(*) FROM events e WHERE e.pass_template_id = t.id AND e.deleted_at IS NULL)`

const templateFrom = ` FROM pass_templates t
	JOIN pass_template_versions v ON v.template_id = t.id AND v.version = t.current_version`

func scanTemplate(row interface{ Scan(...any) error }) (domain.PassTemplate, error) {
	var t domain.PassTemplate
	var design []byte
	err := row.Scan(&t.ID, &t.Name, &t.CurrentVersion, &design, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.UsedBy)
	t.Design = design
	return t, err
}

func (r *PassTemplateRepo) List(ctx context.Context, sc authz.OrgScope) ([]domain.PassTemplate, error) {
	out := []domain.PassTemplate{}
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+templateCols+templateFrom+`
			WHERE t.org_id = $1 ORDER BY t.archived_at IS NOT NULL, lower(t.name)`, sc.OrgID())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			t, err := scanTemplate(rows)
			if err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// Get returns a template with its version history.
func (r *PassTemplateRepo) Get(ctx context.Context, sc authz.OrgScope, id string) (*domain.PassTemplate, error) {
	var t domain.PassTemplate
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		var err error
		if t, err = scanTemplate(tx.QueryRowContext(ctx, `SELECT `+templateCols+templateFrom+`
			WHERE t.org_id = $1 AND t.id = $2`, sc.OrgID(), id)); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT v.version, v.created_at, COALESCE(NULLIF(a.name, ''), a.email, '')
			FROM pass_template_versions v LEFT JOIN accounts a ON a.id = v.created_by
			WHERE v.template_id = $1 ORDER BY v.version DESC`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v domain.PassTemplateVersion
			if err := rows.Scan(&v.Version, &v.CreatedAt, &v.CreatedBy); err != nil {
				return err
			}
			t.Versions = append(t.Versions, v)
		}
		return rows.Err()
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// Create adds a template with design as version 1.
func (r *PassTemplateRepo) Create(ctx context.Context, sc authz.OrgScope, name string, design json.RawMessage) (*domain.PassTemplate, error) {
	id := newID()
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pass_templates (id, org_id, name, created_by) VALUES ($1, $2, $3, $4)`,
			id, sc.OrgID(), name, sc.AccountID()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pass_template_versions (org_id, template_id, version, design, created_by)
			VALUES ($1, $2, 1, $3, $4)`, sc.OrgID(), id, []byte(design), sc.AccountID()); err != nil {
			return err
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: "template.create", TargetType: "template", TargetID: id, Diff: map[string]any{"name": name},
		})
	})
	if err != nil {
		return nil, err
	}
	return r.Get(ctx, sc, id)
}

// SaveVersion stores design as the template's next version (and renames it).
// The template row is locked so two saves get different numbers.
func (r *PassTemplateRepo) SaveVersion(ctx context.Context, sc authz.OrgScope, id, name string, design json.RawMessage) (int, error) {
	var version int
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT current_version + 1 FROM pass_templates WHERE org_id = $1 AND id = $2 AND archived_at IS NULL FOR UPDATE`,
			sc.OrgID(), id).Scan(&version)
		if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO pass_template_versions (org_id, template_id, version, design, created_by)
			VALUES ($1, $2, $3, $4, $5)`, sc.OrgID(), id, version, []byte(design), sc.AccountID()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE pass_templates SET current_version = $3, name = $4, updated_at = NOW()
			WHERE org_id = $1 AND id = $2`, sc.OrgID(), id, version, name); err != nil {
			return err
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: "template.version", TargetType: "template", TargetID: id, Diff: map[string]any{"name": name, "version": version},
		})
	})
	return version, err
}

func (r *PassTemplateRepo) SetArchived(ctx context.Context, sc authz.OrgScope, id string, archived bool) error {
	return withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE pass_templates SET archived_at = CASE WHEN $3 THEN NOW() ELSE NULL END, updated_at = NOW()
			WHERE org_id = $1 AND id = $2`, sc.OrgID(), id, archived)
		if pgCode(err) == pgInvalidTextRepr {
			return domain.ErrNotFound
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return domain.ErrNotFound
		}
		action := "template.restore"
		if archived {
			action = "template.archive"
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{Action: action, TargetType: "template", TargetID: id})
	})
}

// VersionDesign returns one version's design.
func (r *PassTemplateRepo) VersionDesign(ctx context.Context, sc authz.OrgScope, id string, version int) (json.RawMessage, error) {
	var design []byte
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT design FROM pass_template_versions WHERE org_id = $1 AND template_id = $2 AND version = $3`,
			sc.OrgID(), id, version).Scan(&design)
	})
	if errors.Is(err, sql.ErrNoRows) || pgCode(err) == pgInvalidTextRepr {
		return nil, domain.ErrNotFound
	}
	return design, err
}

// SetEventTemplate pins an event to a template version, or (templateID "")
// back to Classic.
func (r *PassTemplateRepo) SetEventTemplate(ctx context.Context, es authz.EventScope, templateID string, version int) error {
	return withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		var err error
		if templateID == "" {
			_, err = tx.ExecContext(ctx, `UPDATE events SET pass_template_id = NULL, pass_template_version = NULL, updated_at = NOW()
				WHERE org_id = $1 AND id = $2`, es.OrgID(), es.EventID())
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE events SET pass_template_id = $3, pass_template_version = $4, updated_at = NOW()
				WHERE org_id = $1 AND id = $2`, es.OrgID(), es.EventID(), templateID, version)
		}
		if pgCode(err) == pgForeignKeyViolation || pgCode(err) == pgInvalidTextRepr {
			return fmt.Errorf("%w: that template version doesn't exist in this organization", domain.ErrValidation)
		}
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.pass_template", TargetType: "event", TargetID: es.EventID(),
			Diff: map[string]any{"template": templateID, "version": version},
		})
	})
}

// QueuePasses queues the pass email again for every registered participant
// (after a design change), returning how many.
func (r *PassTemplateRepo) QueuePasses(ctx context.Context, es authz.EventScope) (int, error) {
	var n int
	err := withTenant(ctx, r.db, es.OrgID(), es.AccountID(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO outbox (id, org_id, event_id, participant_id, to_email, template_key)
			SELECT gen_random_uuid(), org_id, event_id, id, email, 'pass_issued'
			FROM event_participants WHERE org_id = $1 AND event_id = $2 AND status = 'registered'`, es.OrgID(), es.EventID())
		if err != nil {
			return err
		}
		c, _ := res.RowsAffected()
		n = int(c)
		return insertAudit(ctx, tx, es.OrgID(), es.AccountID(), domain.Audit{
			EventID: es.EventID(), Action: "event.passes.resend", TargetType: "event", TargetID: es.EventID(), Diff: map[string]any{"emails": n},
		})
	})
	return n, err
}
