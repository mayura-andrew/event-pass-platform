package httphandler

// Integration tests for M2: events, sharing and the audit log. Same setup as
// saas_integration_test.go (EVENT_PASS_TEST_DATABASE_URL, it-<tag>-* rows).
//
// TestEventAuthorizationMatrix is generated from eventRoutes and the role
// matrix: every route, called by every kind of person, must allow exactly
// what authz allows, and every allowed change must write its audit entry.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
)

// persona is someone calling event routes, and how they relate to the event.
type persona struct {
	name       string
	u          *user  // nil: signed out
	orgRole    string // in the event's org; "" outside it
	share      string // their share on the event
	eventOwner bool
}

type matrixFixture struct {
	t       *testing.T
	env     *saasEnv
	org     domain.OrgMembership
	owner   *user // org owner
	evOwner *user // member who owns each test event
	member  *user // member with no share (transfer target)
	people  []persona
	events  *postgres.EventRepo
	ownerSc authz.OrgScope // evOwner's scope in the org
}

func newMatrixFixture(t *testing.T) *matrixFixture {
	t.Helper()
	env := newSaaSEnv(t)
	f := &matrixFixture{t: t, env: env, events: postgres.NewEventRepo(env.db)}
	b := func(name string) *user { u := env.newBrowser(t, name); u.signIn(""); return u }
	f.owner = b("orgowner")
	f.evOwner, f.member = b("evowner"), b("member")
	admin, editor, checkin, viewer := b("admin"), b("editor"), b("checkin"), b("viewer")
	guest, outsider := b("guest"), b("outsider")
	f.org = f.owner.createOrg("m")
	outsider.createOrg("other")
	for u, role := range map[*user]string{admin: "admin", f.evOwner: "member", editor: "member", checkin: "member", viewer: "member", f.member: "member"} {
		f.owner.invite(f.org.Slug, u.email, role)
		u.join()
	}
	f.ownerSc = authz.ForMember(authz.NewAccountScope(f.evOwner.id), f.org.ID, domain.OrgRoleMember)
	f.people = []persona{
		{name: "org owner", u: f.owner, orgRole: domain.OrgRoleOwner},
		{name: "org admin", u: admin, orgRole: domain.OrgRoleAdmin},
		{name: "event owner", u: f.evOwner, orgRole: domain.OrgRoleMember, eventOwner: true},
		{name: "editor", u: editor, orgRole: domain.OrgRoleMember, share: domain.EventRoleEditor},
		{name: "check-in staff", u: checkin, orgRole: domain.OrgRoleMember, share: domain.EventRoleCheckin},
		{name: "viewer", u: viewer, orgRole: domain.OrgRoleMember, share: domain.EventRoleViewer},
		{name: "guest check-in staff", u: guest, orgRole: domain.OrgRoleGuest, share: domain.EventRoleCheckin},
		{name: "member without a share", u: f.member, orgRole: domain.OrgRoleMember},
		{name: "member of another org", u: outsider},
		{name: "signed out"},
	}
	return f
}

// testEvent is a fresh event owned by evOwner, shared with the personas'
// roles, plus one pending invite: the IDs routes need.
type testEvent struct {
	id, editorShare, invite, field, participant string
	checkpoint, workshop, checkin               string
	ev                                          *domain.Event
	es                                          authz.EventScope
}

