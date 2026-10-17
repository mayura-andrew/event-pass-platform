package postgres

// Integration tests for the sync path against a real PostgreSQL with the
// migrations applied. Skipped unless NSAC_TEST_DATABASE_URL is set:
//
//	NSAC_TEST_DATABASE_URL='postgres://nsac:nsac-local-password@127.0.0.1:5433/nsac_loadtest?sslmode=disable' \
//	  go test ./internal/repository/postgres/ -run Sync -v
//
// Each test creates its own admins, delegates and scenarios with a random
// suffix and deletes them afterwards, so it can share a database.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	mrand "math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

type fixture struct {
	db           *sql.DB
	tag          string
	admins       []string // ids
	adminNames   []string
	participants []string
	single       string // single-claim scenario
	repeat       string // repeatable scenario
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("NSAC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NSAC_TEST_DATABASE_URL not set")
	}
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randTag() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newFixture(t *testing.T, admins, participants int) *fixture {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	f := &fixture{db: db, tag: randTag()}

	for i := 0; i < admins; i++ {
		var id string
		name := fmt.Sprintf("Phone %d %s", i, f.tag)
		if err := db.QueryRowContext(ctx, `INSERT INTO admins (email, password_hash, full_name, role)
			VALUES ($1, 'x', $2, 'scanner') RETURNING id`,
			fmt.Sprintf("it-%s-%d@test.local", f.tag, i), name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.admins = append(f.admins, id)
		f.adminNames = append(f.adminNames, name)
	}
	for i := 0; i < participants; i++ {
		var id string
		if err := db.QueryRowContext(ctx, `INSERT INTO participants (membership_id, email, full_name, is_confirmed, qr_signature)
			VALUES ($1, $2, $3, true, 'sig') RETURNING id`,
			fmt.Sprintf("IT-%s-%d", f.tag, i), fmt.Sprintf("it-%s-%d@delegate.local", f.tag, i), fmt.Sprintf("Delegate %d", i)).Scan(&id); err != nil {
			t.Fatal(err)
		}
		f.participants = append(f.participants, id)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO scan_scenarios (name, code, allow_duplicates) VALUES ($1, $2, false) RETURNING id`,
		"Lunch "+f.tag, "IT_LUNCH_"+f.tag).Scan(&f.single); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO scan_scenarios (name, code, allow_duplicates) VALUES ($1, $2, true) RETURNING id`,
		"Re-entry "+f.tag, "IT_REENTRY_"+f.tag).Scan(&f.repeat); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		ctx := context.Background()
		db.ExecContext(ctx, `DELETE FROM scans WHERE scenario_id IN ($1, $2)`, f.single, f.repeat)
		db.ExecContext(ctx, `DELETE FROM scan_scenarios WHERE id IN ($1, $2)`, f.single, f.repeat)
		db.ExecContext(ctx, `DELETE FROM participants WHERE membership_id LIKE $1`, "IT-"+f.tag+"-%")
		db.ExecContext(ctx, `DELETE FROM admins WHERE email LIKE $1`, "it-"+f.tag+"-%")
	})
	return f
}

func singleScan(admin, participant, scenario string) domain.SyncScanPayload {
	return domain.SyncScanPayload{
		IdempotencyKey:  admin + "_" + participant + "_" + scenario,
		ParticipantID:   participant,
		ScenarioID:      scenario,
		ScannedAtDevice: time.Now().UTC().Truncate(time.Second),
	}
}

func repeatScan(admin, participant, scenario string) domain.SyncScanPayload {
	s := singleScan(admin, participant, scenario)
	s.IdempotencyKey += "_" + randTag() + randTag()
	return s
}

func statuses(rs []domain.SyncResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Status
	}
	return out
}

