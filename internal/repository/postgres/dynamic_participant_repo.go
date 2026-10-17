package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// FieldRepo stores custom delegate field definitions
// (participant_field_definitions) and their values in
// participants.custom_attributes. See migrations/005_jsonb_dynamic_schema.sql.
type FieldRepo struct{ db *sql.DB }

func NewFieldRepo(db *sql.DB) *FieldRepo { return &FieldRepo{db: db} }

const fieldSelect = `
	SELECT id, field_key, field_label, data_type, show_in_scanner_popup,
	       show_in_dashboard_filter, sort_order, source, created_at
	FROM participant_field_definitions`

const fieldOrder = ` ORDER BY sort_order, field_label, field_key`

func scanFields(rows *sql.Rows) ([]domain.FieldDefinition, error) {
	defer rows.Close()
	out := make([]domain.FieldDefinition, 0, 16)
	for rows.Next() {
		var f domain.FieldDefinition
		if err := rows.Scan(&f.ID, &f.Key, &f.Label, &f.DataType, &f.ShowInScannerPopup,
			&f.ShowInDashboardFilter, &f.SortOrder, &f.Source, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func listFields(ctx context.Context, q queryer) ([]domain.FieldDefinition, error) {
	rows, err := q.QueryContext(ctx, fieldSelect+fieldOrder)
	if err != nil {
		return nil, fmt.Errorf("list fields: %w", err)
	}
	return scanFields(rows)
}

// ListFields returns every definition in display order.
func (r *FieldRepo) ListFields(ctx context.Context) ([]domain.FieldDefinition, error) {
	return listFields(ctx, r.db)
}

// ListFieldsWithUsage is ListFields plus, for the admin pages, how many
// delegates have a value for each field and, for dashboard filter fields,
// the most common values (for the filter dropdown).
func (r *FieldRepo) ListFieldsWithUsage(ctx context.Context) ([]domain.FieldDefinition, error) {
	fields, err := listFields(ctx, r.db)
	if err != nil {
		return nil, err
	}

	usage := make(map[string]int, len(fields))
	rows, err := r.db.QueryContext(ctx, `
		SELECT k, count(*)
		FROM participants p, jsonb_object_keys(p.custom_attributes) AS k
		GROUP BY k`)
	if err != nil {
		return nil, fmt.Errorf("count field usage: %w", err)
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return nil, err
		}
		usage[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range fields {
		f := &fields[i]
		f.UsageCount = usage[f.Key]
		if !f.ShowInDashboardFilter || f.UsageCount == 0 {
			continue
		}
		if f.Options, f.OptionsTruncated, err = r.fieldOptions(ctx, f.Key); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

// fieldOptions returns the most common values of one field, as text. One
// more than the limit is read to know whether the list is complete.
func (r *FieldRepo) fieldOptions(ctx context.Context, key string) ([]string, bool, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT p.custom_attributes ->> $1::text
		FROM participants p
		WHERE p.custom_attributes ? $1::text
		GROUP BY 1
		ORDER BY count(*) DESC, 1
		LIMIT $2`, key, domain.MaxFilterOptions+1)
	if err != nil {
		return nil, false, fmt.Errorf("field options: %w", err)
	}
	defer rows.Close()
	opts := make([]string, 0, 8)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, false, err
		}
		opts = append(opts, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(opts) > domain.MaxFilterOptions {
		return opts[:domain.MaxFilterOptions], true, nil
	}
	return opts, false, nil
}

// ScannerFields returns the fields shown in the scanner popup, in order.
func (r *FieldRepo) ScannerFields(ctx context.Context) ([]domain.ScannerField, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT field_key, field_label, data_type
		FROM participant_field_definitions
		WHERE show_in_scanner_popup`+fieldOrder)
	if err != nil {
		return nil, fmt.Errorf("scanner fields: %w", err)
	}
	defer rows.Close()
	out := make([]domain.ScannerField, 0, 8)
	for rows.Next() {
		var f domain.ScannerField
		if err := rows.Scan(&f.Key, &f.Label, &f.DataType); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// CountFields returns how many definitions exist.
func (r *FieldRepo) CountFields(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM participant_field_definitions`).Scan(&n)
	return n, err
}

// CreateField adds one definition.
func (r *FieldRepo) CreateField(ctx context.Context, f domain.NewField) (*domain.FieldDefinition, error) {
	rows, err := r.db.QueryContext(ctx, `
		INSERT INTO participant_field_definitions
			(field_key, field_label, data_type, show_in_scanner_popup, show_in_dashboard_filter,
			 sort_order, source, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, field_key, field_label, data_type, show_in_scanner_popup,
		          show_in_dashboard_filter, sort_order, source, created_at`,
		f.Key, f.Label, f.DataType, f.ShowInScannerPopup, f.ShowInDashboardFilter,
		f.SortOrder, f.Source, nullIfEmpty(f.CreatedBy))
	if pgCode(err) == pgUniqueViolation {
		return nil, fmt.Errorf("%w: a field with key %s already exists", domain.ErrConflict, f.Key)
	}
	if err != nil {
		return nil, fmt.Errorf("create field: %w", err)
	}
	list, err := scanFields(rows)
	if pgCode(err) == pgUniqueViolation {
		return nil, fmt.Errorf("%w: a field with key %s already exists", domain.ErrConflict, f.Key)
	}
	if err != nil {
		return nil, fmt.Errorf("create field: %w", err)
	}
	return &list[0], nil
}

// UpdateField applies the non-nil parts of u. The data type can only change
// while no delegate has a value, so stored values always match their type.
func (r *FieldRepo) UpdateField(ctx context.Context, key string, u domain.FieldUpdateRequest) (*domain.FieldDefinition, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin field tx: %w", err)
	}
	defer tx.Rollback()

	var current string
	err = tx.QueryRowContext(ctx, `
		SELECT data_type FROM participant_field_definitions WHERE field_key = $1 FOR UPDATE`, key).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load field: %w", err)
	}
	if u.DataType != nil && *u.DataType != current {
		var used bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM participants WHERE custom_attributes ? $1::text)`, key).Scan(&used); err != nil {
			return nil, fmt.Errorf("check field usage: %w", err)
		}
		if used {
			return nil, fmt.Errorf("%w: delegates already have values for this field, so its type can't change; "+
				"create a new field instead", domain.ErrConflict)
		}
	}

	rows, err := tx.QueryContext(ctx, `
		UPDATE participant_field_definitions SET
			field_label              = COALESCE($2, field_label),
			data_type                = COALESCE($3, data_type),
			show_in_scanner_popup    = COALESCE($4, show_in_scanner_popup),
			show_in_dashboard_filter = COALESCE($5, show_in_dashboard_filter),
			sort_order               = COALESCE($6, sort_order),
			updated_at               = NOW()
		WHERE field_key = $1
		RETURNING id, field_key, field_label, data_type, show_in_scanner_popup,
		          show_in_dashboard_filter, sort_order, source, created_at`,
		key, u.Label, u.DataType, u.ShowInScannerPopup, u.ShowInDashboardFilter, u.SortOrder)
	if err != nil {
		return nil, fmt.Errorf("update field: %w", err)
	}
	list, err := scanFields(rows)
	if err != nil {
		return nil, fmt.Errorf("update field: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit field: %w", err)
	}
	return &list[0], nil
}

// DeleteField removes a definition and every delegate's value for it, in one
// transaction, and returns how many delegates had a value. Deleting the
// definition alone would leave data nobody can see or filter on.
func (r *FieldRepo) DeleteField(ctx context.Context, key string) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin field tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM participant_field_definitions WHERE field_key = $1`, key)
	if err != nil {
		return 0, fmt.Errorf("delete field: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, domain.ErrNotFound
	}
	res, err = tx.ExecContext(ctx, `
		UPDATE participants SET custom_attributes = custom_attributes - $1::text
		WHERE custom_attributes ? $1::text`, key)
	if err != nil {
		return 0, fmt.Errorf("remove field values: %w", err)
	}
	cleared, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit field delete: %w", err)
	}
	return int(cleared), nil
}

// registerFields adds definitions for custom columns first seen in an upload.
// A field registered meanwhile by someone else keeps its settings.
func registerFields(ctx context.Context, tx *sql.Tx, fields []domain.NewField) error {
	for _, f := range fields {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO participant_field_definitions (field_key, field_label, data_type, sort_order, source)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (field_key) DO NOTHING`,
			f.Key, f.Label, f.DataType, f.SortOrder, f.Source); err != nil {
			return fmt.Errorf("register field %s: %w", f.Key, err)
		}
	}
	return nil
}

// ---- Dynamic filter ------------------------------------------------------------

// andCustomFilters adds one WHERE clause per custom field filter. Keys and
// values are bound parameters, never SQL text, and the service has already
// checked each key against the field definitions and converted each value to
// the field's type (Typed).
//
//   - eq:       custom_attributes @> '{"key": value}'. Containment is what the
//     GIN index idx_participants_custom_attrs serves; ->>'key' = 'value'
//     would read every row.
//   - contains: custom_attributes ->> 'key' ILIKE '%value%' (text fields).
//   - missing:  NOT custom_attributes ? 'key'.
func andCustomFilters(b *queryBuilder, filters []domain.CustomFilter) error {
	for _, f := range filters {
		switch f.Op {
		case domain.CustomEquals:
			doc, err := json.Marshal(map[string]any{f.Key: f.Typed})
			if err != nil {
				return fmt.Errorf("encode filter %s: %w", f.Key, err)
			}
			b.and(`p.custom_attributes @> ` + b.arg(string(doc)) + `::jsonb`)
		case domain.CustomContains:
			b.and(`p.custom_attributes ->> ` + b.arg(f.Key) + `::text ILIKE ` +
				b.arg("%"+escapeLike(f.Value)+"%"))
		case domain.CustomMissing:
			b.and(`NOT (p.custom_attributes ? ` + b.arg(f.Key) + `::text)`)
		default:
			return fmt.Errorf("unknown custom filter operator %q", f.Op)
		}
	}
	return nil
}
