package httphandler

// Integration tests for M4. TestCheckInGate is the spec's gate: three phones
// check in 60 people and record attendance at two workshops; duplicates show
// amber with time and staff name; the dashboard updates live.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

type checkinFixture struct {
	env    *saasEnv
	owner  *user
	phones []*user // owner + two check-in staff
	org    domain.OrgMembership
	ev     *domain.Event
	es     authz.EventScope // owner's
	api    string
	people []domain.EventParticipant
	tokens map[string]string // participant ID -> QR token
	cps    map[string]domain.Checkpoint
}

func newCheckinFixture(t *testing.T, n int) *checkinFixture {
	t.Helper()
	env := newSaaSEnv(t)
	ctx := context.Background()
	f := &checkinFixture{env: env, tokens: map[string]string{}, cps: map[string]domain.Checkpoint{}}
	f.owner = env.newBrowser(t, "owner")
	f.owner.signIn("")
	f.org = f.owner.createOrg("gate")
	starts := time.Now().Add(-time.Hour)
	_, b := f.owner.api("POST", "/api/v1/orgs/"+f.org.Slug+"/events", map[string]any{"name": "Example Event",
		"starts_at": starts, "ends_at": starts.Add(30 * time.Hour), "visibility": "unlisted"})
	f.ev = &domain.Event{}
	json.Unmarshal(b, f.ev)
	f.api = "/api/v1/orgs/" + f.org.Slug + "/events/" + f.ev.ID
	f.owner.api("POST", f.api+"/fields", map[string]any{"label": "Food", "type": "text", "visibility": "staff"})
	f.owner.api("POST", f.api+"/fields", map[string]any{"label": "Phone", "type": "text", "visibility": "admin"})

	f.phones = []*user{f.owner}
	for _, name := range []string{"gate2", "gate3"} {
		u := env.newBrowser(t, name)
		u.signIn("")
		f.owner.api("PUT", f.api+"/shares", map[string]string{"email": u.email, "role": "checkin"})
		f.phones = append(f.phones, u)
	}
	sc := authz.ForMember(authz.NewAccountScope(f.owner.id), f.org.ID, domain.OrgRoleOwner)
	acc, err := postgres.NewEventRepo(env.db).EventAccess(ctx, sc, f.ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.ev = acc.Event
	f.es, _ = authz.ForEvent(sc, f.ev.ID, f.ev.OwnerID, "", f.ev.Settings)
	regs := postgres.NewRegistrationRepo(env.db)
	ps := service.NewPassService(passes.DerivedKeyRing([]byte("test-pass-key")))
	outbox := postgres.NewOutboxRepo(env.db)
	for i := 0; i < n; i++ {
		p, err := regs.Register(ctx, f.ev, domain.EventParticipant{Email: fmt.Sprintf("%sp%02d@test.local", env.prefix, i),
			FullName: fmt.Sprintf("Person %02d", i), Answers: map[string]any{"food": "Vegan", "phone": "+94 77 000 0000"}},
			func() string { return fmt.Sprintf("SAC-%s-%02d", env.prefix[3:7], i) })
		if err != nil {
			t.Fatal(err)
		}
		f.people = append(f.people, *p)
		pc, err := outbox.PassContext(ctx, f.org.ID, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[p.ID], _ = ps.Token(pc)
	}
	cps, _ := postgres.NewCheckinRepo(env.db).Checkpoints(ctx, f.es, false)
	for _, c := range cps {
		f.cps[c.Name] = c
	}
	return f
}

func (f *checkinFixture) scan(t *testing.T, u *user, payload, checkpoint string, key string) service.ScanResult {
	t.Helper()
	if key == "" {
		key = fmt.Sprintf("k-%d-%s", time.Now().UnixNano(), u.email)
	}
	status, b := u.api("POST", f.api+"/scan", map[string]any{"payload": payload, "checkpoint_id": checkpoint,
		"record": true, "idempotency_key": key, "device_id": u.email})
	if status != http.StatusOK {
		t.Fatalf("scan: HTTP %d %s", status, b)
	}
	var r service.ScanResult
	json.Unmarshal(b, &r)
	return r
}

func TestCheckInGate(t *testing.T) {
	f := newCheckinFixture(t, 60)
	checkIn := f.cps["Check in"].ID

	// Three phones scan everyone at the main entrance at the same time.
	var mu sync.Mutex
	counts := map[string]int{}
	var dupDetails []string
	var wg sync.WaitGroup
	for _, phone := range f.phones {
		wg.Add(1)
		go func(phone *user) {
			defer wg.Done()
			for _, p := range f.people {
				r := f.scan(t, phone, f.tokens[p.ID], checkIn, "")
				mu.Lock()
				counts[r.Result]++
				if r.Result == domain.ScanDuplicate {
					dupDetails = append(dupDetails, r.Detail+"|"+r.Tone)
				}
				mu.Unlock()
			}
		}(phone)
	}
	wg.Wait()
	if counts[domain.ScanRecorded] != 60 || counts[domain.ScanDuplicate] != 120 {
		t.Fatalf("3 phones x 60 people: %v; want 60 recorded, 120 duplicates", counts)
	}
	for _, d := range dupDetails {
		if !strings.Contains(d, " by ") || !strings.HasSuffix(d, "|amber") {
			t.Fatalf("duplicate without time and staff name, or not amber: %q", d)
		}
	}

	// The dashboard counts them.
	status, b := f.owner.api("GET", f.api+"/stats", nil)
	var st domain.EventStats
	json.Unmarshal(b, &st)
	if status != http.StatusOK || st.CheckedIn != 60 || st.Participants["registered"] != 60 {
		t.Fatalf("stats: HTTP %d, checked in %d", status, st.CheckedIn)
	}

	// Two workshops: one needs a sign-up (capacity 20, 15 signed up), one is
	// open to everyone.
	at := time.Now().Add(-10 * time.Minute)
	_, b = f.owner.api("POST", f.api+"/workshops", map[string]any{"title": "Satellite data", "signup_mode": "opt_in",
		"capacity": 20, "starts_at": at, "ends_at": at.Add(time.Hour), "room": "Hall B"})
	var sat domain.Workshop
	json.Unmarshal(b, &sat)
	_, b = f.owner.api("POST", f.api+"/workshops", map[string]any{"title": "Opening talk", "signup_mode": "everyone"})
	var talk domain.Workshop
	json.Unmarshal(b, &talk)
	if sat.CheckpointID == "" || talk.CheckpointID == "" {
		t.Fatalf("workshops have no attendance checkpoint: %+v %+v", sat, talk)
	}
	for _, p := range f.people[:15] {
		if status, b := f.owner.api("POST", f.api+"/workshops/"+sat.ID+"/signups", map[string]string{"participant_id": p.ID}); status != http.StatusOK {
			t.Fatalf("sign up: %d %s", status, b)
		}
	}
	phone := f.phones[1]
	for _, p := range f.people[:15] {
		if r := f.scan(t, phone, f.tokens[p.ID], sat.CheckpointID, ""); r.Result != domain.ScanRecorded {
			t.Fatalf("signed-up person at the workshop door: %+v", r)
		}
	}
	// Not signed up: amber, and staff may add them while there's room.
	walkIn := f.people[20]
	r := f.scan(t, phone, f.tokens[walkIn.ID], sat.CheckpointID, "")
	if r.Result != domain.ScanNotSignedUp || r.Tone != "amber" || !r.CanAdd || r.PlacesLeft != 5 {
		t.Fatalf("walk-in at the workshop door: %+v", r)
	}
	if status, _ := phone.api("POST", f.api+"/workshops/"+sat.ID+"/signups", map[string]string{"participant_id": walkIn.ID}); status != http.StatusOK {
		t.Fatalf("check-in staff adding a walk-in: HTTP %d", status)
	}
	if r := f.scan(t, phone, f.tokens[walkIn.ID], sat.CheckpointID, ""); r.Result != domain.ScanRecorded {
		t.Fatalf("walk-in after being added: %+v", r)
	}
	for _, p := range f.people[30:40] {
		if r := f.scan(t, f.phones[2], f.tokens[p.ID], talk.CheckpointID, ""); r.Result != domain.ScanRecorded {
			t.Fatalf("open workshop: %+v", r)
		}
	}
	_, b = f.owner.api("GET", f.api+"/workshops", nil)
	var ws []domain.Workshop
	json.Unmarshal(b, &ws)
	attended := map[string]int{}
	for _, w := range ws {
		attended[w.Title] = w.Attended
	}
	if attended["Satellite data"] != 16 || attended["Opening talk"] != 10 {
		t.Errorf("workshop attendance: %v", attended)
	}

	// A pass for another event is red, not recorded.
	other, _ := passes.DerivedKeyRing([]byte("test-pass-key")).Issue(f.org.ID, "01a15000-0000-7000-8000-0000000000ee", f.people[0].ID, "X")
	if r := f.scan(t, phone, other, checkIn, ""); r.Result != service.CheckWrongEvent || r.Tone != "red" {
		t.Errorf("pass for another event: %+v", r)
	}
	if r := f.scan(t, phone, "garbage", checkIn, ""); r.Result != service.CheckInvalid {
		t.Errorf("not a pass: %+v", r)
	}
}

func TestCheckpointRulesUndoAndLookup(t *testing.T) {
	f := newCheckinFixture(t, 4)
	owner, staff, staff2 := f.phones[0], f.phones[1], f.phones[2]
	checkIn, kit := f.cps["Check in"].ID, f.cps["Kit"].ID
	p0, p1, p2, p3 := f.people[0], f.people[1], f.people[2], f.people[3]

	// Kit needs Check in first.
	if r := f.scan(t, staff, f.tokens[p0.ID], kit, ""); r.Result != domain.ScanNotAllowed || !strings.Contains(r.Message, "Check in") {
		t.Fatalf("kit before check-in: %+v", r)
	}
	f.scan(t, staff, f.tokens[p0.ID], checkIn, "")
	if r := f.scan(t, staff, f.tokens[p0.ID], kit, ""); r.Result != domain.ScanRecorded {
		t.Fatalf("kit after check-in: %+v", r)
	}

	// A retry with the same key returns the same check-in, not a duplicate.
	a := f.scan(t, staff, f.tokens[p1.ID], checkIn, "retry-key-1")
	b2 := f.scan(t, staff, f.tokens[p1.ID], checkIn, "retry-key-1")
	if a.Result != domain.ScanRecorded || b2.Result != domain.ScanRecorded || a.Checkin.ID != b2.Checkin.ID {
		t.Fatalf("idempotent retry: %+v / %+v", a, b2)
	}

	// Undo: check-in staff their own within 30 s; not others'; editors any.
	if !a.Undoable {
		t.Error("own fresh scan isn't marked undoable")
	}
	theirs := f.scan(t, staff2, f.tokens[p2.ID], checkIn, "")
	if status, _ := staff.api("POST", f.api+"/checkins/"+theirs.Checkin.ID+"/undo", nil); status != http.StatusForbidden {
		t.Errorf("staff undoing someone else's scan: HTTP %d, want 403", status)
	}
	if status, _ := staff.api("POST", f.api+"/checkins/"+a.Checkin.ID+"/undo", nil); status != http.StatusNoContent {
		t.Errorf("staff undoing own scan: HTTP %d, want 204", status)
	}
	if r := f.scan(t, staff, f.tokens[p1.ID], checkIn, ""); r.Result != domain.ScanRecorded {
		t.Errorf("scan again after undo: %+v", r)
	}
	old := f.scan(t, staff, f.tokens[p3.ID], checkIn, "")
	f.env.db.ExecContext(context.Background(), `UPDATE checkins SET created_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, old.Checkin.ID)
	if status, _ := staff.api("POST", f.api+"/checkins/"+old.Checkin.ID+"/undo", nil); status != http.StatusForbidden {
		t.Errorf("staff undoing own scan after 30 s: HTTP %d, want 403", status)
	}
	if status, _ := owner.api("POST", f.api+"/checkins/"+old.Checkin.ID+"/undo", nil); status != http.StatusNoContent {
		t.Errorf("owner undoing any scan: HTTP %d, want 204", status)
	}
	if status, _ := owner.api("POST", f.api+"/checkins/"+old.Checkin.ID+"/undo", nil); status != http.StatusConflict {
		t.Errorf("undo twice: HTTP %d, want 409", status)
	}
	// The undo is in the audit log; the check-in row stays.
	var n int
	f.env.db.QueryRowContext(context.Background(), `SELECT count(*) FROM checkins WHERE id = $1 AND undone_at IS NOT NULL`, old.Checkin.ID).Scan(&n)
	if n != 1 {
		t.Error("undone check-in isn't kept with undone_at")
	}
	_, b := owner.api("GET", f.api+"/activity", nil)
	if !strings.Contains(string(b), `"checkin.undo"`) {
		t.Error("undo missing from the activity log")
	}

	// A checkpoint only editors may record, and one that's closed.
	_, b = owner.api("POST", f.api+"/checkpoints", map[string]any{"name": "VIP lounge", "rules": map[string]any{"roles": []string{"editor", "owner"}, "repeatable": true}})
	var vip domain.Checkpoint
	json.Unmarshal(b, &vip)
	if r := f.scan(t, staff, f.tokens[p0.ID], vip.ID, ""); r.Result != domain.ScanNotAllowed {
		t.Errorf("check-in staff at an editors-only checkpoint: %+v", r)
	}
	if r1, r2 := f.scan(t, owner, f.tokens[p0.ID], vip.ID, ""), f.scan(t, owner, f.tokens[p0.ID], vip.ID, ""); r1.Result != domain.ScanRecorded || r2.Result != domain.ScanRecorded {
		t.Errorf("repeatable checkpoint twice: %s, %s", r1.Result, r2.Result)
	}
	past := time.Now().Add(-time.Hour)
	_, b = owner.api("POST", f.api+"/checkpoints", map[string]any{"name": "Breakfast", "rules": map[string]any{"closes_at": past}})
	var breakfast domain.Checkpoint
	json.Unmarshal(b, &breakfast)
	if r := f.scan(t, staff, f.tokens[p0.ID], breakfast.ID, ""); r.Result != domain.ScanOutsideHours {
		t.Errorf("closed time window: %+v", r)
	}
	owner.api("POST", f.api+"/checkpoints/"+breakfast.ID+"/archive", nil)
	if status, _ := staff.api("POST", f.api+"/scan", map[string]any{"payload": f.tokens[p0.ID], "checkpoint_id": breakfast.ID,
		"record": true, "idempotency_key": "closed"}); status != http.StatusConflict {
		t.Errorf("scan at an archived checkpoint: HTTP %d, want 409", status)
	}

	// Check only (no fast lane), then record from lookup.
	status, b := staff.api("POST", f.api+"/scan", map[string]any{"payload": f.tokens[p2.ID], "checkpoint_id": kit, "record": false})
	var ready service.ScanResult
	json.Unmarshal(b, &ready)
	if status != http.StatusOK || ready.Result != domain.ScanReady {
		t.Errorf("check without recording: %+v", ready)
	}
	status, b = staff.api("GET", f.api+"/lookup?q="+p2.PassID, nil)
	var found []service.ScanParticipant
	json.Unmarshal(b, &found)
	if status != http.StatusOK || len(found) != 1 || found[0].ID != p2.ID {
		t.Fatalf("lookup by pass ID: %d %s", status, b)
	}
	for _, a := range found[0].Answers {
		if a.Label == "Phone" {
			t.Error("lookup shows an organizers-only answer to check-in staff")
		}
	}
	status, b = staff.api("POST", f.api+"/checkpoints/"+kit+"/record", map[string]any{"participant_id": p2.ID, "idempotency_key": "lookup-1"})
	var rec service.ScanResult
	json.Unmarshal(b, &rec)
	if status != http.StatusOK || rec.Result != domain.ScanRecorded {
		t.Errorf("record from lookup: %d %s", status, b)
	}

	// A cancelled registration's pass is red.
	owner.api("PATCH", f.api+"/participants/"+p3.ID, map[string]string{"status": "cancelled"})
	if r := f.scan(t, staff, f.tokens[p3.ID], kit, ""); r.Result != service.CheckCancelled || r.Tone != "red" {
		t.Errorf("cancelled pass: %+v", r)
	}
}

func TestOrganizerOnlyParticipantDataHiddenFromEventRoles(t *testing.T) {
	f := newCheckinFixture(t, 1)
	participant := f.people[0]
	if status, b := f.owner.api("POST", f.api+"/fields", map[string]any{
		"label": "Medical note", "type": "text", "visibility": "admin",
	}); status != http.StatusCreated {
		t.Fatalf("add organizer-only field: HTTP %d %s", status, b)
	}

	answers, err := json.Marshal(map[string]string{
		"food": "Vegan", "phone": "+94 77 000 0000", "medical_note": "Restricted note", "birth_date": "2000-01-02",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.env.db.ExecContext(context.Background(),
		`UPDATE event_participants SET answers = $1::jsonb, birth_date = $2 WHERE id = $3`,
		answers, "2000-01-02", participant.ID); err != nil {
		t.Fatal(err)
	}

	viewer := f.env.newBrowser(t, "restricted-viewer")
	viewer.signIn("")
	if status, b := f.owner.api("PUT", f.api+"/shares", map[string]string{"email": viewer.email, "role": "viewer"}); status != http.StatusCreated {
		t.Fatalf("share event with viewer: HTTP %d %s", status, b)
	}

	assertRestricted := func(label string, body []byte) {
		t.Helper()
		var response struct {
			Participant map[string]json.RawMessage `json:"participant"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatalf("%s response: %v: %s", label, err, body)
		}
		assertRedactedParticipant(t, label, response.Participant)
	}

	status, b := viewer.api("GET", f.api+"/participants", nil)
	if status != http.StatusOK {
		t.Fatalf("viewer participant list: HTTP %d %s", status, b)
	}
	var list struct {
		Participants []map[string]json.RawMessage `json:"participants"`
	}
	if err := json.Unmarshal(b, &list); err != nil || len(list.Participants) != 1 {
		t.Fatalf("viewer participant list: %v: %s", err, b)
	}
	assertRedactedParticipant(t, "viewer participant list", list.Participants[0])

	for _, caller := range []struct {
		name string
		user *user
	}{{"viewer", viewer}, {"check-in staff", f.phones[1]}} {
		status, b = caller.user.api("POST", f.api+"/passes/check", map[string]string{"payload": f.tokens[participant.ID]})
		if status != http.StatusOK || !strings.Contains(string(b), `"result":"valid"`) {
			t.Fatalf("%s pass check: HTTP %d %s", caller.name, status, b)
		}
		assertRestricted(caller.name+" pass check", b)
	}

	status, b = f.owner.api("GET", f.api+"/participants", nil)
	if status != http.StatusOK {
		t.Fatalf("owner participant list: HTTP %d %s", status, b)
	}
	if !strings.Contains(string(b), `"birth_date":"2000-01-02"`) ||
		!strings.Contains(string(b), `"medical_note":"Restricted note"`) {
		t.Errorf("owner participant list lost organizer data: %s", b)
	}
	status, b = f.owner.api("POST", f.api+"/passes/check", map[string]string{"payload": f.tokens[participant.ID]})
	if status != http.StatusOK || !strings.Contains(string(b), `"birth_date":"2000-01-02"`) ||
		!strings.Contains(string(b), `"medical_note":"Restricted note"`) {
		t.Errorf("owner pass check lost organizer data: HTTP %d %s", status, b)
	}
}

func assertRedactedParticipant(t *testing.T, label string, participant map[string]json.RawMessage) {
	t.Helper()
	if participant == nil {
		t.Fatalf("%s omitted the participant", label)
	}
	for _, key := range []string{"birth_date", "age"} {
		if _, ok := participant[key]; ok {
			t.Errorf("%s exposed %s", label, key)
		}
	}
	var answers map[string]json.RawMessage
	if err := json.Unmarshal(participant["answers"], &answers); err != nil {
		t.Fatalf("%s answers: %v", label, err)
	}
	if _, ok := answers["food"]; !ok {
		t.Errorf("%s omitted a non-restricted answer", label)
	}
	for _, key := range []string{"phone", "medical_note", "birth_date"} {
		if _, ok := answers[key]; ok {
			t.Errorf("%s exposed organizer-only answer %q", label, key)
		}
	}
}

// The live stream says "change" soon after a scan on another phone.
func TestLiveStream(t *testing.T) {
	f := newCheckinFixture(t, 1)
	req, _ := http.NewRequest("GET", f.env.srv.URL+f.api+"/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := f.owner.c.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: HTTP %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			if sc.Text() == "event: change" {
				got <- sc.Text()
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	f.scan(t, f.phones[1], f.tokens[f.people[0].ID], f.cps["Check in"].ID, "")
	select {
	case <-got:
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("change arrived after %s", d)
		}
	case <-ctx.Done():
		t.Fatal("no change event after a scan")
	}

	// Viewers may watch; people outside the event may not.
	outsider := f.env.newBrowser(t, "outsider")
	outsider.signIn("")
	if status, _ := outsider.api("GET", f.api+"/stats", nil); status != http.StatusNotFound {
		t.Errorf("outsider stats: HTTP %d", status)
	}
}

// Participants sign up for workshops in the portal: clashes are refused, a
// full workshop puts them on the waitlist, cancelling moves the waitlist up.
func TestPortalWorkshopSignups(t *testing.T) {
	f := newCheckinFixture(t, 0)
	at := time.Now().Add(24 * time.Hour)
	mk := func(title string, capacity int, start time.Time) domain.Workshop {
		_, b := f.owner.api("POST", f.api+"/workshops", map[string]any{"title": title, "capacity": capacity,
			"starts_at": start, "ends_at": start.Add(time.Hour), "signup_mode": "opt_in"})
		var w domain.Workshop
		json.Unmarshal(b, &w)
		return w
	}
	a := mk("Rocketry", 1, at)
	clash := mk("Robotics", 5, at.Add(30*time.Minute))
	later := mk("Astronomy", 5, at.Add(3*time.Hour))

	f.owner.api("POST", f.api+"/publish", nil)
	anon := f.env.newBrowser(t, "anon")
	people := []*user{f.env.newBrowser(t, "ama"), f.env.newBrowser(t, "kas")}
	var passIDs []string
	for _, u := range people {
		res := anon.form("POST", "/e/"+f.org.Slug+"/"+f.ev.Slug+"/register", map[string][]string{"full_name": {"Name " + u.email[:6]}, "email": {u.email}})
		res.Body.Close()
		u.signIn("")
		_, b := u.api("GET", "/api/v1/me/passes", nil)
		var mine []domain.EventParticipant
		json.Unmarshal(b, &mine)
		if len(mine) != 1 {
			t.Fatalf("%s passes: %s", u.email, b)
		}
		passIDs = append(passIDs, mine[0].ID)
	}
	act := func(u *user, pid, wid, verb string) (int, string) {
		res := u.form("POST", "/me/passes/"+pid+"/workshops/"+wid+"/"+verb, map[string][]string{})
		return res.StatusCode, body(res)
	}
	if status, page := act(people[0], passIDs[0], a.ID, "signup"); status != http.StatusOK || !strings.Contains(page, "re signed up.") {
		t.Fatalf("sign up: %d", status)
	}
	if status, page := act(people[0], passIDs[0], clash.ID, "signup"); status != http.StatusConflict || !strings.Contains(page, "clashes with Rocketry") {
		t.Errorf("clashing sign-up: %d", status)
	}
	if status, _ := act(people[0], passIDs[0], later.ID, "signup"); status != http.StatusOK {
		t.Errorf("non-clashing sign-up: %d", status)
	}
	if _, page := act(people[1], passIDs[1], a.ID, "signup"); !strings.Contains(page, "on its waitlist") {
		t.Error("full workshop: not put on the waitlist")
	}
	// Someone else's pass can't be used to sign them up.
	if status, _ := act(people[1], passIDs[0], later.ID, "cancel"); status != http.StatusNotFound {
		t.Errorf("acting on another person's pass: %d, want 404", status)
	}
	// Ama cancels; Kas moves up from the waitlist.
	act(people[0], passIDs[0], a.ID, "cancel")
	_, b := f.owner.api("GET", f.api+"/workshops/"+a.ID+"/signups", nil)
	var su []domain.WorkshopSignup
	json.Unmarshal(b, &su)
	if len(su) != 1 || su[0].ParticipantID != passIDs[1] || su[0].Status != domain.SignupSignedUp {
		t.Errorf("after cancelling: %+v; want Kas signed up from the waitlist", su)
	}
}
