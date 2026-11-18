package httphandler

// Integration tests for the security review's credential fixes: pass
// reissue (F4), sessions, idle timeout and step-up (F7), sign-in links that
// keep the token out of the URL (F7). Skipped unless
// EVENT_PASS_TEST_DATABASE_URL is set (see saas_integration_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/mayura-andrew/event-pass-platform/internal/passes"
	"github.com/mayura-andrew/event-pass-platform/internal/repository/postgres"
	"github.com/mayura-andrew/event-pass-platform/internal/service"
)

func TestPassReissueRetiresOldQR(t *testing.T) {
	f := newCheckinFixture(t, 1)
	ctx := context.Background()
	p := f.people[0]
	old := f.tokens[p.ID]
	check := func(tok string) service.PassCheck {
		t.Helper()
		var res service.PassCheck
		status, b := f.owner.api("POST", f.api+"/passes/check", map[string]string{"payload": tok})
		if status != http.StatusOK {
			t.Fatalf("check: HTTP %d %s", status, b)
		}
		decode(t, b, &res)
		return res
	}
	if r := check(old); r.Result != service.CheckValid {
		t.Fatalf("before reissue: %+v", r)
	}
	if status, b := f.owner.api("POST", f.api+"/participants/"+p.ID+"/reissue", nil); status != http.StatusNoContent {
		t.Fatalf("reissue: HTTP %d %s", status, b)
	}
	if r := check(old); r.Result != service.CheckInvalid || !strings.Contains(r.Message, "replaced") {
		t.Errorf("old QR after reissue: %+v, want replaced", r)
	}
	pc, err := postgres.NewOutboxRepo(f.env.db).PassContext(ctx, f.org.ID, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, _ := service.NewPassService(passes.DerivedKeyRing([]byte("test-pass-key"))).Token(pc)
	if r := check(fresh); r.Result != service.CheckValid {
		t.Errorf("new QR: %+v", r)
	}
}

func TestSessionsRevokeIdleAndStepUp(t *testing.T) {
	env := newSaaSEnv(t)
	ctx := context.Background()
	laptop := env.newBrowser(t, "sam")
	laptop.signIn("")
	phone := &user{t: t, env: env, c: newJarClient(), email: laptop.email}
	phone.signIn("")

	var rows []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
	}
	_, b := laptop.api("GET", "/api/v1/me/sessions", nil)
	decode(t, b, &rows)
	if len(rows) != 2 {
		t.Fatalf("sessions: %s, want 2", b)
	}
	if status, _ := laptop.api("POST", "/api/v1/me/sessions/revoke-others", nil); status != http.StatusOK {
		t.Fatalf("revoke others: HTTP %d", status)
	}
	if status, _ := phone.api("GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Errorf("phone after revoke-others: HTTP %d, want 401", status)
	}
	if status, _ := laptop.api("GET", "/api/v1/me", nil); status != http.StatusOK {
		t.Errorf("laptop after revoke-others: HTTP %d, want 200", status)
	}

	// Step-up: an hour-old sign-in can't transfer an event.
	org := laptop.createOrg("stepup")
	status, b := laptop.api("POST", "/api/v1/orgs/"+org.Slug+"/events", map[string]any{"name": "Step-up test"})
	if status != http.StatusCreated {
		t.Fatalf("create event: HTTP %d %s", status, b)
	}
	var ev struct{ ID string }
	decode(t, b, &ev)
	env.db.ExecContext(ctx, `UPDATE sessions SET created_at = NOW() - INTERVAL '1 hour' WHERE account_id = $1`, laptop.id)
	status, b = laptop.api("POST", "/api/v1/orgs/"+org.Slug+"/events/"+ev.ID+"/transfer", map[string]string{"account_id": laptop.id})
	if status != http.StatusForbidden || !strings.Contains(string(b), "REAUTH") {
		t.Errorf("stale transfer: HTTP %d %s, want 403 REAUTH", status, b)
	}

	// Idle timeout: unused for longer than it, the session ends.
	env.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = NOW() - INTERVAL '2 hours' WHERE account_id = $1`, laptop.id)
	if status, _ := laptop.api("GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Errorf("idle session: HTTP %d, want 401", status)
	}
}

func TestSignInLinkKeepsTokenOutOfURL(t *testing.T) {
	env := newSaaSEnv(t)
	u := env.newBrowser(t, "frag")
	res := u.form("POST", "/login", map[string][]string{"email": {u.email}})
	res.Body.Close()
	env.mail.mu.Lock()
	text := env.mail.sent[len(env.mail.sent)-1].Text
	env.mail.mu.Unlock()
	if !strings.Contains(text, "/auth/email/verify#token=") || strings.Contains(text, "verify?token=") {
		t.Fatalf("sign-in link should carry the token in the fragment: %s", text)
	}
	// The page itself needs no token in the request (the fragment never
	// reaches the server) and posts it from the form.
	res = u.get("/auth/email/verify")
	if page := body(res); res.StatusCode != http.StatusOK || !strings.Contains(page, "verify.js") {
		t.Errorf("verify page: HTTP %d", res.StatusCode)
	}
}

// newJarClient is a second browser (its own cookies) for the same person.
func newJarClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func decode(t *testing.T, b []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}
