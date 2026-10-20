package postgres

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// tenantRole is the database role tenant transactions run as. It has no
// BYPASSRLS, so the policies in migration 007 apply even though the API
// connects as the tables' owner.
const tenantRole = "nsac_tenant"

// withTenant runs fn in a transaction scoped to one org and account: wall 2 of
// tenant isolation. Row Level Security then limits every statement in fn to
// rows of orgID, whatever its WHERE clause says. orgID may be empty for an
// account acting outside any org (listing its own orgs).
func withTenant(ctx context.Context, db *sql.DB, orgID, accountID string, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+tenantRole); err != nil {
		return fmt.Errorf("switch to tenant role: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`SELECT set_config('app.org_id', $1, true), set_config('app.account_id', $2, true)`,
		orgID, accountID); err != nil {
		return fmt.Errorf("set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// newID returns a UUIDv7 (RFC 9562): a millisecond timestamp then random
// bits, so new rows sort by creation time and index inserts stay local.
func newID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// insertAudit appends to the audit log inside the change's own transaction,
// so a change and its record commit or fail together.
func insertAudit(ctx context.Context, tx *sql.Tx, orgID, actorID string, a domain.Audit) error {
	diff := a.Diff
	if diff == nil {
		diff = map[string]any{}
	}
	b, err := json.Marshal(diff)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_log (id, org_id, event_id, actor_account_id, action, target_type, target_id, diff, ip)
		VALUES ($1, $2, NULLIF($3, '')::uuid, NULLIF($4, '')::uuid, $5, $6, $7, $8, $9)`,
		newID(), orgID, a.EventID, actorID, a.Action, a.TargetType, a.TargetID, b, domain.ClientIP(ctx))
	return err
}
