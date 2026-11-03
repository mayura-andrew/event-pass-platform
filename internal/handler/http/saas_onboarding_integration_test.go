package httphandler

// Integration tests for M6's setup checklist, help pages and health check.

import (
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

var progressRe = regexp.MustCompile(`(\d) of 6 done`)

func checklistProgress(t *testing.T, u *user, slug string) string {
	t.Helper()
	html := body(u.get("/o/" + slug))
	if !strings.Contains(html, "Set up your first event") {
		return "hidden"
	}
	m := progressRe.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("checklist without a progress count")
	}
	return m[1]
}

func TestSetupChecklist(t *testing.T) {
	// A brand-new org: nothing done, the first step links to New event.
	env := newSaaSEnv(t)
	fresh := env.newBrowser(t, "fresh")
	fresh.signIn("")
	org := fresh.createOrg("fresh")
	html := body(fresh.get("/o/" + org.Slug))
	if !strings.Contains(html, "0 of 6 done · next: Create an event") || !strings.Contains(html, `href="/help/getting-started"`) {
		t.Fatalf("new org checklist missing or wrong")
	}

	// Hide it; it comes back from Members.
	res := fresh.form("POST", "/o/"+org.Slug+"/checklist/hide", url.Values{})
	if got := body(res); strings.Contains(got, "Set up your first event") || !strings.Contains(got, "Setup checklist hidden") {
		t.Error("hide didn't hide the checklist")
	}
	if !strings.Contains(body(fresh.get("/o/"+org.Slug+"/members")), "Show the checklist") {
		t.Error("members page doesn't offer to show the checklist again")
	}
	fresh.form("POST", "/o/"+org.Slug+"/checklist/show", url.Values{})
	if checklistProgress(t, fresh, org.Slug) != "0" {
		t.Error("show didn't bring the checklist back")
	}

	// An org part-way through: event, questions and check-in staff exist.
	f := newCheckinFixture(t, 1)
	slug := f.org.Slug
	if got := checklistProgress(t, f.owner, slug); got != "3" {
		t.Fatalf("progress %s of 6, want 3", got)
	}
	// Check-in staff (guests of the org) never see it.
	if got := checklistProgress(t, f.phones[1], slug); got != "hidden" {
		t.Errorf("check-in staff see the checklist (%s)", got)
	}
	steps := []func(){
		func() {
			f.owner.api("POST", "/api/v1/orgs/"+slug+"/pass-templates", map[string]string{"from": "minimal"})
		},
		func() { f.owner.api("POST", f.api+"/publish", nil) },
	}
	for i, step := range steps {
		step()
		if got := checklistProgress(t, f.owner, slug); got != string(rune('4'+i)) {
			t.Fatalf("after step %d: progress %s", i+4, got)
		}
	}
	// The last step, a scan, completes it and the checklist goes away.
	cp, ok := f.cps["Check in"]
	if !ok {
		t.Fatal("no Check in checkpoint")
	}
	if r := f.scan(t, f.owner, f.tokens[f.people[0].ID], cp.ID, ""); r.Result != domain.ScanRecorded {
		t.Fatalf("test scan: %s %s", r.Result, r.Message)
	}
	if got := checklistProgress(t, f.owner, slug); got != "hidden" {
		t.Errorf("finished checklist still shown (%s of 6)", got)
	}
}

var hrefRe = regexp.MustCompile(`href="(/[^"#?]*)`)

func TestHelpPages(t *testing.T) {
	env := newSaaSEnv(t)
	anon := env.newBrowser(t, "anon")
	index := body(anon.get("/help"))
	for _, topic := range helpTopics {
		if !strings.Contains(index, `href="/help/`+topic.Slug+`"`) {
			t.Errorf("help index doesn't list %s", topic.Slug)
		}
	}
	// Every page renders, and every help link on it leads somewhere.
	links := map[string]bool{}
	for _, topic := range helpTopics {
		res := anon.get("/help/" + topic.Slug)
		html := body(res)
		if res.StatusCode != http.StatusOK || !strings.Contains(html, "<h1>"+topic.Title+"</h1>") {
			t.Errorf("/help/%s: HTTP %d", topic.Slug, res.StatusCode)
		}
		for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
			if strings.HasPrefix(m[1], "/help") {
				links[m[1]] = true
			}
		}
	}
	for l := range links {
		if res := anon.get(l); res.StatusCode != http.StatusOK {
			t.Errorf("broken help link %s: HTTP %d", l, res.StatusCode)
		}
	}
	if res := anon.get("/help/no-such-topic"); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown topic: HTTP %d", res.StatusCode)
	}
	// Signed in, the usual header shows.
	u := env.newBrowser(t, "reader")
	u.signIn("")
	if !strings.Contains(body(u.get("/help/scanner")), u.email) {
		t.Error("help page doesn't show the signed-in header")
	}
}

func TestHealthCheck(t *testing.T) {
	env := newSaaSEnv(t)
	anon := env.newBrowser(t, "anon")
	h := service.NewHealth(postgres.NewHealthRepo(env.db), true)
	h.EmailLate = 1000 * time.Hour // other tests leave emails queued (no worker runs)
	env.h.Health = h
	status, b := anon.api("GET", "/api/v1/health", nil)
	if status != http.StatusOK || !strings.Contains(string(b), `"status":"ok"`) || !strings.Contains(string(b), `"database":"ok"`) {
		t.Errorf("health: HTTP %d %s", status, b)
	}
}
