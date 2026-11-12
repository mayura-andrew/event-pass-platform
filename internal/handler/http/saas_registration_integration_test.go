package httphandler

// Integration tests for M3 registration: publishing, the public form,
// capacity and waitlist, approval, and the organizer's list. Same setup as
// saas_integration_test.go.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

var passIDRe = regexp.MustCompile(`\b(SAC-[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4})\b`)

func TestPageTemplatesParse(t *testing.T) {
	if _, err := loadPages(); err != nil {
		t.Fatal(err)
	}
}

func TestParticipantCSVImportIssuesAndEmailsPasses(t *testing.T) {
	env := newSaaSEnv(t)
	owner := env.newBrowser(t, "import-owner")
	owner.signIn("")
	org := owner.createOrg("csv-import")
	base := "/api/v1/orgs/" + org.Slug + "/events"
	status, b := owner.api("POST", base, map[string]any{
		"name": "Example Event", "starts_at": time.Now().Add(14 * 24 * time.Hour),
		"capacity": 2, "visibility": "unlisted", "time_zone": "Asia/Colombo",
	})
	if status != http.StatusCreated {
		t.Fatalf("create event: HTTP %d %s", status, b)
	}
	var ev domain.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	if status, b = owner.api("PATCH", base+"/"+ev.ID+"/registration", map[string]any{"reg_mode": "approval"}); status != http.StatusNoContent {
		t.Fatalf("set approval mode: HTTP %d %s", status, b)
	}

	csv := []byte("Email,Title,Name\n" +
		env.prefix + "csv-one@test.local,Engineer,Ann Perera\n" +
		env.prefix + "csv-two@test.local,Organizer,Ben Silva\n")
	page := "/o/" + org.Slug + "/events/" + ev.ID + "/participants/import"
	res := uploadAs(t, owner, page, "csv", nil, csv)
	html := body(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(html, "Imported 2 participants") {
		t.Fatalf("import CSV: HTTP %d at %s: %s", res.StatusCode, res.Request.URL.Path, html)
	}

	_, b = owner.api("GET", base+"/"+ev.ID+"/participants", nil)
	var list struct{ Participants []domain.EventParticipant }
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Participants) != 2 {
		t.Fatalf("participant count %d, want 2", len(list.Participants))
	}
	var imported domain.EventParticipant
	for _, p := range list.Participants {
		if p.Status != domain.PartRegistered || p.Source != "import" {
			t.Errorf("imported participant status/source: %s/%s", p.Status, p.Source)
		}
		if p.FullName == "Ann Perera" {
			imported = p
		}
	}
	if imported.ID == "" || imported.Answers["title"] != "Engineer" {
		t.Fatalf("name/title not stored from CSV: %+v", imported)
	}

	pc, err := postgres.NewOutboxRepo(env.db).PassContext(context.Background(), org.ID, imported.ID)
	if err != nil {
		t.Fatal(err)
	}
	card, err := env.h.Passes.Card(pc)
	if err != nil {
		t.Fatal(err)
	}
	if len(card.Lines) != 1 || card.Lines[0].Label != "Title" || card.Lines[0].Value != "Engineer" {
		t.Errorf("pass title: %+v", card.Lines)
	}

	worker := service.NewOutboxWorker(postgres.NewOutboxRepo(env.db), env.mail, env.h.Passes, env.h.BaseURL, env.h.Product)
	if sent, err := worker.RunOnce(context.Background()); err != nil || sent != 2 {
		t.Fatalf("send imported pass emails: sent=%d err=%v", sent, err)
	}
	env.mail.mu.Lock()
	emailCount := 0
	for _, message := range env.mail.sent {
		if strings.HasPrefix(message.To.Address, env.prefix+"csv-") {
			emailCount++
			if len(message.Attachments) != 1 {
				t.Errorf("pass email to %s has %d attachments", message.To.Address, len(message.Attachments))
			}
		}
	}
	env.mail.mu.Unlock()
	if emailCount != 2 {
		t.Errorf("imported pass emails sent %d, want 2", emailCount)
	}

	for _, invalidCSV := range [][]byte{
		[]byte("Name,Email,Title\nDuplicate," + env.prefix + "csv-one@test.local,Engineer\n"),
		[]byte("Name,Email,Title\nThird Person," + env.prefix + "csv-three@test.local,Guest\n"),
	} {
		res := uploadAs(t, owner, page, "csv", nil, invalidCSV)
		html := body(res)
		if res.StatusCode != http.StatusConflict || !strings.Contains(html, "already") && !strings.Contains(html, "capacity") {
			t.Errorf("expected a safe duplicate/capacity conflict, got HTTP %d: %s", res.StatusCode, html)
		}
	}
	_, b = owner.api("GET", base+"/"+ev.ID+"/participants", nil)
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Participants) != 2 {
		t.Errorf("failed imports changed participant count to %d", len(list.Participants))
	}
}

