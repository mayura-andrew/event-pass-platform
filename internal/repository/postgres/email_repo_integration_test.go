package postgres

// Integration tests for the pass email queue (migration 006). Skipped unless
// NSAC_TEST_DATABASE_URL is set. Unlike the other integration tests these need
// the email_jobs table to themselves (a claim takes the oldest job anywhere,
// bulk enqueue covers every delegate), so they also skip unless the database
// is named like a test database and holds no confirmed delegates of its own.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

func newEmailFixture(t *testing.T, delegates int) (*EmailRepo, []domain.CreatedParticipant) {
	t.Helper()
	db := openTestDB(t)
	ctx := context.Background()
	tag := randTag()
	var name string
	var others int
	if err := db.QueryRowContext(ctx, `
		SELECT current_database(),
		       (SELECT count(*) FROM participants WHERE is_confirmed AND email NOT LIKE 'it-%')`).Scan(&name, &others); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(name, "test") || others > 0 {
		t.Skipf("email queue tests clear email_jobs; database %q (with %d real confirmed delegates) doesn't look disposable", name, others)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM email_jobs`); err != nil {
		t.Fatal(err)
	}
	db.ExecContext(ctx, `UPDATE email_queue_state SET paused = false, pause_reason = NULL`)

	ingest := NewIngestRepo(db)
	created, _, err := ingest.BulkInsertWithAutoID(ctx, ingestRows(tag, 0, delegates), nil, testSign)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.ExecContext(context.Background(), `DELETE FROM participants WHERE email LIKE $1`, "it-"+tag+"-%")
	})
	return NewEmailRepo(db), created
}

func TestEmailEnqueue(t *testing.T) {
	repo, created := newEmailFixture(t, 5)
	ctx := context.Background()

	n, err := repo.EnqueueBulk(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Fatalf("bulk queued %d, want the 5 test delegates", n)
	}
	if again, _ := repo.EnqueueBulk(ctx, ""); again != 0 {
		t.Fatalf("second bulk queued %d; nobody may be queued twice", again)
	}
	if _, err := repo.EnqueueSingle(ctx, created[0].ID, "", ""); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("single while queued: %v, want conflict", err)
	}

	// Unconfirmed delegates can't be queued.
	repo.db.ExecContext(ctx, `UPDATE participants SET is_confirmed = false WHERE id = $1`, created[4].ID)
	repo.db.ExecContext(ctx, `DELETE FROM email_jobs WHERE participant_id = $1`, created[4].ID)
	if _, err := repo.EnqueueSingle(ctx, "", created[4].MembershipID, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("unconfirmed: %v, want validation error", err)
	}
	if _, err := repo.EnqueueSingle(ctx, "", "NSAC-NOPE", ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown: %v, want not found", err)
	}

	// After it's sent, a single enqueue is a resend; bulk leaves them alone.
	job, _ := repo.FetchNextJob(ctx)
	if err := repo.MarkSent(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueSingle(ctx, job.ParticipantID, "", ""); err != nil {
		t.Fatalf("resend after sent: %v", err)
	}
	st, err := repo.Stats(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Sent != 1 || st.SentLast24h != 1 || st.LastSentAt == nil || st.NotYetEmailed != 0 {
		t.Errorf("stats %+v", st)
	}
}

// Workers claiming at the same time never get the same job, and every job
// is claimed once.
func TestEmailFetchSkipLocked(t *testing.T) {
	repo, _ := newEmailFixture(t, 40)
	ctx := context.Background()
	total, err := repo.EnqueueBulk(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := repo.FetchNextJob(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("claimed %d distinct jobs, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func TestEmailRetryLifecycle(t *testing.T) {
	repo, created := newEmailFixture(t, 1)
	ctx := context.Background()
	if _, err := repo.EnqueueSingle(ctx, created[0].ID, "", ""); err != nil {
		t.Fatal(err)
	}

	claim := func() *domain.EmailJob {
		t.Helper()
		// Make it due now, whatever backoff was set.
		repo.db.ExecContext(ctx, `UPDATE email_jobs SET scheduled_at = NOW() WHERE status = 'pending'`)
		j, err := repo.FetchNextJob(ctx)
		if err != nil || j == nil {
			t.Fatalf("claim: %v %v", j, err)
		}
		return j
	}

	j := claim()
	if j.Attempts != 1 || j.MembershipID != created[0].MembershipID || j.QRSignature == "" || !j.IsConfirmed {
		t.Fatalf("claimed %+v", j)
	}
	status, _ := repo.MarkFailed(ctx, j.ID, "451 try later", false, time.Minute)
	if status != "pending" {
		t.Fatalf("after first failure: %s", status)
	}
	if next, _ := repo.FetchNextJob(ctx); next != nil {
		t.Fatal("a job in backoff was claimed early")
	}

	// A sender-side problem doesn't use up an attempt.
	j = claim()
	repo.Release(ctx, j.ID, "535 auth", 0)
	j = claim()
	if j.Attempts != 2 {
		t.Fatalf("attempts after release = %d, want 2", j.Attempts)
	}
	repo.MarkFailed(ctx, j.ID, "451 again", false, 0)
	j = claim()
	status, _ = repo.MarkFailed(ctx, j.ID, "451 third time", false, 0)
	if status != "failed" {
		t.Fatalf("after the third attempt: %s, want failed", status)
	}

	st, _ := repo.Stats(ctx, time.Now().Add(-time.Hour))
	if st.Failed != 1 || len(st.RecentFailures) != 1 || st.RecentFailures[0].LastError != "451 third time" {
		t.Fatalf("stats %+v", st)
	}
	// Bulk includes delegates whose earlier email failed.
	if n, _ := repo.EnqueueBulk(ctx, ""); n != 1 {
		t.Fatalf("bulk after failure queued %d, want 1", n)
	}
	st, _ = repo.Stats(ctx, time.Now().Add(-time.Hour))
	if len(st.RecentFailures) != 0 {
		t.Fatal("a failure that has been queued again should leave the failure list")
	}
}

func TestEmailPermanentAndReclaim(t *testing.T) {
	repo, _ := newEmailFixture(t, 2)
	ctx := context.Background()
	repo.EnqueueBulk(ctx, "")

	j, _ := repo.FetchNextJob(ctx)
	if status, _ := repo.MarkFailed(ctx, j.ID, "550 no such user", true, time.Minute); status != "failed" {
		t.Fatalf("permanent failure left status %s", status)
	}

	// A claim abandoned mid-send (crash) goes back to pending.
	j, _ = repo.FetchNextJob(ctx)
	repo.db.ExecContext(ctx, `UPDATE email_jobs SET locked_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, j.ID)
	n, err := repo.ReclaimStale(ctx, domain.EmailStaleClaimAfter)
	if err != nil || n != 1 {
		t.Fatalf("reclaimed %d, %v", n, err)
	}
	again, _ := repo.FetchNextJob(ctx)
	if again == nil || again.ID != j.ID || again.Attempts != 2 {
		t.Fatalf("reclaimed job came back as %+v", again)
	}
}

func TestEmailPauseState(t *testing.T) {
	repo, _ := newEmailFixture(t, 1)
	ctx := context.Background()
	st, err := repo.SetPaused(ctx, true, "Gmail's daily sending limit was reached.", "")
	if err != nil || !st.Paused || st.PauseReason == "" {
		t.Fatalf("%+v %v", st, err)
	}
	st, _ = repo.State(ctx)
	if !st.Paused {
		t.Fatal("pause not stored")
	}
	st, _ = repo.SetPaused(ctx, false, "", "")
	if st.Paused || st.PauseReason != "" {
		t.Fatalf("resume kept %+v", st)
	}
}
