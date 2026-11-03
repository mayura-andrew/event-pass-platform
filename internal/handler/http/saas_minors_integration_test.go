package httphandler

// Birth dates and the under-18 check: a participant is a minor when they
// haven't turned 18 by the event's "age is counted on" date (14 November in
// the Colombo event), whatever today's date is.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mayura-andrew/event-pass-platform/internal/domain"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

func TestUnder18OnCutoffDate(t *testing.T) {
	f := newCheckinFixture(t, 1) // one registration from before the question existed
	owner, anon := f.owner, f.env.newBrowser(t, "anon")

	// The organizer asks for a birth date and counts ages on 14 Nov 2026.
	status, b := owner.api("POST", f.api+"/fields", map[string]any{"label": "Date of birth", "type": "birth_date", "visibility": "admin"})
	var field domain.FormField
	json.Unmarshal(b, &field)
	if status != http.StatusCreated || field.Key != "date_of_birth" {
		t.Fatalf("add birth date question: HTTP %d %s", status, b)
	}
	if status, b := owner.api("POST", f.api+"/fields", map[string]any{"label": "Birthday", "type": "birth_date"}); status != http.StatusConflict {
		t.Errorf("second birth date question: HTTP %d %s, want 409", status, b)
	}
	if status, b := owner.api("PATCH", f.api+"/registration", map[string]any{"reg_mode": "open", "pass_prefix": "SAC",
		"age_on": "2026-11-14T00:00:00Z"}); status != http.StatusNoContent {
		t.Fatalf("set age_on: HTTP %d %s", status, b)
	}
	owner.api("POST", f.api+"/publish", nil)

	// People register on the public form.
	page := "/e/" + f.org.Slug + "/" + f.ev.Slug
	register := func(name, dob string) *http.Response {
		email := strings.ToLower(strings.ReplaceAll(name, " ", ".")) + "@" + strings.TrimSuffix(f.env.prefix, "-") + ".test.local"
		return anon.form("POST", page+"/register", url.Values{"full_name": {name}, "email": {email}, "q.date_of_birth": {dob}})
	}
	for name, dob := range map[string]string{
		"Exactly Eighteen": "2008-11-14", // 18th birthday on the cut-off: an adult
		"One Day Short":    "2008-11-15", // turns 18 the day after: a minor
		"Young Coder":      "2011-03-02",
		"Leap Day":         "2008-02-29",
	} {
		if res := register(name, dob); res.StatusCode != http.StatusOK || strings.Contains(body(res), "Enter your date of birth") {
			t.Fatalf("register %s: HTTP %d", name, res.StatusCode)
		}
	}
	for _, dob := range []string{"2999-01-01", "1899-12-31", "14/11/2008"} {
		if html := body(register("Bad Date", dob)); !strings.Contains(html, "Enter your date of birth") {
			t.Errorf("birth date %q accepted", dob)
		}
	}

	minors := func() []domain.EventParticipant {
		t.Helper()
		_, b := owner.api("GET", f.api+"/participants?status=minors", nil)
		var res struct {
			Participants []domain.EventParticipant `json:"participants"`
			Counts       domain.ParticipantCounts  `json:"counts"`
		}
		json.Unmarshal(b, &res)
		if res.Counts[domain.CountMinors] != len(res.Participants) {
			t.Errorf("minors count %d, list %d", res.Counts[domain.CountMinors], len(res.Participants))
		}
		return res.Participants
	}
	names := func(ps []domain.EventParticipant) string {
		var out []string
		for _, p := range ps {
			out = append(out, p.FullName)
		}
		return strings.Join(out, ", ")
	}
	got := minors()
	if len(got) != 2 || !strings.Contains(names(got), "One Day Short") || !strings.Contains(names(got), "Young Coder") {
		t.Fatalf("minors on 14 Nov 2026: %s; want One Day Short and Young Coder", names(got))
	}
	for _, p := range got {
		if p.FullName == "Young Coder" && (p.Age == nil || *p.Age != 15) {
			t.Errorf("Young Coder's age on 14 Nov 2026: %v, want 15", p.Age)
		}
	}

	// Door staff see the flag; the dashboard counts it.
	_, b = f.phones[1].api("GET", f.api+"/lookup?q=One+Day", nil)
	var found []service.ScanParticipant
	json.Unmarshal(b, &found)
	if len(found) != 1 || !found[0].Minor {
		t.Errorf("scanner lookup: %s", b)
	}
	_, b = owner.api("GET", f.api+"/stats", nil)
	var st domain.EventStats
	json.Unmarshal(b, &st)
	if !st.AsksBirthDate || st.Minors != 2 {
		t.Errorf("dashboard: asks %v, minors %d", st.AsksBirthDate, st.Minors)
	}
	html := body(owner.get("/o/" + f.org.Slug + "/events/" + f.ev.ID + "/participants?status=minors"))
	if !strings.Contains(html, "Under 18 on 14 Nov 2026") && !strings.Contains(html, "under 18 on 14 Nov 2026") {
		t.Error("participants page doesn't say which day ages count on")
	}

	// A later cut-off: One Day Short has turned 18 by 16 November.
	owner.api("PATCH", f.api+"/registration", map[string]any{"reg_mode": "open", "pass_prefix": "SAC", "age_on": "2026-11-16T00:00:00Z"})
	if got := minors(); len(got) != 1 || got[0].FullName != "Young Coder" {
		t.Errorf("minors on 16 Nov: %s", names(got))
	}

	// Removing the question removes the flags; adding it back restores them
	// from the answers people gave.
	owner.api("DELETE", f.api+"/fields/"+field.ID, nil)
	if got := minors(); len(got) != 0 {
		t.Errorf("minors without a birth date question: %s", names(got))
	}
	owner.api("POST", f.api+"/fields", map[string]any{"label": "Date of birth", "type": "birth_date", "visibility": "admin"})
	if got := minors(); len(got) != 1 {
		t.Errorf("minors after re-adding the question: %s", names(got))
	}
}