func TestRegistrationJourney(t *testing.T) {
	env := newSaaSEnv(t)
	ann := env.newBrowser(t, "ann")
	ann.signIn("")
	org := ann.createOrg("reg")
	base := "/api/v1/orgs/" + org.Slug + "/events"

	starts := time.Now().Add(14 * 24 * time.Hour).UTC().Format(time.RFC3339)
	status, b := ann.api("POST", base, map[string]any{"name": "Example Event", "starts_at": starts,
		"capacity": 2, "visibility": "unlisted", "time_zone": "Asia/Colombo"})
	if status != http.StatusCreated {
		t.Fatalf("create: HTTP %d %s", status, b)
	}
	var ev domain.Event
	json.Unmarshal(b, &ev)
	if ev.PassPrefix != "SAC" || ev.RegMode != "open" || !ev.Waitlist {
		t.Fatalf("defaults: prefix %q, mode %q, waitlist %v", ev.PassPrefix, ev.RegMode, ev.Waitlist)
	}
	evAPI := base + "/" + ev.ID
	for _, f := range []map[string]any{
		{"label": "T-shirt size", "type": "select", "options": []string{"S", "M", "L"}, "required": true, "visibility": "staff"},
		{"label": "I agree to the code of conduct", "type": "consent", "required": true},
		{"label": "Laptops", "type": "number"},
	} {
		if status, b := ann.api("POST", evAPI+"/fields", f); status != http.StatusCreated {
			t.Fatalf("add field %v: HTTP %d %s", f["label"], status, b)
		}
	}

	anon := env.newBrowser(t, "anon")
	page := "/e/" + org.Slug + "/" + ev.Slug
	if res := anon.get(page); res.StatusCode != http.StatusNotFound {
		t.Fatalf("draft event page: HTTP %d, want 404", res.StatusCode)
	}
	if status, b := ann.api("POST", evAPI+"/publish", nil); status != http.StatusNoContent {
		t.Fatalf("publish: HTTP %d %s", status, b)
	}
	res := anon.get(page)
	html := body(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(html, "T-shirt size") || !strings.Contains(html, "code of conduct") {
		t.Fatalf("public page: HTTP %d", res.StatusCode)
	}

	register := func(name, email string, extra url.Values) (int, string) {
		t.Helper()
		v := url.Values{"full_name": {name}, "email": {email}}
		for k, x := range extra {
			v[k] = x
		}
		res := anon.form("POST", page+"/register", v)
		return res.StatusCode, body(res)
	}
	good := url.Values{"q.t_shirt_size": {"M"}, "q.i_agree_to_the_code_of_conduct": {"yes"}, "q.laptops": {"2"}}

	// Missing answers: errors per question, and what was typed stays.
	status, html = register("Ann Perera", "not-an-email", url.Values{"q.laptops": {"two"}})
	for _, want := range []string{"Enter a valid email address", "Please answer this question", "Please tick this box", "Enter a number", `value="Ann Perera"`} {
		if !strings.Contains(html, want) {
			t.Errorf("invalid form: page lacks %q", want)
		}
	}
	if status != http.StatusBadRequest {
		t.Errorf("invalid form: HTTP %d, want 400", status)
	}

	status, html = register("Ben Silva", env.prefix+"ben@test.local", good)
	m := passIDRe.FindStringSubmatch(html)
	if status != http.StatusOK || !strings.Contains(html, "You're registered") || m == nil {
		t.Fatalf("register: HTTP %d, pass ID found: %v", status, m != nil)
	}
	if _, html = register("Ben Silva", strings.ToUpper(env.prefix+"ben@test.local"), good); !strings.Contains(html, "already registered") || !strings.Contains(html, m[1]) {
		t.Error("same email again: not told they're already registered with the same pass ID")
	}
	register("Chamari Fernando", env.prefix+"chamari@test.local", good)
	if _, html = register("Dilan Perera", env.prefix+"dilan@test.local", good); !strings.Contains(html, "waitlist") {
		t.Error("third registration with capacity 2: not waitlisted")
	}

	// The organizer's list: counts, typed answers.
	status, b = ann.api("GET", evAPI+"/participants", nil)
	var list struct {
		Participants []domain.EventParticipant
		Counts       map[string]int
	}
	json.Unmarshal(b, &list)
	if status != http.StatusOK || list.Counts["registered"] != 2 || list.Counts["waitlisted"] != 1 {
		t.Fatalf("participants: HTTP %d counts %v", status, list.Counts)
	}
	var ben, dilan domain.EventParticipant
	for _, p := range list.Participants {
		switch p.FullName {
		case "Ben Silva":
			ben = p
		case "Dilan Perera":
			dilan = p
		}
	}
	if ben.Answers["t_shirt_size"] != "M" || ben.Answers["i_agree_to_the_code_of_conduct"] != true || ben.Answers["laptops"] != 2.0 {
		t.Errorf("stored answers: %v", ben.Answers)
	}

	// Cancel Ben, give Dilan his place.
	ann.api("PATCH", evAPI+"/participants/"+ben.ID, map[string]string{"status": "cancelled"})
	ann.api("PATCH", evAPI+"/participants/"+dilan.ID, map[string]string{"status": "registered"})
	_, b = ann.api("GET", evAPI+"/participants?status=registered", nil)
	json.Unmarshal(b, &list)
	if len(list.Participants) != 2 || list.Counts["cancelled"] != 1 {
		t.Errorf("after cancel and promote: %d registered, counts %v", len(list.Participants), list.Counts)
	}
	_, b = ann.api("GET", evAPI+"/participants?q=dilan", nil)
	json.Unmarshal(b, &list)
	if len(list.Participants) != 1 {
		t.Errorf("search by name: %d results", len(list.Participants))
	}

	// Approval mode: new registrations wait.
	if status, b := ann.api("PATCH", evAPI+"/registration", map[string]any{"reg_mode": "approval"}); status != http.StatusNoContent {
		t.Fatalf("approval mode: HTTP %d %s", status, b)
	}
	ann.api("PATCH", base+"/"+ev.ID, map[string]any{"clear_capacity": true})
	if _, html = register("Esha Raj", env.prefix+"esha@test.local", good); !strings.Contains(html, "review each registration") {
		t.Error("approval mode: not told to wait for approval")
	}

	// Closed registration.
	past := time.Now().Add(-time.Hour)
	ann.api("PATCH", evAPI+"/registration", map[string]any{"reg_closes_at": past})
	if html = body(anon.get(page)); !strings.Contains(html, "Registration has closed") {
		t.Error("closed registration still shows the form")
	}
	if status, _ = register("Late Comer", env.prefix+"late@test.local", good); status != http.StatusConflict {
		t.Errorf("register after closing: HTTP %d, want 409", status)
	}

	// The audit log saw it all.
	_, b = ann.api("GET", evAPI+"/activity", nil)
	for _, want := range []string{"event.publish", "form.field.add", "participant.status", "event.registration"} {
		if !strings.Contains(string(b), `"`+want+`"`) {
			t.Errorf("event activity lacks %s", want)
		}
	}

	// The organizer pages render.
	for _, p := range []string{"/registration", "/participants", "/participants?status=waitlisted&q=d"} {
		res := ann.get("/o/" + org.Slug + "/events/" + ev.ID + p)
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s: HTTP %d", p, res.StatusCode)
		}
		res.Body.Close()
	}
}