func (f *matrixFixture) newEvent(t *testing.T) testEvent {
	t.Helper()
	ctx := context.Background()
	starts := time.Now().Add(30 * 24 * time.Hour)
	e, err := f.events.CreateEvent(ctx, f.ownerSc, &domain.Event{
		Slug: "matrix", Name: "Matrix event", Type: "hackathon", TimeZone: "UTC", Visibility: "unlisted", Language: "en",
		Tags: []string{}, StartsAt: &starts, RegMode: domain.RegOpen, Waitlist: true, PassPrefix: "MTX",
	}, "", domain.Audit{Action: "event.create", TargetType: "event"})
	if err != nil {
		t.Fatal(err)
	}
	es, _ := authz.ForEvent(f.ownerSc, e.ID, e.OwnerID, "", e.Settings)
	te := testEvent{id: e.ID, es: es}
	for _, p := range f.people {
		if p.share == "" {
			continue
		}
		s, err := f.events.PutShare(ctx, es, p.u.id, p.share)
		if err != nil {
			t.Fatal(err)
		}
		if p.share == domain.EventRoleEditor {
			te.editorShare = s.ID
		}
	}
	inv, err := f.events.CreateInvite(ctx, es, f.env.prefix+"pending-"+e.ID[len(e.ID)-6:]+"@test.local", "viewer",
		[]byte("inv-"+e.ID), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	te.invite = inv.ID
	regs := postgres.NewRegistrationRepo(f.env.db)
	field, err := regs.SaveField(ctx, es, domain.FormField{Key: "setup_question", Type: "text", Label: "Setup question", Visibility: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	te.field = field.ID
	p, err := regs.Register(ctx, e, domain.EventParticipant{Email: f.env.prefix + "reg-" + e.ID[len(e.ID)-6:] + "@test.local",
		FullName: "Matrix Participant", Answers: map[string]any{}}, func() string { return "MTX-" + e.ID[len(e.ID)-9:] })
	if err != nil {
		t.Fatal(err)
	}
	te.participant, te.ev = p.ID, e
	cps, err := postgres.NewCheckinRepo(f.env.db).Checkpoints(ctx, es, false)
	if err != nil || len(cps) == 0 {
		t.Fatalf("default checkpoints: %v %v", cps, err)
	}
	for _, c := range cps {
		if c.Kind == domain.KindCheckIn {
			te.checkpoint = c.ID
		}
	}
	capacity := 10
	ws, err := postgres.NewWorkshopRepo(f.env.db).Save(ctx, es, domain.Workshop{Title: "Matrix workshop",
		SignupMode: domain.SignupOptIn, Capacity: &capacity, Waitlist: true})
	if err != nil {
		t.Fatal(err)
	}
	te.workshop = ws.ID
	return te
}

// expected is what p may do on route rt: "ok", "forbidden" (has access,
// lacks the action), "hidden" (no access: 404) or "signin".
func (f *matrixFixture) expected(p persona, rt eventRoute) string {
	if p.u == nil {
		return "signin"
	}
	if p.orgRole == "" {
		return "hidden"
	}
	orgSc := authz.ForMember(authz.NewAccountScope(p.u.id), f.org.ID, p.orgRole)
	ownerID := "someone-else"
	if p.eventOwner {
		ownerID = p.u.id
	}
	es, ok := authz.ForEvent(orgSc, "e", ownerID, p.share, domain.EventSettings{})
	switch {
	case !ok:
		return "hidden"
	case rt.action == authz.EventDelete && strings.HasSuffix(rt.pattern, "{event}/restore") && !es.Can(authz.EventDelete):
		return "hidden" // a trashed event only opens for those who can restore it
	case !es.Can(rt.action):
		return "forbidden"
	case strings.HasSuffix(rt.pattern, "/duplicate") && !orgSc.Can(authz.CreateEvent):
		return "forbidden"
	}
	return "ok"
}

func (f *matrixFixture) call(p persona, rt eventRoute, te testEvent) (int, string) {
	path := fill(rt.pattern, map[string]string{"org": f.org.Slug, "event": te.id, "share": te.editorShare, "id": te.invite,
		"field": te.field, "participant": te.participant, "checkpoint": te.checkpoint, "workshop": te.workshop,
		"checkin": te.checkin})
	u := p.u
	if u == nil {
		u = f.env.newBrowser(f.t, "anon")
	}
	if rt.api {
		var body any
		switch rt.method {
		case "POST", "PATCH", "PUT":
			body = map[string]any{"name": "Matrix renamed", "role": "viewer", "email": f.env.prefix + "new-" + te.id[len(te.id)-6:] + "@test.local",
				"account_id": f.member.id, "editors_can_share": true, "expires_in_days": 7,
				"label": "Matrix question", "status": "cancelled", "title": "Matrix workshop 2",
				"payload": "not a pass", "checkpoint_id": te.checkpoint, "record": true,
				"idempotency_key": "matrix-" + te.id, "participant_id": te.participant}
			if strings.Contains(rt.pattern, "/fields") {
				body.(map[string]any)["type"] = "text"
			}
		}
		status, _ := u.api(rt.method, path, body)
		return status, ""
	}
	var res *http.Response
	if rt.method == "GET" {
		res = u.get(path)
	} else {
		form := url.Values{"name": {"Matrix renamed"}, "type": {"hackathon"}, "time_zone": {"UTC"},
			"visibility": {"private"}, "role": {"viewer"}, "email": {f.env.prefix + "new-" + te.id[len(te.id)-6:] + "@test.local"},
			"account_id": {f.member.id}, "editors_can_share": {"on"}, "expires_in_days": {"7"},
			"label": {"Matrix question"}, "status": {"cancelled"}, "reg_mode": {"open"}, "pass_prefix": {"MTX"},
			"title": {"Matrix workshop 2"}, "signup_mode": {"opt_in"}}
		if strings.Contains(rt.pattern, "/fields") {
			form.Set("type", "text") // a question's answer type and audience, not the event's
			form.Set("visibility", "admin")
		}
		res = u.form(rt.method, path, form)
	}
	res.Body.Close()
	return res.StatusCode, res.Request.URL.Path
}

func TestEventAuthorizationMatrix(t *testing.T) {
	f := newMatrixFixture(t)
	ctx := context.Background()
	for _, rt := range eventRoutes {
		if strings.HasSuffix(rt.pattern, "/stream") {
			continue // never ends; TestLiveStream covers it
		}
		for _, p := range f.people {
			te := f.newEvent(t)
			switch {
			case strings.HasSuffix(rt.pattern, "{event}/restore"):
				if err := f.events.SetDeleted(ctx, te.es, true); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(rt.pattern, "/unarchive"):
				if err := f.events.SetArchived(ctx, te.es, true); err != nil {
					t.Fatal(err)
				}
			case strings.HasSuffix(rt.pattern, "/checkins/{checkin}/undo"):
				// The caller's own scan, just now: what check-in staff may undo.
				who := f.ownerSc
				if p.u != nil && p.orgRole != "" {
					who = authz.ForMember(authz.NewAccountScope(p.u.id), f.org.ID, p.orgRole)
				}
				wes, _ := authz.ForEvent(who, te.id, te.ev.OwnerID, p.share, te.ev.Settings)
				if wes.EventID() == "" { // no access: record as the owner
					wes = te.es
				}
				rec, _, _, err := postgres.NewCheckinRepo(f.env.db).Record(ctx, wes, domain.RecordInput{
					ParticipantID: te.participant, CheckpointID: te.checkpoint, IdempotencyKey: "undo-" + te.id, Once: true})
				if err != nil {
					t.Fatal(err)
				}
				te.checkin = rec.ID
			case rt.audit == "workshop.signup.cancel":
				if _, err := postgres.NewWorkshopRepo(f.env.db).StaffSignUp(ctx, te.es, te.workshop, te.participant); err != nil {
					t.Fatal(err)
				}
			case rt.audit == "checkpoint.restore":
				if err := postgres.NewCheckinRepo(f.env.db).SetCheckpointArchived(ctx, te.es, te.checkpoint, true); err != nil {
					t.Fatal(err)
				}
			case rt.audit == "event.unpublish":
				if err := f.events.SetPublished(ctx, te.es, true); err != nil {
					t.Fatal(err)
				}
			case rt.audit == "event.link.off":
				if err := f.events.SetLink(ctx, te.es, []byte("link-"+te.id), nil); err != nil {
					t.Fatal(err)
				}
			}
			want := f.expected(p, rt)
			got, landed := f.call(p, rt, te)

			okStatus := got >= 200 && got < 400
			var pass bool
			switch want {
			case "ok":
				pass = okStatus && landed != "/login"
			case "forbidden":
				pass = got == http.StatusForbidden
			case "hidden":
				pass = got == http.StatusNotFound
			case "signin":
				pass = (rt.api && got == http.StatusUnauthorized) || (!rt.api && landed == "/login")
			}
			if !pass {
				t.Errorf("%s %s as %s: HTTP %d (at %s), want %s", rt.method, rt.pattern, p.name, got, landed, want)
				continue
			}
			if want == "ok" && rt.audit != "" {
				var n int
				if err := f.env.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_log
					WHERE action = $1 AND (event_id = $2 OR diff->>'from_event' = $3)`, rt.audit, te.id, te.id).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n == 0 {
					t.Errorf("%s %s as %s: no %q entry in the audit log", rt.method, rt.pattern, p.name, rt.audit)
				}
			}
		}
	}

	// An event ID from another org is unknown in yours, even to its owner.
	other := f.people[8].u // member of another org, owns it
	te := f.newEvent(t)
	var otherSlug string
	f.env.db.QueryRowContext(ctx, `SELECT o.slug FROM orgs o JOIN org_members m ON m.org_id = o.id WHERE m.account_id = $1`, other.id).Scan(&otherSlug)
	for _, rt := range eventRoutes {
		if !rt.api {
			continue
		}
		path := fill(rt.pattern, map[string]string{"org": otherSlug, "event": te.id, "share": te.editorShare, "id": te.invite})
		if got, _ := other.api(rt.method, path, map[string]any{}); got != http.StatusNotFound {
			t.Errorf("%s %s with another org's event ID: HTTP %d, want 404", rt.method, path, got)
		}
	}
}

func TestEventsHomeViews(t *testing.T) {
	env := newSaaSEnv(t)
	ann, ben := env.newBrowser(t, "ann"), env.newBrowser(t, "ben")
	ann.signIn("")
	ben.signIn("")
	org := ann.createOrg("home")
	ann.invite(org.Slug, ben.email, "member")
	ben.join()
	base := "/api/v1/orgs/" + org.Slug + "/events"

	create := func(u *user, name string, extra map[string]any) domain.Event {
		t.Helper()
		body := map[string]any{"name": name, "time_zone": "Asia/Colombo", "venue": map[string]any{"name": "BMICH"}, "tags": []string{"Space", "2026"}}
		for k, v := range extra {
			body[k] = v
		}
		status, b := u.api("POST", base, body)
		if status != http.StatusCreated {
			t.Fatalf("create %s: HTTP %d %s", name, status, b)
		}
		var e domain.Event
		json.Unmarshal(b, &e)
		return e
	}
	list := func(u *user, query string) []string {
		t.Helper()
		status, b := u.api("GET", base+"?"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("list %s: HTTP %d %s", query, status, b)
		}
		var cards []domain.EventCard
		json.Unmarshal(b, &cards)
		names := []string{}
		for _, c := range cards {
			names = append(names, c.Name)
		}
		return names
	}
	eq := func(what string, got []string, want ...string) {
		t.Helper()
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: %v, want %v", what, got, want)
		}
	}

	hack := create(ben, "Hack Night", map[string]any{"starts_at": "2026-11-02T18:00:00+05:30", "ends_at": "2026-11-03T06:00:00+05:30"})
	if hack.Slug != "hack-night" || hack.DateRange() != "2–3 Nov 2026" {
		t.Errorf("slug %q, dates %q", hack.Slug, hack.DateRange())
	}
	second := create(ben, "Hack Night", nil)
	if second.Slug != "hack-night-2" {
		t.Errorf("second slug %q, want hack-night-2", second.Slug)
	}
	create(ann, "Annual Meetup", map[string]any{"type": "meetup"})

	eq("ben mine", list(ben, "view=mine&sort=name"), "Hack Night", "Hack Night")
	eq("ben shared (none)", list(ben, "view=shared"))
	eq("ann all (org owner sees every event)", list(ann, "view=all&sort=name"), "Annual Meetup", "Hack Night", "Hack Night")
	eq("ben all is only his own", list(ben, "view=all&sort=name"), "Hack Night", "Hack Night")

	// Sharing puts it in Shared with me; opening puts it in Recent.
	status, b := ann.api("PUT", base+"/"+create(ann, "Board Meeting", nil).ID+"/shares", map[string]string{"email": ben.email, "role": "viewer"})
	if status != http.StatusOK || !strings.Contains(string(b), `"share"`) {
		t.Fatalf("share with a member: HTTP %d %s", status, b)
	}
	eq("ben shared", list(ben, "view=shared"), "Board Meeting")
	ben.api("GET", base+"/"+hack.ID, nil)
	eq("ben recent", list(ben, "view=recent"), "Hack Night")
	ben.api("PUT", base+"/"+second.ID+"/star", nil)
	eq("ben starred", list(ben, "view=starred"), "Hack Night")
	if c := list(ann, "view=starred"); len(c) != 0 {
		t.Errorf("stars are personal: ann sees %v", c)
	}

	// Search by name, venue and tag; archive and trash move events out of
	// the main lists.
	eq("search name", list(ben, "view=mine&q=hack&sort=name"), "Hack Night", "Hack Night")
	eq("search tag", list(ann, "view=all&q=space&sort=name"), "Annual Meetup", "Board Meeting", "Hack Night", "Hack Night")
	eq("search venue", list(ann, "view=all&q=bmich&sort=name"), "Annual Meetup", "Board Meeting", "Hack Night", "Hack Night")
	eq("search wildcard is literal", list(ann, "view=all&q=%25"))
	ben.api("POST", base+"/"+second.ID+"/archive", nil)
	eq("archived", list(ben, "view=archived"), "Hack Night")
	eq("mine after archive", list(ben, "view=mine"), "Hack Night")
	eq("archived stays searchable", list(ben, "view=mine&q=hack&sort=name"), "Hack Night", "Hack Night")
	ben.api("DELETE", base+"/"+hack.ID, nil)
	eq("trash", list(ben, "view=trash"), "Hack Night")
	eq("mine after delete", list(ben, "view=mine"))
	if status, _ := ben.api("PATCH", base+"/"+hack.ID, map[string]string{"name": "Nope"}); status != http.StatusConflict {
		t.Errorf("edit in trash: HTTP %d, want 409", status)
	}
	ben.api("POST", base+"/"+hack.ID+"/restore", nil)
	eq("restored", list(ben, "view=mine"), "Hack Night")

	// The events home page renders every view.
	for _, v := range []string{"mine", "all", "shared", "recent", "starred", "archived", "trash"} {
		for _, layout := range []string{"grid", "list"} {
			res := ann.get("/o/" + org.Slug + "?view=" + v + "&layout=" + layout)
			if res.StatusCode != http.StatusOK {
				t.Errorf("events home view %s/%s: HTTP %d", v, layout, res.StatusCode)
			}
			res.Body.Close()
		}
	}
}

func TestGuestsLinksAndOrgSwitch(t *testing.T) {
	env := newSaaSEnv(t)
	ann, gita := env.newBrowser(t, "ann"), env.newBrowser(t, "gita")
	ann.signIn("")
	org := ann.createOrg("guests")
	base := "/api/v1/orgs/" + org.Slug + "/events"
	_, b := ann.api("POST", base, map[string]any{"name": "Example Event"})
	var ev domain.Event
	json.Unmarshal(b, &ev)

	// No account yet: an emailed invite, accepted after signing up.
	status, b := ann.api("PUT", base+"/"+ev.ID+"/shares", map[string]string{"email": gita.email, "role": "checkin"})
	if status != http.StatusOK || !strings.Contains(string(b), `"invite"`) {
		t.Fatalf("share with a new address: HTTP %d %s", status, b)
	}
	inviteToken := env.mail.lastToken(t, gita.email) // before signing in sends a newer email
	gita.signIn("")
	if status, b := gita.api("POST", "/api/v1/invites/accept", map[string]string{"token": inviteToken}); status != http.StatusOK {
		t.Fatalf("accept event invite: HTTP %d %s", status, b)
	}

	// The guest sees the org and only that event, not its members.
	status, b = gita.api("GET", "/api/v1/orgs", nil)
	if status != http.StatusOK || !strings.Contains(string(b), `"role":"guest"`) {
		t.Fatalf("guest's orgs: HTTP %d %s", status, b)
	}
	if status, _ := gita.api("GET", "/api/v1/orgs/"+org.Slug+"/members", nil); status != http.StatusForbidden {
		t.Errorf("guest lists members: HTTP %d, want 403", status)
	}
	if status, _ := gita.api("POST", base, map[string]any{"name": "Guest event"}); status != http.StatusForbidden {
		t.Errorf("guest creates an event: HTTP %d, want 403", status)
	}
	status, b = gita.api("GET", base+"?view=shared", nil)
	if status != http.StatusOK || !strings.Contains(string(b), "Example Event") {
		t.Fatalf("guest's shared view: HTTP %d %s", status, b)
	}
	res := gita.get("/o/" + org.Slug)
	if page := body(res); res.StatusCode != http.StatusOK || !strings.Contains(page, "Example Event") {
		t.Fatalf("guest's events home: HTTP %d", res.StatusCode)
	}

	// View-only link: works signed out, shows only the overview, and stops
	// working when turned off.
	status, b = ann.api("POST", base+"/"+ev.ID+"/link-share", map[string]int{"expires_in_days": 30})
	var link struct{ URL string }
	json.Unmarshal(b, &link)
	if status != http.StatusCreated || !strings.Contains(link.URL, "/l/") {
		t.Fatalf("enable link: HTTP %d %s", status, b)
	}
	anon := env.newBrowser(t, "anon")
	path := link.URL[strings.Index(link.URL, "/l/"):]
	if res := anon.get(path); res.StatusCode != http.StatusOK || !strings.Contains(body(res), "Example Event") {
		t.Fatalf("open link signed out: HTTP %d", res.StatusCode)
	}
	ann.api("DELETE", base+"/"+ev.ID+"/link-share", nil)
	if res := anon.get(path); res.StatusCode != http.StatusNotFound {
		t.Fatalf("link after turning off: HTTP %d, want 404", res.StatusCode)
	}

	// With outside sharing off, guests can't be added and links can't be
	// made; members still can be shared with.
	if status, _ := ann.api("PATCH", "/api/v1/orgs/"+org.Slug+"/settings", map[string]bool{"external_sharing": false}); status != http.StatusNoContent {
		t.Fatalf("turn off outside sharing: HTTP %d", status)
	}
	if status, _ := ann.api("PUT", base+"/"+ev.ID+"/shares", map[string]string{"email": env.prefix + "stranger@test.local", "role": "viewer"}); status != http.StatusForbidden {
		t.Errorf("outside share with the switch off: HTTP %d, want 403", status)
	}
	if status, _ := ann.api("POST", base+"/"+ev.ID+"/link-share", nil); status != http.StatusForbidden {
		t.Errorf("link with the switch off: HTTP %d, want 403", status)
	}

	// The audit log tells the story, newest first.
	status, b = ann.api("GET", base+"/"+ev.ID+"/activity", nil)
	var entries []domain.AuditEntry
	json.Unmarshal(b, &entries)
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
	}
	want := "event.link.off|event.link.on|invite.accept|event.invite.create|event.create"
	if status != http.StatusOK || strings.Join(actions, "|") != want {
		t.Errorf("event activity: %v, want %s", actions, want)
	}
	status, b = ann.api("GET", "/api/v1/orgs/"+org.Slug+"/activity", nil)
	if status != http.StatusOK || !strings.Contains(string(b), `"org.settings"`) || !strings.Contains(string(b), `"org.create"`) {
		t.Errorf("org activity: HTTP %d %s", status, b)
	}
	// The tenant role can't rewrite history.
	if _, err := env.db.ExecContext(context.Background(), `
		DO $$ BEGIN SET LOCAL ROLE event_pass_tenant; UPDATE audit_log SET action = 'x'; END $$`); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("tenant role updating the audit log: %v, want permission denied", err)
	}
}

// The pages render for an owner, including the one-time link.
func TestEventPagesRender(t *testing.T) {
	env := newSaaSEnv(t)
	ann := env.newBrowser(t, "ann")
	ann.signIn("")
	org := ann.createOrg("pages")
	res := ann.form("POST", "/o/"+org.Slug+"/events", url.Values{"name": {"Example Event"}, "type": {"hackathon"},
		"starts": {"2026-10-03T09:00"}, "ends": {"2026-10-04T18:00"}, "time_zone": {"Asia/Colombo"}, "visibility": {"private"},
		"venue_name": {"BMICH"}, "lat": {"6.9010"}, "lng": {"79.8722"}, "capacity": {"300"}, "tags": {"space, 2026"}})
	page := body(res)
	if !strings.Contains(res.Request.URL.Path, "/events/") || !strings.Contains(page, "Event created") || !strings.Contains(page, "3–4 Oct 2026") {
		t.Fatalf("create from the form landed on %s", res.Request.URL.Path)
	}
	eventPath := res.Request.URL.Path
	for _, tab := range []string{"", "/share", "/activity", "/settings"} {
		res := ann.get(eventPath + tab)
		if res.StatusCode != http.StatusOK {
			t.Errorf("event tab %q: HTTP %d", tab, res.StatusCode)
		}
		res.Body.Close()
	}
	res = ann.form("POST", eventPath+"/link", url.Values{"expires_in_days": {"30"}})
	if page := body(res); !strings.Contains(page, "/l/") || !strings.Contains(page, "shown only once") {
		t.Error("link not shown after turning it on")
	}
	// A bad edit keeps what was typed and says why.
	res = ann.form("POST", eventPath, url.Values{"name": {"Example Event"}, "starts": {"2026-10-04T09:00"},
		"ends": {"2026-10-03T09:00"}, "time_zone": {"Asia/Colombo"}, "visibility": {"private"}, "type": {"hackathon"}})
	if page := body(res); res.StatusCode != http.StatusBadRequest || !strings.Contains(page, "ends before it starts") || !strings.Contains(page, "2026-10-04T09:00") {
		t.Errorf("invalid edit: HTTP %d", res.StatusCode)
	}
	for _, p := range []string{"/o/" + org.Slug + "/events/new", "/o/" + org.Slug + "/activity", "/o/" + org.Slug + "/members"} {
		res := ann.get(p)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: HTTP %d", p, res.StatusCode)
		}
		res.Body.Close()
	}
}
