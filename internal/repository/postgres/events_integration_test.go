package postgres

// Integration tests for migration 008: events, shares, stars, opens and the
// audit log behind Row Level Security, and the composite foreign keys
// (wall 3). Same setup as tenancy_integration_test.go.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/authz"
	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

func (f *tenancyFixture) event(t *testing.T, sc authz.OrgScope, name string) authz.EventScope {
	t.Helper()
	e, err := NewEventRepo(f.db).CreateEvent(context.Background(), sc, &domain.Event{
		Slug: "e", Name: name, Type: "meetup", TimeZone: "UTC", Visibility: "private", Language: "en", Tags: []string{"x"},
	}, "", domain.Audit{Action: "event.create", TargetType: "event"})
	if err != nil {
		t.Fatal(err)
	}
	es, ok := authz.ForEvent(sc, e.ID, e.OwnerID, "", e.Settings)
	if !ok {
		t.Fatal("owner has no access to their event")
	}
	return es
}

func TestEventTablesRLS(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	events := NewEventRepo(f.db)
	a, b := f.account(t, "a"), f.account(t, "b")
	scA, orgA := f.org(t, a, "a")
	scB, orgB := f.org(t, b, "b")
	evA, evB := f.event(t, scA, "A event"), f.event(t, scB, "B event")
	for _, es := range []authz.EventScope{evA, evB} {
		if err := events.SetStarred(ctx, es, true); err != nil {
			t.Fatal(err)
		}
		if err := events.TouchOpened(ctx, es); err != nil {
			t.Fatal(err)
		}
	}

	tables := []string{"events", "event_shares", "event_stars", "event_opens", "audit_log"}
	err := withTenant(ctx, f.db, "", "", func(tx *sql.Tx) error {
		for _, table := range tables {
			if n := count(t, tx, "SELECT count(*) FROM "+table); n != 0 {
				t.Errorf("%s: %d rows visible with no tenant set", table, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM events WHERE id = $1`, evB.EventID()); n != 0 {
			t.Error("sees org B's event by ID")
		}
		if n := count(t, tx, `SELECT count(*) FROM audit_log WHERE org_id = $1`, orgB.ID); n != 0 {
			t.Error("sees org B's audit log")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wall 3: a share in org A can't point at org B's event, and writing
	// org B's org_id is refused by RLS.
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO event_shares (id, org_id, event_id, account_id, role) VALUES ($1, $2, $3, $4, 'editor')`,
			newID(), orgA.ID, evB.EventID(), a.ID)
		return err
	})
	if pgCode(err) != pgForeignKeyViolation {
		t.Errorf("share pointing at another org's event: %v, want a foreign key violation", err)
	}
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO event_shares (id, org_id, event_id, account_id, role) VALUES ($1, $2, $3, $4, 'editor')`,
			newID(), orgB.ID, evB.EventID(), a.ID)
		return err
	})
	if pgCode(err) != "42501" {
		t.Errorf("share written into org B: %v, want an RLS violation", err)
	}

	// The audit log is append-only and written only as yourself.
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE audit_log SET action = 'x'`)
		return err
	})
	if pgCode(err) != "42501" {
		t.Errorf("update audit log: %v, want permission denied", err)
	}
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		return insertAudit(ctx, tx, orgA.ID, b.ID, domain.Audit{Action: "forged", TargetType: "event"})
	})
	if pgCode(err) != "42501" {
		t.Errorf("audit entry as someone else: %v, want an RLS violation", err)
	}
}

