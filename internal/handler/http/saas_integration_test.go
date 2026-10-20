package httphandler

// End-to-end tests for the M1 SaaS routes against a real PostgreSQL with the
// migrations applied (go run ./cmd/migrate up). Skipped unless
// NSAC_TEST_DATABASE_URL is set. Accounts and orgs use it-<tag>-* emails and
// slugs and are deleted afterwards.
//
// TestTenantIsolation is the spec's isolation suite: every route in
// orgRoutes, called by a member of org A with org B's real IDs, must be 404.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
	"github.com/nasa-spaceapps/nsac-qr/internal/service"
)

// captureMailer keeps sent messages so tests can follow emailed links.
type captureMailer struct {
	mu   sync.Mutex
	sent []service.Message
}

func (m *captureMailer) Send(_ context.Context, msg service.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, msg)
	return nil
}

var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

// lastToken returns the token in the newest email to addr.
func (m *captureMailer) lastToken(t *testing.T, addr string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].To.Address == addr {
			if mm := tokenRe.FindStringSubmatch(m.sent[i].Text); mm != nil {
				return mm[1]
			}
		}
	}
	t.Fatalf("no emailed link for %s", addr)
	return ""
}

type saasEnv struct {
	srv    *httptest.Server
	mail   *captureMailer
	db     *sql.DB
	prefix string // it-<tag>- on every email and slug
}

func newSaaSEnv(t *testing.T) *saasEnv { return newSaaSEnvWith(t, nil) }

