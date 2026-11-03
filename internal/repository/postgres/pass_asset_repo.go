package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
)

// PassAssetRepo stores uploaded pass images (migration 014).
type PassAssetRepo struct{ db *sql.DB }

func NewPassAssetRepo(db *sql.DB) *PassAssetRepo { return &PassAssetRepo{db: db} }

const assetCols = `id, content_type, width, height, created_at`

func scanAsset(row interface{ Scan(...any) error }) (*domain.PassAsset, error) {
	var a domain.PassAsset
	if err := row.Scan(&a.ID, &a.ContentType, &a.Width, &a.Height, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.Src = "asset:" + a.ID
	return &a, nil
}

// Save stores an image, or returns the org's existing copy of the same one.
func (r *PassAssetRepo) Save(ctx context.Context, sc authz.OrgScope, a *passes.Asset) (*domain.PassAsset, error) {
	sum := sha256.Sum256(a.Data)
	var out *domain.PassAsset
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		id := newID()
		res, err := tx.ExecContext(ctx, `INSERT INTO pass_assets (id, org_id, content_type, width, height, data, sha256, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (org_id, sha256) DO NOTHING`,
			id, sc.OrgID(), a.ContentType, a.Width, a.Height, a.Data, sum[:], sc.AccountID())
		if err != nil {
			return err
		}
		if out, err = scanAsset(tx.QueryRowContext(ctx, `SELECT `+assetCols+` FROM pass_assets WHERE org_id = $1 AND sha256 = $2`,
			sc.OrgID(), sum[:])); err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // already uploaded
		}
		return insertAudit(ctx, tx, sc.OrgID(), sc.AccountID(), domain.Audit{
			Action: "template.asset", TargetType: "pass_asset", TargetID: out.ID,
			Diff: map[string]any{"width": a.Width, "height": a.Height, "bytes": len(a.Data)},
		})
	})
	return out, err
}

// Get returns an asset and its bytes.
func (r *PassAssetRepo) Get(ctx context.Context, sc authz.OrgScope, id string) (*domain.PassAsset, []byte, error) {
	var a *domain.PassAsset
	var data []byte
	err := withTenant(ctx, r.db, sc.OrgID(), sc.AccountID(), func(tx *sql.Tx) error {
		var err error
		row := tx.QueryRowContext(ctx, `SELECT `+assetCols+`, data FROM pass_assets WHERE org_id = $1 AND id::text = $2`, sc.OrgID(), id)
		var x domain.PassAsset
		if err = row.Scan(&x.ID, &x.ContentType, &x.Width, &x.Height, &x.CreatedAt, &data); err != nil {
			return err
		}
		x.Src = "asset:" + x.ID
		a = &x
		return nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, domain.ErrNotFound
	}
	return a, data, err
}

// LoadAssets reads the org's assets by ID (renderer, system path). IDs
// from another org are simply not found.
func (r *PassAssetRepo) LoadAssets(ctx context.Context, orgID string, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(ids) == 0 {
		return out, nil
	}
	err := withTenant(ctx, r.db, orgID, "", func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id::text, data FROM pass_assets WHERE org_id = $1 AND id::text = ANY($2::text[])`,
			orgID, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			var data []byte
			if err := rows.Scan(&id, &data); err != nil {
				return err
			}
			out[id] = data
		}
		return rows.Err()
	})
	return out, err
}