func TestPrivateEventsHaveNoPublicPage(t *testing.T) {
	env := newSaaSEnv(t)
	ann := env.newBrowser(t, "ann")
	ann.signIn("")
	org := ann.createOrg("priv")
	base := "/api/v1/orgs/" + org.Slug + "/events"
	_, b := ann.api("POST", base, map[string]any{"name": "Board Retreat", "starts_at": time.Now().Add(48 * time.Hour)})
	var ev domain.Event
	json.Unmarshal(b, &ev)
	if status, _ := ann.api("POST", base+"/"+ev.ID+"/publish", nil); status != http.StatusNoContent {
		t.Fatal("publish private event")
	}
	anon := env.newBrowser(t, "anon")
	if res := anon.get("/e/" + org.Slug + "/" + ev.Slug); res.StatusCode != http.StatusNotFound {
		t.Errorf("private published event page: HTTP %d, want 404", res.StatusCode)
	}
	res := anon.form("POST", "/e/"+org.Slug+"/"+ev.Slug+"/register", url.Values{"full_name": {"X Y"}, "email": {env.prefix + "x@test.local"}})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("register for a private event: HTTP %d, want 404", res.StatusCode)
	}
	// An event without dates can't be published.
	_, b = ann.api("POST", base, map[string]any{"name": "Someday"})
	json.Unmarshal(b, &ev)
	if status, _ := ann.api("POST", base+"/"+ev.ID+"/publish", nil); status != http.StatusBadRequest {
		t.Errorf("publish without dates: HTTP %d, want 400", status)
	}
}