func TestStarsAndOpensArePersonal(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	events := NewEventRepo(f.db)
	a, b := f.account(t, "a"), f.account(t, "b")
	scA, orgA := f.org(t, a, "a")
	es := f.event(t, scA, "Shared event")
	if _, err := events.PutShare(ctx, es, b.ID, domain.EventRoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := events.SetStarred(ctx, es, true); err != nil {
		t.Fatal(err)
	}
	err := withTenant(ctx, f.db, orgA.ID, b.ID, func(tx *sql.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM event_stars`); n != 0 {
			t.Error("b sees a's star")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGuestMembership(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	events := NewEventRepo(f.db)
	a, g := f.account(t, "a"), f.account(t, "guest")
	scA, orgA := f.org(t, a, "a")
	es := f.event(t, scA, "Guest event")
	gs := authz.NewAccountScope(g.ID)

	if _, err := f.orgs.Membership(ctx, gs, orgA.Slug); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("before the share: %v, want ErrNotFound", err)
	}
	share, err := events.PutShare(ctx, es, g.ID, domain.EventRoleCheckin)
	if err != nil || !share.Guest {
		t.Fatalf("share with a non-member: %+v, %v (want Guest)", share, err)
	}
	m, err := f.orgs.Membership(ctx, gs, orgA.Slug)
	if err != nil || m.Role != domain.OrgRoleGuest {
		t.Fatalf("guest membership: %+v, %v", m, err)
	}
	orgs, err := f.orgs.ListMyOrgs(ctx, gs)
	if err != nil || len(orgs) != 1 || orgs[0].Role != domain.OrgRoleGuest {
		t.Fatalf("guest's orgs: %+v, %v", orgs, err)
	}

	// The guest sees only events shared with them; the owner's name shows
	// although the guest can't list the org's members.
	gsc := authz.ForMember(gs, orgA.ID, domain.OrgRoleGuest)
	f.event(t, scA, "Not shared")
	rows, err := events.ListEvents(ctx, gsc, domain.EventListQuery{View: domain.ViewShared}, false)
	if err != nil || len(rows) != 1 || rows[0].Name != "Guest event" || rows[0].OwnerEmail != a.Email {
		t.Fatalf("guest's shared events: %+v, %v", rows, err)
	}

	// Removing the last share ends guest access to the org.
	if err := events.RemoveShare(ctx, es, share.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.orgs.Membership(ctx, gs, orgA.Slug); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("after removing the share: %v, want ErrNotFound", err)
	}
}

func TestDuplicateKeepsTeamAndTrashRetention(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	events := NewEventRepo(f.db)
	a, b, c := f.account(t, "a"), f.account(t, "b"), f.account(t, "c")
	scA, _ := f.org(t, a, "a")
	es := f.event(t, scA, "Space Apps 2025")
	events.PutShare(ctx, es, b.ID, domain.EventRoleEditor)
	events.PutShare(ctx, es, c.ID, domain.EventRoleCheckin)
	if err := events.SetLink(ctx, es, []byte("link-"+f.tag), nil); err != nil {
		t.Fatal(err)
	}

	src, _ := events.EventAccess(ctx, scA, es.EventID())
	copyEv := *src.Event
	copyEv.Name, copyEv.Slug = "Space Apps 2026", "e"
	dup, err := events.CreateEvent(ctx, scA, &copyEv, es.EventID(), domain.Audit{Action: "event.duplicate", TargetType: "event"})
	if err != nil {
		t.Fatal(err)
	}
	if dup.Slug != "e-2" {
		t.Errorf("duplicate slug %q, want e-2", dup.Slug)
	}
	des, _ := authz.ForEvent(scA, dup.ID, dup.OwnerID, "", dup.Settings)
	st, err := events.Sharing(ctx, des)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Shares) != 2 || st.Link.Enabled {
		t.Fatalf("duplicate's sharing: %d shares, link %v; want the 2 people and no link", len(st.Shares), st.Link.Enabled)
	}

	// Trash: restorable within 30 days, not after.
	if err := events.SetDeleted(ctx, es, true); err != nil {
		t.Fatal(err)
	}
	f.db.ExecContext(ctx, `UPDATE events SET deleted_at = NOW() - INTERVAL '31 days' WHERE id = $1`, es.EventID())
	if err := events.SetDeleted(ctx, es, false); err != nil {
		t.Fatal(err)
	}
	got, _ := events.EventAccess(ctx, scA, es.EventID())
	if got.Event.DeletedAt == nil || time.Since(*got.Event.DeletedAt) < 30*24*time.Hour {
		t.Fatal("an event 31 days in Trash was restored")
	}
	rows, _ := events.ListEvents(ctx, scA, domain.EventListQuery{View: domain.ViewTrash}, true)
	if len(rows) != 0 {
		t.Errorf("trash lists %d events past retention", len(rows))
	}
}

// Ten people at once for three places: exactly three get them, the rest are
// waitlisted, and every pass ID is distinct.
func TestRegisterCapacityUnderConcurrency(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a := f.account(t, "a")
	sc, _ := f.org(t, a, "a")
	es := f.event(t, sc, "Popular workshop")
	f.db.ExecContext(ctx, `UPDATE events SET capacity = 3, waitlist = true, status = 'published' WHERE id = $1`, es.EventID())
	acc, err := NewEventRepo(f.db).EventAccess(ctx, sc, es.EventID())
	if err != nil {
		t.Fatal(err)
	}
	regs := NewRegistrationRepo(f.db)

	var wg sync.WaitGroup
	results := make([]*domain.EventParticipant, 10)
	errs := make([]error, 10)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = regs.Register(ctx, acc.Event, domain.EventParticipant{
				Email: fmt.Sprintf("it-%s-r%d@test.local", f.tag, i), FullName: "Person", Answers: map[string]any{},
			}, func() string { return fmt.Sprintf("T-%s-%02d", f.tag[:4], i) })
		}(i)
	}
	wg.Wait()
	counts := map[string]int{}
	ids := map[string]bool{}
	for i, p := range results {
		if errs[i] != nil {
			t.Fatalf("register %d: %v", i, errs[i])
		}
		counts[p.Status]++
		ids[p.PassID] = true
	}
	if counts[domain.PartRegistered] != 3 || counts[domain.PartWaitlisted] != 7 || len(ids) != 10 {
		t.Fatalf("got %v and %d distinct pass IDs; want 3 registered, 7 waitlisted, 10 IDs", counts, len(ids))
	}

	// A pass ID collision is retried with a fresh one.
	n := 0
	p, err := regs.Register(ctx, acc.Event, domain.EventParticipant{Email: "it-" + f.tag + "-dup@test.local", FullName: "Dup",
		Answers: map[string]any{}}, func() string {
		n++
		if n == 1 {
			return results[0].PassID // taken
		}
		return "T-" + f.tag[:4] + "-NEW"
	})
	if err != nil || p.PassID != "T-"+f.tag[:4]+"-NEW" {
		t.Fatalf("pass ID collision: %+v, %v", p, err)
	}
}

// A participant's own passes are readable across orgs (portal), nothing else.
func TestParticipantRowsRLS(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a, b, person := f.account(t, "a"), f.account(t, "b"), f.account(t, "person")
	scA, orgA := f.org(t, a, "a")
	scB, _ := f.org(t, b, "b")
	esA, esB := f.event(t, scA, "A"), f.event(t, scB, "B")
	regs, events := NewRegistrationRepo(f.db), NewEventRepo(f.db)
	for i, es := range []authz.EventScope{esA, esB} {
		acc, _ := events.EventAccess(ctx, map[int]authz.OrgScope{0: scA, 1: scB}[i], es.EventID())
		p, err := regs.Register(ctx, acc.Event, domain.EventParticipant{Email: person.Email, FullName: "Person", Answers: map[string]any{}},
			func() string { return fmt.Sprintf("P-%s-%d", f.tag[:4], i) })
		if err != nil {
			t.Fatal(err)
		}
		f.db.ExecContext(ctx, `UPDATE event_participants SET account_id = $1 WHERE id = $2`, person.ID, p.ID)
	}
	err := withTenant(ctx, f.db, "", person.ID, func(tx *sql.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM event_participants`); n != 2 {
			t.Errorf("person sees %d of their passes, want 2", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM events`); n != 2 {
			t.Errorf("person sees %d events, want the 2 they're registered for", n)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM events`); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var left int
	f.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE org_id = $1`, orgA.ID).Scan(&left)
	if left != 1 {
		t.Fatal("a participant's tenant transaction deleted an event")
	}
}

func TestCheckinTablesRLSAndAppendOnly(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a, b := f.account(t, "a"), f.account(t, "b")
	scA, orgA := f.org(t, a, "a")
	scB, _ := f.org(t, b, "b")
	esA, esB := f.event(t, scA, "A"), f.event(t, scB, "B")
	regs, checkins, workshops := NewRegistrationRepo(f.db), NewCheckinRepo(f.db), NewWorkshopRepo(f.db)
	acc, _ := NewEventRepo(f.db).EventAccess(ctx, scA, esA.EventID())
	p, err := regs.Register(ctx, acc.Event, domain.EventParticipant{Email: "it-" + f.tag + "-p@test.local", FullName: "P", Answers: map[string]any{}},
		func() string { return "C-" + f.tag })
	if err != nil {
		t.Fatal(err)
	}
	cps, _ := checkins.Checkpoints(ctx, esA, false)
	if len(cps) != 3 {
		t.Fatalf("new event has %d checkpoints, want the 3 defaults", len(cps))
	}
	if _, err := workshops.Save(ctx, esB, domain.Workshop{Title: "B's", SignupMode: "opt_in"}); err != nil {
		t.Fatal(err)
	}
	rec, _, _, err := checkins.Record(ctx, esA, domain.RecordInput{ParticipantID: p.ID, CheckpointID: cps[1].ID, IdempotencyKey: "k1", Once: true})
	if err != nil {
		t.Fatal(err)
	}

	err = withTenant(ctx, f.db, "", "", func(tx *sql.Tx) error {
		for _, table := range []string{"workshops", "workshop_signups", "checkpoints", "checkins"} {
			if n := count(t, tx, "SELECT count(*) FROM "+table); n != 0 {
				t.Errorf("%s: %d rows visible with no tenant set", table, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"delete a check-in":       `DELETE FROM checkins`,
		"rewrite a check-in time": `UPDATE checkins SET created_at = NOW() - INTERVAL '1 day'`,
	} {
		err := withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, q); return err })
		if pgCode(err) != "42501" {
			t.Errorf("%s: %v, want permission denied", name, err)
		}
	}
	// Recorded as someone else: refused by RLS.
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO checkins (id, org_id, event_id, participant_id, checkpoint_id, idempotency_key, staff_account_id)
			VALUES ($1, $2, $3, $4, $5, 'forged', $6)`, newID(), orgA.ID, esA.EventID(), p.ID, cps[2].ID, b.ID)
		return err
	})
	if pgCode(err) != "42501" {
		t.Errorf("check-in recorded as another account: %v, want an RLS violation", err)
	}
	if err := checkins.Undo(ctx, esA, rec.ID); err != nil {
		t.Fatalf("undo through the tenant role: %v", err)
	}
	// Wall 3: a sign-up can't point at another org's workshop.
	wsB, _ := workshops.List(ctx, esB)
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO workshop_signups (org_id, workshop_id, participant_id) VALUES ($1, $2, $3)`,
			orgA.ID, wsB[0].ID, p.ID)
		return err
	})
	if pgCode(err) != pgForeignKeyViolation {
		t.Errorf("sign-up to another org's workshop: %v, want a foreign key violation", err)
	}
}

// Twenty phones scan the same person at once: one check-in, nineteen
// duplicates pointing at it.
func TestOnceCheckpointUnderConcurrency(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a := f.account(t, "a")
	sc, _ := f.org(t, a, "a")
	es := f.event(t, sc, "Gate")
	acc, _ := NewEventRepo(f.db).EventAccess(ctx, sc, es.EventID())
	p, err := NewRegistrationRepo(f.db).Register(ctx, acc.Event, domain.EventParticipant{Email: "it-" + f.tag + "-x@test.local",
		FullName: "X", Answers: map[string]any{}}, func() string { return "O-" + f.tag })
	if err != nil {
		t.Fatal(err)
	}
	checkins := NewCheckinRepo(f.db)
	cps, _ := checkins.Checkpoints(ctx, es, false)
	var wg sync.WaitGroup
	recs := make([]*domain.Checkin, 20)
	dups := make([]*domain.Checkin, 20)
	errs := make([]error, 20)
	for i := range recs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i], dups[i], _, errs[i] = checkins.Record(ctx, es, domain.RecordInput{ParticipantID: p.ID, CheckpointID: cps[1].ID,
				IdempotencyKey: fmt.Sprintf("phone-%d", i), Once: true})
		}(i)
	}
	wg.Wait()
	var recorded int
	var winner string
	for i := range recs {
		if errs[i] != nil {
			t.Fatalf("record %d: %v", i, errs[i])
		}
		if recs[i] != nil {
			recorded++
			winner = recs[i].ID
		}
	}
	if recorded != 1 {
		t.Fatalf("%d check-ins recorded, want 1", recorded)
	}
	for i := range dups {
		if recs[i] == nil && (dups[i] == nil || dups[i].ID != winner) {
			t.Fatalf("scan %d: duplicate %+v doesn't point at the recorded check-in %s", i, dups[i], winner)
		}
	}
}