func TestSyncBatchSemantics(t *testing.T) {
	f := newFixture(t, 2, 4)
	repo := NewScanRepo(f.db)
	ctx := context.Background()
	a, b := f.admins[0], f.admins[1]
	p0, p1, p2 := f.participants[0], f.participants[1], f.participants[2]
	unknown := "00000000-0000-4000-8000-000000000001"

	batch := []domain.SyncScanPayload{
		singleScan(a, p0, f.single),      // new claim
		repeatScan(a, p0, f.repeat),      // repeatable
		repeatScan(a, p0, f.repeat),      // repeatable again: its own row
		singleScan(a, unknown, f.single), // participant doesn't exist
		singleScan(a, p1, f.single),      // new claim
	}
	res, err := repo.SyncBatch(ctx, a, batch)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{domain.SyncSuccess, domain.SyncSuccess, domain.SyncSuccess, domain.SyncInvalid, domain.SyncSuccess}
	if fmt.Sprint(statuses(res)) != fmt.Sprint(want) {
		t.Fatalf("first batch: got %v want %v", statuses(res), want)
	}
	for i, r := range res {
		if r.IdempotencyKey != batch[i].IdempotencyKey {
			t.Fatalf("result %d is for %q, want input order", i, r.IdempotencyKey)
		}
	}

	// Replay (lost response): same keys come back as ALREADY_SYNCED.
	res, err = repo.SyncBatch(ctx, a, batch[:3])
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(statuses(res)) != fmt.Sprint([]string{domain.SyncAlreadySynced, domain.SyncAlreadySynced, domain.SyncAlreadySynced}) {
		t.Fatalf("replay: got %v", statuses(res))
	}

	// Another phone claims p0 later: rejected, told who won.
	res, err = repo.SyncBatch(ctx, b, []domain.SyncScanPayload{singleScan(b, p0, f.single), singleScan(b, p2, f.single)})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != domain.SyncDuplicateRejected || res[0].ClaimedBy != f.adminNames[0] || res[0].ClaimedAt == nil {
		t.Fatalf("lost race: got %+v", res[0])
	}
	if res[1].Status != domain.SyncSuccess {
		t.Fatalf("p2: got %+v", res[1])
	}

	// The losing attempt is kept for the audit log, once, even when replayed.
	if _, err := repo.SyncBatch(ctx, b, []domain.SyncScanPayload{singleScan(b, p0, f.single)}); err != nil {
		t.Fatal(err)
	}
	var rejected int
	f.db.QueryRow(`SELECT count(*) FROM scans WHERE scenario_id = $1 AND status = 'DUPLICATE_REJECTED'`, f.single).Scan(&rejected)
	if rejected != 1 {
		t.Fatalf("rejected audit rows = %d, want 1", rejected)
	}
}

// A scenario switched from repeatable to single-claim while a phone was offline
// leaves two keys for the same claim in one batch: the earlier scan wins.
func TestSyncBatchSameClaimTwiceInBatch(t *testing.T) {
	f := newFixture(t, 1, 1)
	repo := NewScanRepo(f.db)
	a, p := f.admins[0], f.participants[0]
	first, second := repeatScan(a, p, f.single), repeatScan(a, p, f.single)
	res, err := repo.SyncBatch(context.Background(), a, []domain.SyncScanPayload{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != domain.SyncSuccess || res[1].Status != domain.SyncDuplicateRejected {
		t.Fatalf("got %v, want [SUCCESS DUPLICATE_REJECTED]", statuses(res))
	}
}

// Many phones upload overlapping claims at once, each in its own scan order.
// Exactly one upload may win each claim, nobody may get an error, and no claim
// may end up with two VALID rows.
func TestSyncBatchConcurrentFirstScanWins(t *testing.T) {
	const phones, delegates, perPhone = 12, 60, 40
	f := newFixture(t, phones, delegates)
	repo := NewScanRepo(f.db)

	var (
		mu      sync.Mutex
		winners = map[string]int{}
		wg      sync.WaitGroup
		errs    []error
	)
	for i := 0; i < phones; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			admin := f.admins[i]
			rng := mrand.New(mrand.NewSource(int64(i)))
			perm := rng.Perm(delegates)[:perPhone] // random order per phone
			batch := make([]domain.SyncScanPayload, 0, perPhone)
			for _, k := range perm {
				batch = append(batch, singleScan(admin, f.participants[k], f.single))
			}
			res, err := repo.SyncBatch(context.Background(), admin, batch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			for j, r := range res {
				switch r.Status {
				case domain.SyncSuccess:
					winners[batch[j].ParticipantID]++
				case domain.SyncDuplicateRejected:
				default:
					errs = append(errs, fmt.Errorf("unexpected %s: %s", r.Status, r.Message))
				}
			}
		}(i)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("%d errors, first: %v", len(errs), errs[0])
	}
	for p, n := range winners {
		if n != 1 {
			t.Fatalf("participant %s won by %d uploads", p, n)
		}
	}
	var doubled int
	f.db.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM scans WHERE scenario_id = $1 AND status = 'VALID'
		GROUP BY participant_id HAVING count(*) > 1) x`, f.single).Scan(&doubled)
	if doubled != 0 {
		t.Fatalf("%d claims hold more than one VALID scan", doubled)
	}
}
