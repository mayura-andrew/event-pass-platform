package httphandler

// Integration tests for M5's Google Sheets sync and Google Form import,
// against a fake Google (consent, token, Sheets and Forms endpoints). The
// gate: a connected spreadsheet stays up to date within about a minute, and
// importing a form never creates a participant twice.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
	"github.com/nasa-spaceapps/nsac-qr/internal/secrets"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

const testFormID = "1FAIpQLSf-test-form-0123456789abc"

// fakeGoogle plays accounts.google.com, oauth2.googleapis.com, Sheets and Forms.
type fakeGoogle struct {
	t         *testing.T
	srv       *httptest.Server
	app       string // the app under test, for the consent redirect
	mu        sync.Mutex
	revoked   bool
	sheets    map[string]map[string][][]any // spreadsheet ID -> tab -> rows
	writes    int
	responses []map[string]any
	codes     map[string]string // code -> refresh token
}

func newFakeGoogle(t *testing.T, app string) *fakeGoogle {
	g := &fakeGoogle{t: t, app: app, sheets: map[string]map[string][][]any{}, codes: map[string]string{}}
	mux := http.NewServeMux()
	// Consent: grant at once and send the browser back with a code.
	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("access_type") != "offline" || q.Get("code_challenge") == "" {
			t.Errorf("consent request without offline access or PKCE: %s", r.URL)
		}
		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		g.mu.Lock()
		g.codes[code] = "refresh-" + strings.ReplaceAll(q.Get("scope"), " ", "+")
		g.mu.Unlock()
		http.Redirect(w, r, g.app+"/auth/google/integration/callback?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		g.mu.Lock()
		defer g.mu.Unlock()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			refresh, ok := g.codes[r.Form.Get("code")]
			if !ok || r.Form.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			claims, _ := json.Marshal(map[string]any{"email": "Organizer@Example.com"})
			json.NewEncoder(w).Encode(map[string]string{"access_token": "access-1", "refresh_token": refresh,
				"id_token": "x." + base64.RawURLEncoding.EncodeToString(claims) + ".y"})
		case "refresh_token":
			if g.revoked || !strings.HasPrefix(r.Form.Get("refresh_token"), "refresh-") {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "access-2"})
		}
	})
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux.HandleFunc("POST /v4/spreadsheets", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		g.mu.Lock()
		id := fmt.Sprintf("sheet-%d", len(g.sheets)+1)
		g.sheets[id] = map[string][][]any{}
		g.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"spreadsheetId": id, "spreadsheetUrl": "https://docs.google.com/spreadsheets/d/" + id})
	})
	mux.HandleFunc("POST /v4/spreadsheets/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		id, op, _ := strings.Cut(r.PathValue("rest"), "/values:")
		var body struct {
			Ranges []string `json:"ranges"`
			Data   []struct {
				Range  string  `json:"range"`
				Values [][]any `json:"values"`
			} `json:"data"`
			ValueInputOption string `json:"valueInputOption"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		defer g.mu.Unlock()
		tabs, ok := g.sheets[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch op {
		case "batchClear":
			for _, rg := range body.Ranges {
				delete(tabs, strings.Trim(rg, "'"))
			}
		case "batchUpdate":
			if body.ValueInputOption != "RAW" {
				t.Errorf("values written as %q, want RAW (no formulas from participant data)", body.ValueInputOption)
			}
			for _, d := range body.Data {
				tab, _, _ := strings.Cut(d.Range, "!")
				tabs[strings.Trim(tab, "'")] = d.Values
			}
			g.writes++
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET /v1/forms/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.PathValue("id") != testFormID {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Requested entity was not found."}})
			return
		}
		item := func(id, title string) map[string]any {
			return map[string]any{"title": title, "questionItem": map[string]any{"question": map[string]string{"questionId": id}}}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"info":     map[string]string{"title": "Colombo sign-up"},
			"settings": map[string]string{"emailCollectionType": "DO_NOT_COLLECT"},
			"items":    []any{item("q1", "Your full name"), item("q2", "Email address"), item("q3", "Dietary needs"), map[string]string{"title": "A section header"}},
		})
	})
	mux.HandleFunc("GET /v1/forms/{id}/responses", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		// Two per page, to exercise paging.
		start := 0
		fmt.Sscan(r.URL.Query().Get("pageToken"), &start)
		end := min(start+2, len(g.responses))
		out := map[string]any{"responses": g.responses[start:end]}
		if end < len(g.responses) {
			out["nextPageToken"] = fmt.Sprint(end)
		}
		json.NewEncoder(w).Encode(out)
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGoogle) addResponse(id, name, email, food string) {
	text := func(v string) map[string]any {
		return map[string]any{"textAnswers": map[string]any{"answers": []map[string]string{{"value": v}}}}
	}
	answers := map[string]any{"q1": text(name), "q2": text(email)}
	if food != "" {
		answers["q3"] = text(food)
	}
	g.mu.Lock()
	g.responses = append(g.responses, map[string]any{"responseId": id, "answers": answers,
		"lastSubmittedTime": time.Now().UTC().Format(time.RFC3339Nano)})
	g.mu.Unlock()
}

func (g *fakeGoogle) tab(id, name string) [][]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sheets[id][name]
}

func TestIntegrationsGate(t *testing.T) {
	f := newCheckinFixture(t, 2)
	env, owner := f.env, f.owner
	ctx := context.Background()
	g := newFakeGoogle(t, env.srv.URL)

	oauth := service.NewGoogleOAuth("client-1", "secret", "")
	oauth.AuthURL, oauth.TokenURL = g.srv.URL+"/auth", g.srv.URL+"/token"
	api := service.NewGoogleAPI()
	api.SheetsBase, api.FormsBase = g.srv.URL, g.srv.URL
	repo := postgres.NewIntegrationRepo(env.db)
	svc := service.NewIntegrationService(repo, postgres.NewRegistrationRepo(env.db), oauth, api,
		secrets.Derived([]byte("test-integrations-key")), env.srv.URL+"/auth/google/integration/callback")
	if svc.Debounce+svc.Tick > 50*time.Second {
		t.Fatalf("default debounce %v + tick %v: a change could take over a minute to reach the sheet", svc.Debounce, svc.Tick)
	}
	svc.Debounce, svc.Interval = time.Second, time.Second
	env.h.Integrations = svc
	page := "/o/" + f.org.Slug + "/events/" + f.ev.ID + "/integrations"

	// Check-in staff can't see or change integrations.
	if res := f.phones[1].get(page); res.StatusCode != http.StatusForbidden {
		t.Errorf("staff integrations page: HTTP %d, want 403", res.StatusCode)
	}

	// ---- Sheets -----------------------------------------------------------
	res := owner.get(page + "/sheets/connect") // -> fake consent -> callback -> back to the tab
	if res.StatusCode != http.StatusOK || res.Request.URL.Path != page || !strings.Contains(body(res), "Open the spreadsheet") {
		t.Fatalf("connect sheets: HTTP %d at %s", res.StatusCode, res.Request.URL.Path)
	}
	var secret []byte
	var email string
	var cfg domain.SheetsConfig
	var rawCfg []byte
	if err := env.db.QueryRowContext(ctx, `SELECT secret, google_email, config FROM integrations WHERE event_id = $1 AND kind = 'google_sheets'`,
		f.ev.ID).Scan(&secret, &email, &rawCfg); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(rawCfg, &cfg)
	if bytes.Contains(secret, []byte("refresh-")) || len(secret) < 30 {
		t.Errorf("the refresh token is stored readable: %q", secret)
	}
	if email != "organizer@example.com" || cfg.SpreadsheetID == "" {
		t.Errorf("connected as %q, spreadsheet %q", email, cfg.SpreadsheetID)
	}
	// The API never returns the secret.
	_, b := owner.api("GET", f.api+"/integrations", nil)
	if strings.Contains(string(b), "secret") || !strings.Contains(string(b), cfg.SpreadsheetID) {
		t.Errorf("integrations API: %s", b)
	}

	run := func() int {
		t.Helper()
		n, err := svc.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := run(); n != 1 {
		t.Fatalf("first sync ran %d integrations, want 1", n)
	}
	people := g.tab(cfg.SpreadsheetID, "Participants")
	if len(people) != 3 || people[0][1] != "Full name" || people[0][6] != "Food" || people[1][6] != "Vegan" {
		t.Fatalf("Participants tab: %v", people)
	}
	if run() != 0 {
		t.Error("synced again with nothing changed")
	}

	// A check-in reaches the sheet on the next run after the debounce.
	var cp string
	for _, c := range f.cps {
		cp = c.ID
		break
	}
	f.scan(t, owner, f.tokens[f.people[0].ID], cp, "")
	time.Sleep(1100 * time.Millisecond)
	if run() != 1 {
		t.Fatal("a check-in didn't make the sheet due")
	}
	if ci := g.tab(cfg.SpreadsheetID, "Check-ins"); len(ci) != 2 || ci[1][2] != f.people[0].FullName {
		t.Errorf("Check-ins tab: %v", ci)
	}

	// ---- Form import --------------------------------------------------------
	res = owner.get(page + "/form/connect")
	if !strings.Contains(body(res), "Link form") {
		t.Fatalf("connect form: HTTP %d at %s", res.StatusCode, res.Request.URL.Path)
	}
	res = owner.form("POST", page+"/form/link", url.Values{"form": {"https://docs.google.com/forms/d/e/1FAIpQLSdpublished/viewform"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body(res), "public link") {
		t.Errorf("published link: HTTP %d, want 422 explaining which link", res.StatusCode)
	}
	res = owner.form("POST", page+"/form/link", url.Values{"form": {"https://docs.google.com/forms/d/" + testFormID + "/edit"}})
	html := body(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(html, "Dietary needs") || !strings.Contains(html, `value="q1" selected`) {
		t.Fatalf("link form: HTTP %d %s", res.StatusCode, html)
	}

	g.addResponse("r1", "Nimal Perera", "nimal@"+env.prefix+"test.local", "Vegetarian")
	g.addResponse("r2", "Already Here", f.people[1].Email, "")                 // registered already: skipped
	g.addResponse("r3", "No Email", "not an email", "")                        // unusable: reported
	g.addResponse("r4", "කසුන් ජයසූරිය", "kasun@"+env.prefix+"test.local", "") // non-Latin names import too
	res = owner.form("POST", page+"/form/map", url.Values{"name": {"q1"}, "email": {"q2"}, "q.q3": {"food"}})
	if res.Request.URL.Query().Get("notice") != "mapped" {
		t.Fatalf("map form: HTTP %d at %s", res.StatusCode, res.Request.URL)
	}
	run()
	count := func() (n int) {
		env.db.QueryRowContext(ctx, `SELECT count(*) FROM event_participants WHERE event_id = $1`, f.ev.ID).Scan(&n)
		return
	}
	if n := count(); n != 4 {
		t.Fatalf("after import: %d participants, want 4 (2 + Nimal + Kasun)", n)
	}
	var food, source, status string
	env.db.QueryRowContext(ctx, `SELECT answers->>'food', source, status FROM event_participants WHERE event_id = $1 AND external_ref = 'r1'`,
		f.ev.ID).Scan(&food, &source, &status)
	if food != "Vegetarian" || source != "import" || status != domain.PartRegistered {
		t.Errorf("imported r1: food %q source %q status %q", food, source, status)
	}
	var queued int
	env.db.QueryRowContext(ctx, `SELECT count(*) FROM outbox o JOIN event_participants p ON p.id = o.participant_id
		WHERE p.event_id = $1 AND p.external_ref IS NOT NULL`, f.ev.ID).Scan(&queued)
	if queued != 2 {
		t.Errorf("%d pass emails queued for imported people, want 2", queued)
	}
	in, _ := repo.Get(ctx, f.es, domain.IntegrationForm)
	if in.Imported != 2 || !strings.Contains(in.LastError, "1 response has") {
		t.Errorf("form integration: imported %d, last error %q", in.Imported, in.LastError)
	}

	// Re-running, even with a new response, never imports anyone twice.
	time.Sleep(1100 * time.Millisecond)
	g.addResponse("r5", "Late Comer", "late@"+env.prefix+"test.local", "Halal")
	run()
	time.Sleep(1100 * time.Millisecond)
	run()
	if n := count(); n != 5 {
		t.Errorf("after re-runs: %d participants, want 5", n)
	}
	var dupes int
	env.db.QueryRowContext(ctx, `SELECT count(*) - count(DISTINCT email) FROM event_participants WHERE event_id = $1`, f.ev.ID).Scan(&dupes)
	if dupes != 0 {
		t.Errorf("%d duplicate participants", dupes)
	}

	// The imports reached the sheet too.
	time.Sleep(1100 * time.Millisecond)
	run()
	if people := g.tab(cfg.SpreadsheetID, "Participants"); len(people) != 6 {
		t.Errorf("Participants tab has %d rows after imports, want 6", len(people))
	}

	// ---- Revoked access -----------------------------------------------------
	g.mu.Lock()
	g.revoked = true
	g.mu.Unlock()
	owner.form("POST", page+"/sheets/sync", url.Values{})
	run()
	in, _ = repo.Get(ctx, f.es, domain.IntegrationSheets)
	if in.Status != "error" {
		t.Errorf("after revocation: status %q, want error", in.Status)
	}
	if html := body(owner.get(page)); !strings.Contains(html, "Needs reconnecting") || !strings.Contains(html, "connect again") {
		t.Error("the page doesn't ask to reconnect")
	}

	// Disconnecting removes the connection (and its token).
	owner.form("POST", page+"/sheets/disconnect", url.Values{})
	var left int
	env.db.QueryRowContext(ctx, `SELECT count(*) FROM integrations WHERE event_id = $1 AND kind = 'google_sheets'`, f.ev.ID).Scan(&left)
	if left != 0 {
		t.Error("disconnect left the integration")
	}
}

func TestIntegrationStateIsChecked(t *testing.T) {
	env := newSaaSEnv(t)
	u := env.newBrowser(t, "owner")
	u.signIn("")
	res := u.get("/auth/google/integration/callback?code=x&state=forged")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("forged callback: HTTP %d, want 400", res.StatusCode)
	}
}
