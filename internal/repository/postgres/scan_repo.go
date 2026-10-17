package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type ScanRepo struct{ db *sql.DB }

func NewScanRepo(db *sql.DB) *ScanRepo { return &ScanRepo{db: db} }

// SyncBatch writes a batch of offline scans in one transaction using a fixed
// number of statements, whatever the batch size:
//
//  1. one INSERT ... SELECT FROM unnest(...) for every scan,
//  2. one SELECT that explains every scan the INSERT skipped,
//  3. one INSERT for the audit rows of the scans that lost a race.
//
// Why not a statement (and savepoint) per scan: that was 3 round trips per
// scan, so a 200-scan upload held its transaction open for ~6 s at a 10 ms
// database round trip. Meanwhile it held uncommitted claims other phones were
// trying to insert. With 60 phones uploading overlapping claims at once, the
// transactions blocked each other and deadlocked hundreds of times.
//
// Lock order: rows are inserted sorted by (participant_id, scenario_id). Two
// uploads that both claim A and B therefore always meet at A first, so one
// waits for the other instead of each holding what the other needs. No cycle,
// no deadlock. The sort ties on input position, so when one batch holds two
// scans for the same claim, the earlier scan wins.
//
// First scan wins: if two uploads claim the same delegate at once, the second
// INSERT waits on idx_unique_valid_claim until the first commits, then skips
// the row (ON CONFLICT DO NOTHING) and step 2 reports DUPLICATE_REJECTED. This
// runs at READ COMMITTED, so there are no serialization failures (40001).
//
// Results are returned in input order.
func (r *ScanRepo) SyncBatch(ctx context.Context, adminID string, scans []domain.SyncScanPayload) ([]domain.SyncResult, error) {
	results := make([]domain.SyncResult, len(scans))
	if len(scans) == 0 {
		return results, nil
	}

	order := make([]int, len(scans))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := scans[order[a]], scans[order[b]]
		if x.ParticipantID != y.ParticipantID {
			return x.ParticipantID < y.ParticipantID
		}
		return x.ScenarioID < y.ScenarioID // stable: equal keys keep input order
	})
	keys := make([]string, len(scans))
	pids := make([]string, len(scans))
	sids := make([]string, len(scans))
	ats := make([]time.Time, len(scans))
	byKey := make(map[string]int, len(scans)) // idempotency key -> input index
	for pos, i := range order {
		s := scans[i]
		keys[pos], pids[pos], sids[pos], ats[pos] = s.IdempotencyKey, s.ParticipantID, s.ScenarioID, s.ScannedAtDevice
		byKey[s.IdempotencyKey] = i
		results[i].IdempotencyKey = s.IdempotencyKey
	}

	err := r.syncTx(ctx, adminID, keys, pids, sids, ats, byKey, results)
	if err != nil && isTransient(err) {
		// Lock timeout, a deadlock involving another code path, or a dropped
		// connection: nothing was committed, so the phone can safely retry
		// every scan.
		for i := range results {
			results[i].Status, results[i].Message = domain.SyncFailed, "Database busy; will retry"
		}
		return results, nil
	}
	if err != nil {
		return nil, err
	}
	return results, nil
}

