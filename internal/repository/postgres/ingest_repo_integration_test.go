package postgres

// Integration tests for delegate ingestion against a real PostgreSQL with
// migrations 001-004 applied. Skipped unless NSAC_TEST_DATABASE_URL is set
// (see scan_repo_integration_test.go). Rows use it-<tag>-* emails and are
// deleted afterwards.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"testing"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

var generatedID = regexp.MustCompile(`^NSAC-[0-9]{6}$`)

func ingestRows(tag string, from, n int) []domain.NewParticipant {
	rows := make([]domain.NewParticipant, n)
	for i := range rows {
		rows[i] = domain.NewParticipant{
			Line:     from + i,
			FullName: fmt.Sprintf("Delegate %d", from+i),
			Email:    fmt.Sprintf("it-%s-%d@test.local", tag, from+i),
		}
	}
	return rows
}

func cleanupIngest(t *testing.T, repo *IngestRepo, tag string) {
	t.Cleanup(func() {
		repo.db.ExecContext(context.Background(), `DELETE FROM participants WHERE email LIKE $1`, "it-"+tag+"-%")
	})
}

func testSign(id, email string) string { return "sig:" + id + ":" + email }

func TestIngestBulkInsert(t *testing.T) {
	repo := NewIngestRepo(openTestDB(t))
	ctx := context.Background()
	tag := randTag()
	cleanupIngest(t, repo, tag)

	rows := ingestRows(tag, 2, 500)
	rows[0].TeamName = "Orion"
	rows[0].DateOfBirth = "1998-04-12"

	created, skipped, err := repo.BulkInsertWithAutoID(ctx, rows, nil, testSign)
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != len(rows) || len(skipped) != 0 {
		t.Fatalf("created %d, skipped %d; want %d, 0", len(created), len(skipped), len(rows))
	}
	prev := 0
	for i, c := range created {
		if !generatedID.MatchString(c.MembershipID) {
			t.Fatalf("membership ID %q", c.MembershipID)
		}
		n, _ := strconv.Atoi(c.MembershipID[5:])
		if n <= prev {
			t.Fatalf("IDs not increasing: %s after %d", c.MembershipID, prev)
		}
		prev = n
		if c.Email != rows[i].Email || c.Line != rows[i].Line {
			t.Fatalf("row %d came back as %+v", i, c)
		}
	}

	var team, dob, sig string
	var confirmed bool
	err = repo.db.QueryRowContext(ctx, `
		SELECT COALESCE(team_name, ''), COALESCE(date_of_birth::text, ''), qr_signature, is_confirmed
		FROM participants WHERE id = $1`, created[0].ID).Scan(&team, &dob, &sig, &confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if team != "Orion" || dob != "1998-04-12" || !confirmed || sig != testSign(created[0].MembershipID, rows[0].Email) {
		t.Fatalf("stored team=%q dob=%q confirmed=%v sig=%q", team, dob, confirmed, sig)
	}
	var nullTeam bool
	repo.db.QueryRowContext(ctx, `SELECT team_name IS NULL FROM participants WHERE id = $1`, created[1].ID).Scan(&nullTeam)
	if !nullTeam {
		t.Fatal("blank team_name should be stored as NULL")
	}

	existing, err := repo.ExistingEmails(ctx, []string{rows[3].Email, "it-" + tag + "-none@test.local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(existing) != 1 || existing[rows[3].Email] != created[3].MembershipID {
		t.Fatalf("ExistingEmails = %v", existing)
	}

	// Same emails again: nothing inserted, all reported back as skipped.
	created2, skipped2, err := repo.BulkInsertWithAutoID(ctx, rows[:10], nil, testSign)
	if err != nil {
		t.Fatal(err)
	}
	if len(created2) != 0 || len(skipped2) != 10 {
		t.Fatalf("re-insert: created %d, skipped %d", len(created2), len(skipped2))
	}
}

// Two organizers uploading overlapping files at once: every email ends up
// inserted exactly once, and every membership ID is unique.
func TestIngestConcurrentUploads(t *testing.T) {
	repo := NewIngestRepo(openTestDB(t))
	ctx := context.Background()
	tag := randTag()
	cleanupIngest(t, repo, tag)

	const uploads = 6
	rows := ingestRows(tag, 0, 300)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created []domain.CreatedParticipant
		skipped int
	)
	for u := 0; u < uploads; u++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := rows
			if u%2 == 1 { // half the uploads list the delegates in reverse
				batch = make([]domain.NewParticipant, len(rows))
				for i, r := range rows {
					batch[len(rows)-1-i] = r
				}
			}
			c, s, err := repo.BulkInsertWithAutoID(ctx, batch, nil, testSign)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			created = append(created, c...)
			skipped += len(s)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(created) != len(rows) || skipped != len(rows)*(uploads-1) {
		t.Fatalf("created %d, skipped %d; want %d, %d", len(created), skipped, len(rows), len(rows)*(uploads-1))
	}
	ids := map[string]bool{}
	for _, c := range created {
		if ids[c.MembershipID] {
			t.Fatalf("membership ID %s handed out twice", c.MembershipID)
		}
		ids[c.MembershipID] = true
	}
}

// A hand-typed NSAC-00xxxx ID that the sequence reaches later must not fail
// the import: the insert retries with fresh numbers.
func TestIngestRetriesMembershipClash(t *testing.T) {
	repo := NewIngestRepo(openTestDB(t))
	ctx := context.Background()
	tag := randTag()
	cleanupIngest(t, repo, tag)

	var next int
	if err := repo.db.QueryRowContext(ctx, `
		SELECT CASE WHEN is_called THEN last_value + 1 ELSE last_value END
		FROM participant_membership_seq`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	// Squat on the next two numbers the sequence will hand out.
	for i := 0; i < 2; i++ {
		if _, err := repo.db.ExecContext(ctx, `
			INSERT INTO participants (membership_id, email, full_name, is_confirmed, qr_signature)
			VALUES ($1, $2, 'Squatter', true, 'x')`,
			fmt.Sprintf("NSAC-%06d", next+i), fmt.Sprintf("it-%s-squat%d@test.local", tag, i)); err != nil {
			t.Fatal(err)
		}
	}

	created, _, err := repo.BulkInsertWithAutoID(ctx, ingestRows(tag, 0, 1), nil, testSign)
	if err != nil {
		t.Fatalf("insert should retry past the taken ID: %v", err)
	}
	if len(created) != 1 || created[0].MembershipID == fmt.Sprintf("NSAC-%06d", next) {
		t.Fatalf("created %+v", created)
	}
}

func TestIngestBulkUpdateByEmail(t *testing.T) {
	repo := NewIngestRepo(openTestDB(t))
	ctx := context.Background()
	tag := randTag()
	cleanupIngest(t, repo, tag)
	shirt := "it_" + tag + "_shirt"
	t.Cleanup(func() {
		repo.db.ExecContext(context.Background(), `DELETE FROM participant_field_definitions WHERE field_key = $1`, shirt)
	})

	rows := ingestRows(tag, 0, 3)
	rows[0].TeamName = "Orion"
	rows[0].CustomAttributes = map[string]any{"food": "Vegetarian"}
	created, _, err := repo.BulkInsertWithAutoID(ctx, rows, nil, testSign)
	if err != nil {
		t.Fatal(err)
	}

	update := []domain.NewParticipant{
		{Line: 2, Email: rows[0].Email, TeamName: "Helio", CustomAttributes: map[string]any{shirt: "M"}}, // name blank: kept
		{Line: 3, Email: rows[1].Email, FullName: rows[1].FullName},                                      // same name: unchanged
		{Line: 4, Email: "it-" + tag + "-ghost@test.local", FullName: "Ghost"},                           // not registered
	}
	newFields := []domain.NewField{{Key: shirt, Label: "Shirt", DataType: "text", Source: "csv", SortOrder: 100}}
	updated, unchanged, missing, err := repo.BulkUpdateByEmail(ctx, update, newFields)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || len(unchanged) != 1 || len(missing) != 1 {
		t.Fatalf("updated %+v, unchanged %d, missing %d", updated, len(unchanged), len(missing))
	}
	if u := updated[0]; u.MembershipID != created[0].MembershipID || fmt.Sprint(u.Changed) != "[team_name "+shirt+"]" {
		t.Errorf("updated = %+v", u)
	}

	var name, team, sig, memberID, custom string
	if err := repo.db.QueryRowContext(ctx, `
		SELECT full_name, team_name, qr_signature, membership_id, custom_attributes::text
		FROM participants WHERE id = $1`, created[0].ID).Scan(&name, &team, &sig, &memberID, &custom); err != nil {
		t.Fatal(err)
	}
	if name != rows[0].FullName || team != "Helio" {
		t.Errorf("name %q team %q; the blank name must keep the stored one", name, team)
	}
	if memberID != created[0].MembershipID || sig != created[0].QRSignature {
		t.Error("membership ID or QR signature changed; passes already sent would stop working")
	}
	if custom != `{"food": "Vegetarian", "`+shirt+`": "M"}` {
		t.Errorf("custom attributes = %s; want the old key kept and the new one added", custom)
	}
	var registered bool
	repo.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM participant_field_definitions WHERE field_key = $1)`, shirt).Scan(&registered)
	if !registered {
		t.Error("new field not registered by the update")
	}
}
