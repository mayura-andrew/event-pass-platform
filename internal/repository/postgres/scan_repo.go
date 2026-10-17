package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ScanRepo struct{ db *sql.DB }

func NewScanRepo(db *sql.DB) *ScanRepo { return &ScanRepo{db: db} }

// insertScanSQL records a scan, copying the scenario's duplicate rule into
// enforce_unique so the partial unique index applies only to single-claim
// scenarios.
//
// ON CONFLICT DO NOTHING has no conflict target on purpose: it must absorb
// BOTH a replayed idempotency_key (device retried after a lost response) and a
// claim already held in idx_unique_valid_claim (another device won). With a
// target on only one of them, the other raises 23505 and aborts the
// transaction.
const insertScanSQL = `
	INSERT INTO scans (idempotency_key, participant_id, scenario_id,
	                   scanned_by_admin_id, scanned_at_device, status, enforce_unique)
	SELECT $1::varchar, $2::uuid, $3::uuid, $4::uuid, $5::timestamptz,
	       'VALID', NOT s.allow_duplicates
	FROM scan_scenarios s
	WHERE s.id = $3::uuid
	ON CONFLICT DO NOTHING
	RETURNING id`

// insertRejectedSQL keeps an audit trail of losing duplicate attempts. The
// key suffix keeps it from colliding with the original key, and DO NOTHING
// makes repeated replays of the same rejected scan harmless.
const insertRejectedSQL = `
	INSERT INTO scans (idempotency_key, participant_id, scenario_id,
	                   scanned_by_admin_id, scanned_at_device, status, enforce_unique)
	VALUES ($1::varchar || '#rejected', $2, $3, $4, $5, 'DUPLICATE_REJECTED', false)
	ON CONFLICT DO NOTHING`

// SyncBatch writes a batch of offline scans in a single transaction.
//
// Every row runs inside its own SAVEPOINT. In PostgreSQL any error inside a
// transaction (a foreign-key violation, a bad UUID) poisons the whole
// transaction, so without savepoints one bad row would make every later row
// fail with "current transaction is aborted". Rolling back to the savepoint
// discards just that row and lets the batch continue.
//
// Concurrency: if two devices sync a claim for the same participant and
// scenario at once, the second INSERT blocks on idx_unique_valid_claim until
// the first transaction commits, then resolves to DO NOTHING. The first
// committed scan wins, regardless of device clock.
func (r *ScanRepo) SyncBatch(ctx context.Context, adminID string, scans []domain.SyncScanPayload) ([]domain.SyncResult, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin sync tx: %w", err)
	}
	defer tx.Rollback() // no-op after a successful Commit

	results := make([]domain.SyncResult, 0, len(scans))

	for _, s := range scans {
		res, err := r.syncOne(ctx, tx, adminID, s)
		if err != nil {
			// Savepoint machinery itself failed: the connection is unusable,
			// so abort the batch. The client keeps every scan and retries.
			return nil, err
		}
		results = append(results, res)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit sync tx: %w", err)
	}
	return results, nil
}

func (r *ScanRepo) syncOne(ctx context.Context, tx *sql.Tx, adminID string, s domain.SyncScanPayload) (domain.SyncResult, error) {
	res := domain.SyncResult{IdempotencyKey: s.IdempotencyKey}

	if _, err := tx.ExecContext(ctx, "SAVEPOINT scan_row"); err != nil {
		return res, fmt.Errorf("savepoint: %w", err)
	}

	var insertedID string
	err := tx.QueryRowContext(ctx, insertScanSQL,
		s.IdempotencyKey, s.ParticipantID, s.ScenarioID, adminID, s.ScannedAtDevice,
	).Scan(&insertedID)

	switch {
	case err == nil:
		res.Status, res.Message = domain.SyncSuccess, "Scan recorded"

	case errors.Is(err, sql.ErrNoRows):
		// Nothing inserted: a conflict. Find out which kind.
		if err := r.classifyConflict(ctx, tx, adminID, s, &res); err != nil {
			if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT scan_row"); rbErr != nil {
				return res, fmt.Errorf("rollback to savepoint: %w", rbErr)
			}
			res.Status, res.Message = domain.SyncFailed, "Could not classify conflict; will retry"
			return res, nil
		}

	default:
		if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT scan_row"); rbErr != nil {
			return res, fmt.Errorf("rollback to savepoint: %w", rbErr)
		}
		switch pgCode(err) {
		case pgForeignKeyViolation:
			res.Status, res.Message = domain.SyncInvalid, "Participant, scenario or admin does not exist"
		case pgInvalidTextRepr:
			res.Status, res.Message = domain.SyncInvalid, "Malformed identifier"
		default:
			res.Status, res.Message = domain.SyncFailed, "Database error; will retry"
		}
		return res, nil
	}

	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT scan_row"); err != nil {
		return res, fmt.Errorf("release savepoint: %w", err)
	}
	return res, nil
}

// classifyConflict distinguishes an idempotent replay from a lost race.
func (r *ScanRepo) classifyConflict(ctx context.Context, tx *sql.Tx, adminID string, s domain.SyncScanPayload, res *domain.SyncResult) error {
	// 1. Same idempotency key already stored: the device is retrying a scan the
	//    server accepted earlier (for example the HTTP response was lost).
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM scans WHERE idempotency_key = $1)`, s.IdempotencyKey,
	).Scan(&exists); err != nil {
		return err
	}
	if exists {
		res.Status, res.Message = domain.SyncAlreadySynced, "Scan was already recorded"
		return nil
	}

	// 2. Someone else holds the claim. Report who, and log the attempt.
	var (
		winnerName string
		winnerAt   time.Time
	)
	err := tx.QueryRowContext(ctx, `
		SELECT a.full_name, sc.scanned_at_device
		FROM scans sc
		JOIN admins a ON a.id = sc.scanned_by_admin_id
		WHERE sc.participant_id = $1 AND sc.scenario_id = $2
		  AND sc.status = 'VALID' AND sc.enforce_unique
		LIMIT 1`, s.ParticipantID, s.ScenarioID,
	).Scan(&winnerName, &winnerAt)
	if err != nil {
		// sql.ErrNoRows here means the conflicting row vanished between the two
		// statements (e.g. deleted by an admin); report it so the client retries.
		return err
	}

	if _, err := tx.ExecContext(ctx, insertRejectedSQL,
		s.IdempotencyKey, s.ParticipantID, s.ScenarioID, adminID, s.ScannedAtDevice,
	); err != nil {
		return err
	}

	res.Status = domain.SyncDuplicateRejected
	res.Message = "Already claimed by another scan"
	res.ClaimedBy = winnerName
	res.ClaimedAt = &winnerAt
	return nil
}

// ListClaims returns VALID single-claim scans recorded after `since`.
//
// synced_at is the inserting transaction's start time, and transactions can
// commit out of that order, so callers must re-request with an overlap window
// (the scanner subtracts a few minutes from its watermark). Claims are
// idempotent on the client, so overlap is harmless.
func (r *ScanRepo) ListClaims(ctx context.Context, since time.Time) ([]domain.Claim, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT participant_id, scenario_id, synced_at
		FROM scans
		WHERE status = 'VALID' AND enforce_unique AND synced_at > $1
		ORDER BY synced_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	claims := make([]domain.Claim, 0, 256)
	for rows.Next() {
		var c domain.Claim
		if err := rows.Scan(&c.ParticipantID, &c.ScenarioID, &c.SyncedAt); err != nil {
			return nil, err
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}