func newSaaSEnvWith(t *testing.T, google *service.GoogleOAuth) *saasEnv {
	t.Helper()
	dsn := os.Getenv("NSAC_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("NSAC_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	rand.Read(b)
	env := &saasEnv{mail: &captureMailer{}, db: db, prefix: "it-" + hex.EncodeToString(b) + "-"}

	base := "http://pass.test"
	h := &Handler{
		LoginRatePerMin: 10_000, PassRatePerMin: 10_000,
		SignIn: service.NewAccountService(postgres.NewAccountRepo(db), env.mail, google, service.AccountConfig{
			BaseURL: base, Product: "Event Pass", SessionTTL: time.Hour,
		}),
		Orgs: service.NewOrgService(postgres.NewOrgRepo(db), env.mail, base, "Event Pass"),
		Events: service.NewEventService(postgres.NewEventRepo(db), postgres.NewAccountRepo(db), postgres.NewOrgRepo(db),
			env.mail, base, "Event Pass"),
		BaseURL:   base,
		Product:   "Event Pass",
		CookieKey: []byte("test-cookie-key-0123456789abcdef"),
	}
	env.srv = httptest.NewServer(NewRouter(h, "../../../public", ""))
	t.Cleanup(func() {
		env.srv.Close()
		db.ExecContext(ctx, `DELETE FROM orgs WHERE slug LIKE $1`, env.prefix+"%")
		db.ExecContext(ctx, `DELETE FROM accounts WHERE email LIKE $1`, env.prefix+"%")
		db.ExecContext(ctx, `DELETE FROM login_links WHERE email LIKE $1`, env.prefix+"%")
		db.Close()
	})
	return env
}

// user is one browser: a cookie jar and the session's CSRF token.
type user struct {
	t     *testing.T
	env   *saasEnv
	c     *http.Client
	email string
	id    string
	csrf  string
}

func (env *saasEnv) newBrowser(t *testing.T, name string) *user {
	jar, _ := cookiejar.New(nil)
	return &user{t: t, env: env, c: &http.Client{Jar: jar}, email: env.prefix + name + "@test.local"}
}

// signIn goes through the emailed-link journey, starting at next.
func (u *user) signIn(next string) *http.Response {
	u.t.Helper()
	res := u.form("POST", "/login", url.Values{"email": {u.email}, "next": {next}})
	if res.StatusCode != http.StatusOK || !strings.Contains(body(res), "Check your email") {
		u.t.Fatalf("request link: HTTP %d", res.StatusCode)
	}
	token := u.env.mail.lastToken(u.t, u.email)

	res = u.get("/auth/email/verify?token=" + token)
	if res.StatusCode != http.StatusOK || !strings.Contains(body(res), "Finish signing in") {
		u.t.Fatalf("verify page: HTTP %d", res.StatusCode)
	}
	res = u.form("POST", "/auth/email/verify", url.Values{"token": {token}})
	u.loadMe()
	return res
}

func (u *user) loadMe() {
	u.t.Helper()
	status, b := u.api("GET", "/api/v1/me", nil)
	if status != http.StatusOK {
		u.t.Fatalf("GET /api/v1/me: HTTP %d %s", status, b)
	}
	var me struct {
		Account   domain.Account `json:"account"`
		CSRFToken string         `json:"csrf_token"`
	}
	json.Unmarshal(b, &me)
	u.id, u.csrf = me.Account.ID, me.CSRFToken
}

func (u *user) get(path string) *http.Response {
	u.t.Helper()
	res, err := u.c.Get(u.env.srv.URL + path)
	if err != nil {
		u.t.Fatal(err)
	}
	return res
}

// form posts a page form, adding the session's CSRF token when there is one.
func (u *user) form(method, path string, v url.Values) *http.Response {
	u.t.Helper()
	if u.csrf != "" && v.Get(csrfField) == "" {
		v.Set(csrfField, u.csrf)
	}
	req, _ := http.NewRequest(method, u.env.srv.URL+path, strings.NewReader(v.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := u.c.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	return res
}

func (u *user) api(method, path string, payload any) (int, []byte) {
	u.t.Helper()
	var rd io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u.env.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if u.csrf != "" {
		req.Header.Set(csrfHeader, u.csrf)
	}
	res, err := u.c.Do(req)
	if err != nil {
		u.t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return res.StatusCode, b
}

func (u *user) createOrg(name string) domain.OrgMembership {
	u.t.Helper()
	status, b := u.api("POST", "/api/v1/orgs", map[string]string{"name": "Org " + name, "slug": u.env.prefix + name})
	if status != http.StatusCreated {
		u.t.Fatalf("create org: HTTP %d %s", status, b)
	}
	var m domain.OrgMembership
	json.Unmarshal(b, &m)
	return m
}

func (u *user) invite(slug, email, role string) domain.Invite {
	u.t.Helper()
	status, b := u.api("POST", "/api/v1/orgs/"+slug+"/invites", map[string]string{"email": email, "role": role})
	if status != http.StatusCreated {
		u.t.Fatalf("invite: HTTP %d %s", status, b)
	}
	var inv domain.Invite
	json.Unmarshal(b, &inv)
	return inv
}

// join has u accept the newest invite emailed to them.
func (u *user) join() {
	u.t.Helper()
	status, b := u.api("POST", "/api/v1/invites/accept", map[string]string{"token": u.env.mail.lastToken(u.t, u.email)})
	if status != http.StatusOK {
		u.t.Fatalf("accept invite: HTTP %d %s", status, b)
	}
}

func body(res *http.Response) string {
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	return string(b)
}

// The M1 gate as a person does it: sign in by email, create an org, invite a
// member, who signs in from the invite and joins.
func TestJourneyCreateOrgAndInvite(t *testing.T) {
	env := newSaaSEnv(t)
	ann := env.newBrowser(t, "ann")

	res := ann.get("/app")
	if res.Request.URL.Path != "/login" {
		t.Fatalf("/app signed out landed on %s, want /login", res.Request.URL.Path)
	}
	res = ann.signIn("")
	if res.Request.URL.Path != "/app" || !strings.Contains(body(res), "Create an organization") {
		t.Fatalf("after sign-in landed on %s", res.Request.URL.Path)
	}

	slug := env.prefix + "club"
	res = ann.form("POST", "/app/orgs", url.Values{"name": {"Colombo Space Club"}, "slug": {slug}})
	page := body(res)
	if res.Request.URL.Path != "/o/"+slug || !strings.Contains(page, "Colombo Space Club") || !strings.Contains(page, "Organization created") {
		t.Fatalf("create org landed on %s", res.Request.URL.Path)
	}
	// The same address again is a friendly error, not a crash.
	res = ann.form("POST", "/app/orgs", url.Values{"name": {"Another"}, "slug": {slug}})
	if res.StatusCode != http.StatusConflict || !strings.Contains(body(res), "is taken") {
		t.Fatalf("duplicate slug: HTTP %d", res.StatusCode)
	}

	ben := env.newBrowser(t, "ben")
	res = ann.form("POST", "/o/"+slug+"/invites", url.Values{"email": {strings.ToUpper(ben.email)}, "role": {"member"}})
	if !strings.Contains(body(res), "Invite sent") {
		t.Fatalf("invite: HTTP %d", res.StatusCode)
	}

	// Ben opens the invite signed out, signs in, and comes back to it.
	inviteToken := env.mail.lastToken(t, ben.email)
	res = ben.get("/invite?token=" + inviteToken)
	if res.Request.URL.Path != "/login" {
		t.Fatalf("invite signed out landed on %s", res.Request.URL.Path)
	}
	res = ben.signIn(res.Request.URL.Query().Get("next"))
	page = body(res)
	if res.Request.URL.Path != "/invite" || !strings.Contains(page, "Join Colombo Space Club") {
		t.Fatalf("after sign-in landed on %s", res.Request.URL.Path)
	}
	res = ben.form("POST", "/invite/accept", url.Values{"token": {inviteToken}})
	page = body(res)
	if res.Request.URL.Path != "/o/"+slug || !strings.Contains(page, "You joined") {
		t.Fatalf("accept landed on %s", res.Request.URL.Path)
	}
	if page = body(ben.get("/o/" + slug + "/members")); !strings.Contains(page, ann.email) {
		t.Fatal("members page doesn't list the owner")
	}

	status, b := ann.api("GET", "/api/v1/orgs/"+slug+"/members", nil)
	var members []domain.OrgMember
	json.Unmarshal(b, &members)
	if status != http.StatusOK || len(members) != 2 || members[1].Role != domain.OrgRoleMember {
		t.Fatalf("members: HTTP %d %s", status, b)
	}

	// Signing out ends the session server-side.
	ben.form("POST", "/logout", url.Values{})
	if status, _ := ben.api("GET", "/api/v1/me", nil); status != http.StatusUnauthorized {
		t.Fatalf("after sign-out /me: HTTP %d", status)
	}
}

func TestEmailLinkIsSingleUseAndStaysOnSite(t *testing.T) {
	env := newSaaSEnv(t)
	ann := env.newBrowser(t, "ann")
	res := ann.form("POST", "/login", url.Values{"email": {ann.email}, "next": {"//evil.example/steal"}})
	res.Body.Close()
	token := env.mail.lastToken(t, ann.email)

	res = ann.form("POST", "/auth/email/verify", url.Values{"token": {token}})
	if res.Request.URL.Host != strings.TrimPrefix(env.srv.URL, "http://") || res.Request.URL.Path != "/app" {
		t.Fatalf("redirected to %s, want this site's /app", res.Request.URL)
	}
	other := env.newBrowser(t, "ann") // same address, another device
	res = other.form("POST", "/auth/email/verify", url.Values{"token": {token}})
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(body(res), "already used") {
		t.Fatalf("reused link: HTTP %d", res.StatusCode)
	}

	// A form posted from another site is refused.
	req, _ := http.NewRequest("POST", env.srv.URL+"/login", strings.NewReader("email="+url.QueryEscape(ann.email)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if r2.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site login form: HTTP %d", r2.StatusCode)
	}
	r2.Body.Close()
}

// fill replaces the path parameters in an org route with concrete values.
func fill(pattern string, vals map[string]string) string {
	for k, v := range vals {
		pattern = strings.ReplaceAll(pattern, "{"+k+"}", v)
	}
	return pattern
}

func TestTenantIsolation(t *testing.T) {
	env := newSaaSEnv(t)
	ann, bob := env.newBrowser(t, "ann"), env.newBrowser(t, "bob")
	ann.signIn("")
	bob.signIn("")
	orgA := ann.createOrg("a")
	orgB := bob.createOrg("b")
	invA := ann.invite(orgA.Slug, env.prefix+"guest-a@test.local", "member")
	invB := bob.invite(orgB.Slug, env.prefix+"guest-b@test.local", "member")

	payload := map[string]any{"role": "member", "email": env.prefix + "x@test.local", "name": "Isolation", "external_sharing": true}
	// call returns the status, and for pages the path the browser ended on.
	call := func(u *user, rt orgRoute, path string) (int, string) {
		if rt.api {
			var p any
			if rt.method == "POST" || rt.method == "PATCH" {
				p = payload
			}
			status, _ := u.api(rt.method, path, p)
			return status, ""
		}
		var res *http.Response
		if rt.method == "GET" {
			res = u.get(path)
		} else {
			res = u.form(rt.method, path, url.Values{"role": {"member"}, "email": {env.prefix + "x@test.local"}, "name": {"Isolation"}})
		}
		res.Body.Close()
		return res.StatusCode, res.Request.URL.Path
	}

	anon := env.newBrowser(t, "anon")
	for _, rt := range orgRoutes {
		pathB := fill(rt.pattern, map[string]string{"org": orgB.Slug, "account": bob.id, "id": invB.ID})
		if got, _ := call(ann, rt, pathB); got != http.StatusNotFound {
			t.Errorf("%s %s as a member of another org: HTTP %d, want 404", rt.method, pathB, got)
		}
		// Signed out, the API says 401 and pages send you to sign in.
		got, landed := call(anon, rt, pathB)
		if rt.api && got != http.StatusUnauthorized || !rt.api && landed != "/login" {
			t.Errorf("%s %s signed out: HTTP %d at %s", rt.method, pathB, got, landed)
		}
	}

	// The same routes work in your own org, so the 404s above are isolation,
	// not wrong paths. GETs only: the changes are covered by the role tests.
	for _, rt := range orgRoutes {
		if rt.method != "GET" {
			continue
		}
		pathA := fill(rt.pattern, map[string]string{"org": orgA.Slug, "account": ann.id, "id": invA.ID})
		if got, _ := call(ann, rt, pathA); got != http.StatusOK {
			t.Errorf("%s %s in own org: HTTP %d, want 200", rt.method, pathA, got)
		}
	}

	// Org B is untouched.
	status, b := bob.api("GET", "/api/v1/orgs/"+orgB.Slug+"/invites", nil)
	var invs []domain.Invite
	json.Unmarshal(b, &invs)
	if status != http.StatusOK || len(invs) != 1 || invs[0].ID != invB.ID {
		t.Fatalf("org B invites after the attempts: HTTP %d %s", status, b)
	}
	status, b = bob.api("GET", "/api/v1/orgs/"+orgB.Slug+"/members", nil)
	var members []domain.OrgMember
	json.Unmarshal(b, &members)
	if status != http.StatusOK || len(members) != 1 || members[0].Role != domain.OrgRoleOwner {
		t.Fatalf("org B members after the attempts: HTTP %d %s", status, b)
	}
}

func TestOrgRoles(t *testing.T) {
	env := newSaaSEnv(t)
	owner, admin, member := env.newBrowser(t, "owner"), env.newBrowser(t, "admin"), env.newBrowser(t, "member")
	for _, u := range []*user{owner, admin, member} {
		u.signIn("")
	}
	org := owner.createOrg("roles")
	owner.invite(org.Slug, admin.email, "admin")
	admin.join()
	owner.invite(org.Slug, member.email, "member")
	member.join()
	base := "/api/v1/orgs/" + org.Slug

	cases := []struct {
		name   string
		u      *user
		method string
		path   string
		body   any
		want   int
	}{
		{"member lists members", member, "GET", base + "/members", nil, 200},
		{"member can't see invites", member, "GET", base + "/invites", nil, 403},
		{"member can't invite", member, "POST", base + "/invites", map[string]string{"email": env.prefix + "m1@test.local", "role": "member"}, 403},
		{"member can't change roles", member, "PATCH", base + "/members/" + admin.id, map[string]string{"role": "member"}, 403},
		{"admin invites", admin, "POST", base + "/invites", map[string]string{"email": env.prefix + "a1@test.local", "role": "admin"}, 201},
		{"admin can't make owners", admin, "PATCH", base + "/members/" + member.id, map[string]string{"role": "owner"}, 403},
		{"admin can't demote the owner", admin, "PATCH", base + "/members/" + owner.id, map[string]string{"role": "member"}, 403},
		{"admin can't remove the owner", admin, "DELETE", base + "/members/" + owner.id, nil, 403},
		{"bad role", owner, "PATCH", base + "/members/" + member.id, map[string]string{"role": "king"}, 400},
		{"owner can't leave as the last owner", owner, "DELETE", base + "/members/" + owner.id, nil, 409},
		{"owner makes an owner", owner, "PATCH", base + "/members/" + admin.id, map[string]string{"role": "owner"}, 204},
		{"member leaves", member, "DELETE", base + "/members/" + member.id, nil, 204},
		{"former member gets 404", member, "GET", base + "/members", nil, 404},
	}
	for _, c := range cases {
		if got, b := c.u.api(c.method, c.path, c.body); got != c.want {
			t.Errorf("%s: HTTP %d %s, want %d", c.name, got, b, c.want)
		}
	}

	// Changes without the CSRF token are refused even with a valid session.
	saved := owner.csrf
	owner.csrf = ""
	if got, _ := owner.api("POST", base+"/invites", map[string]string{"email": env.prefix + "c@test.local", "role": "member"}); got != http.StatusForbidden {
		t.Errorf("invite without CSRF: HTTP %d, want 403", got)
	}
	res := owner.form("POST", "/o/"+org.Slug+"/invites", url.Values{"email": {env.prefix + "c@test.local"}, "role": {"member"}})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("page form without CSRF: HTTP %d, want 403", res.StatusCode)
	}
	res.Body.Close()
	owner.csrf = saved

	// Only the address an invite was sent to can use it.
	stranger := env.newBrowser(t, "stranger")
	stranger.signIn("")
	owner.invite(org.Slug, env.prefix+"intended@test.local", "member")
	token := env.mail.lastToken(t, env.prefix+"intended@test.local")
	if got, b := stranger.api("POST", "/api/v1/invites/accept", map[string]string{"token": token}); got != http.StatusForbidden {
		t.Errorf("forwarded invite: HTTP %d %s, want 403", got, b)
	}
}

// Google sign-in end to end, with Google's token endpoint faked: start sets
// the state cookie and redirects with PKCE; the callback checks state, swaps
// the code and signs in.
func TestGoogleSignIn(t *testing.T) {
	var mu sync.Mutex
	var nonce, verifier string
	fakeGoogle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") != verifier || r.Form.Get("client_secret") != "secret" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		claims, _ := json.Marshal(map[string]any{
			"iss": "https://accounts.google.com", "aud": "client-1", "exp": time.Now().Add(time.Hour).Unix(),
			"sub": "g-" + r.Form.Get("code_verifier")[:8], "email": strings.ToUpper(googleEmail), "email_verified": true,
			"name": "Gita Google", "nonce": nonce,
		})
		json.NewEncoder(w).Encode(map[string]string{"id_token": "e30." + base64URL(claims) + ".sig"})
	}))
	defer fakeGoogle.Close()

	g := service.NewGoogleOAuth("client-1", "secret", "http://pass.test/auth/google/callback")
	g.TokenURL = fakeGoogle.URL
	env := newSaaSEnvWith(t, g)
	googleEmail = env.prefix + "gita@test.local"
	u := env.newBrowser(t, "gita")
	u.c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	res := u.get("/auth/google/start?next=/app")
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	q := loc.Query()
	if res.StatusCode != http.StatusFound || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Fatalf("start: HTTP %d to %s", res.StatusCode, loc)
	}

	// A forged state is refused before Google is called.
	res = u.get("/auth/google/callback?code=good-code&state=forged")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("forged state: HTTP %d", res.StatusCode)
	}
	res.Body.Close()

	// The verifier lives in the signed cookie; read it back the way the fake
	// Google would see it in the token request.
	res = u.get("/auth/google/start?next=/app")
	res.Body.Close()
	loc, _ = url.Parse(res.Header.Get("Location"))
	q = loc.Query()
	for _, c := range u.c.Jar.Cookies(&url.URL{Scheme: "http", Host: strings.TrimPrefix(env.srv.URL, "http://"), Path: "/auth/google"}) {
		if c.Name == googleCookie {
			body, _, _ := strings.Cut(c.Value, ".")
			raw, _ := base64.RawURLEncoding.DecodeString(body)
			var st googleState
			json.Unmarshal(raw, &st)
			mu.Lock()
			nonce, verifier = q.Get("nonce"), st.Verifier
			mu.Unlock()
		}
	}
	if verifier == "" {
		t.Fatal("no Google state cookie")
	}
	res = u.get("/auth/google/callback?code=good-code&state=" + url.QueryEscape(q.Get("state")))
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/app" {
		t.Fatalf("callback: HTTP %d to %s", res.StatusCode, res.Header.Get("Location"))
	}
	u.loadMe()
	status, b := u.api("GET", "/api/v1/me", nil)
	if status != http.StatusOK || !strings.Contains(string(b), `"name":"Gita Google"`) || !strings.Contains(string(b), googleEmail) {
		t.Fatalf("me after Google sign-in: HTTP %d %s", status, b)
	}

	// The state cookie is single use: replaying the callback fails.
	res = u.get("/auth/google/callback?code=good-code&state=" + url.QueryEscape(q.Get("state")))
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback: HTTP %d", res.StatusCode)
	}
}

var googleEmail string

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