func (r *ScanRepo) syncTx(ctx context.Context, adminID string, keys, pids, sids []string, ats []time.Time,
	byKey map[string]int, results []domain.SyncResult) error {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin sync tx: %w", err)
	}
	defer tx.Rollback() // no-op after Commit

	// 1. Insert everything. Joining participants and scenarios drops scans for
	//    unknown ones instead of failing the statement on a foreign key.
	//    ON CONFLICT DO NOTHING has no target on purpose: it must absorb a
	//    replayed idempotency key AND an existing claim in
	//    idx_unique_valid_claim. With a target on one, the other raises 23505
	//    and aborts the whole batch.
	rows, err := tx.QueryContext(ctx, `
		INSERT INTO scans (idempotency_key, participant_id, scenario_id,
		                   scanned_by_admin_id, scanned_at_device, status, enforce_unique)
		SELECT t.key, t.pid, t.sid, $5::uuid, t.at, 'VALID', NOT s.allow_duplicates
		FROM unnest($1::text[], $2::uuid[], $3::uuid[], $4::timestamptz[])
		     WITH ORDINALITY AS t(key, pid, sid, at, ord)
		JOIN scan_scenarios s ON s.id = t.sid
		JOIN participants p   ON p.id = t.pid
		ORDER BY t.ord
		ON CONFLICT DO NOTHING
		RETURNING idempotency_key`,
		keys, pids, sids, ats, adminID)
	if err != nil {
		return fmt.Errorf("bulk insert scans: %w", err)
	}
	inserted := make(map[string]bool, len(keys))
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		inserted[k] = true
		results[byKey[k]].Status, results[byKey[k]].Message = domain.SyncSuccess, "Scan recorded"
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("bulk insert scans: %w", err)
	}

	// 2. Explain the rest in one query: replay, unknown delegate, or lost race.
	var rk, rp, rs []string
	var ra []time.Time
	for pos, k := range keys {
		if !inserted[k] {
			rk, rp, rs, ra = append(rk, k), append(rp, pids[pos]), append(rs, sids[pos]), append(ra, ats[pos])
		}
	}
	if len(rk) > 0 {
		rows, err := tx.QueryContext(ctx, `
			SELECT t.key,
			       EXISTS (SELECT 1 FROM scans x WHERE x.idempotency_key = t.key),
			       EXISTS (SELECT 1 FROM participants p WHERE p.id = t.pid),
			       w.full_name, w.scanned_at_device
			FROM unnest($1::text[], $2::uuid[], $3::uuid[]) AS t(key, pid, sid)
			LEFT JOIN LATERAL (
				SELECT a.full_name, sc.scanned_at_device
				FROM scans sc
				JOIN admins a ON a.id = sc.scanned_by_admin_id
				WHERE sc.participant_id = t.pid AND sc.scenario_id = t.sid
				  AND sc.status = 'VALID' AND sc.enforce_unique
				LIMIT 1
			) w ON true`,
			rk, rp, rs)
		if err != nil {
			return fmt.Errorf("classify skipped scans: %w", err)
		}
		lost := make(map[string]bool, len(rk))
		for rows.Next() {
			var (
				k             string
				replay, known bool
				winner        sql.NullString
				winnerAt      sql.NullTime
			)
			if err := rows.Scan(&k, &replay, &known, &winner, &winnerAt); err != nil {
				rows.Close()
				return err
			}
			res := &results[byKey[k]]
			switch {
			case replay:
				// The device is retrying a scan the server accepted earlier
				// (for example the HTTP response was lost).
				res.Status, res.Message = domain.SyncAlreadySynced, "Scan was already recorded"
			case !known:
				res.Status, res.Message = domain.SyncInvalid, "Participant does not exist"
			case winner.Valid:
				t := winnerAt.Time
				res.Status, res.Message = domain.SyncDuplicateRejected, "Already claimed by another scan"
				res.ClaimedBy, res.ClaimedAt = winner.String, &t
				lost[k] = true
			default:
				// The conflicting row vanished between statements (deleted by
				// an admin): let the phone retry.
				res.Status, res.Message = domain.SyncFailed, "Could not classify conflict; will retry"
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("classify skipped scans: %w", err)
		}

		// 3. Audit trail of losing attempts. The suffix keeps them clear of the
		//    original key, and DO NOTHING makes replays of a rejected scan
		//    harmless. enforce_unique=false keeps them out of the claim index.
		if len(lost) > 0 {
			var lk, lp, ls []string
			var la []time.Time
			for i, k := range rk {
				if lost[k] {
					lk, lp, ls, la = append(lk, k+"#rejected"), append(lp, rp[i]), append(ls, rs[i]), append(la, ra[i])
				}
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO scans (idempotency_key, participant_id, scenario_id,
				                   scanned_by_admin_id, scanned_at_device, status, enforce_unique)
				SELECT t.key, t.pid, t.sid, $5::uuid, t.at, 'DUPLICATE_REJECTED', false
				FROM unnest($1::text[], $2::uuid[], $3::uuid[], $4::timestamptz[]) AS t(key, pid, sid, at)
				ON CONFLICT DO NOTHING`,
				lk, lp, ls, la, adminID); err != nil {
				return fmt.Errorf("insert rejected scans: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sync tx: %w", err)
	}
	return nil
}

// isTransient reports errors after which retrying the same batch is safe and
// likely to succeed.
func isTransient(err error) bool {
	switch pgCode(err) {
	case pgDeadlockDetected, pgSerializationFailure, pgLockNotAvailable, pgQueryCanceled, pgTooManyConnections:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn)
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
