package postgres

// Database roles (migration 017, security review F1): the API's login can
// read only the sign-in tables directly; tenant data needs withTenant or
// withSystem. Skipped unless EVENT_PASS_TEST_DATABASE_URL is set; the test
// user must be allowed to CREATE ROLE.

import (
	"context"
	"database/sql"
	"net/url"
	"strings"
	"testing"
)

func TestAPILoginCannotSkipTenantScope(t *testing.T) {
	owner := openTestDB(t)
	ctx := context.Background()
	login, password := "it_api_"+randTag(), "pw-"+randTag()+randTag()
	if _, err := owner.ExecContext(ctx, `CREATE ROLE `+login+` LOGIN PASSWORD '`+password+`' IN ROLE event_pass_api`); err != nil {
		t.Skipf("cannot create a login role here: %v", err)
	}
	t.Cleanup(func() { owner.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+login) })

	u, err := url.Parse(strings.TrimSpace(testDSN()))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(login, password)
	api, err := Open(ctx, u.String(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()

	// Bare queries: sign-in tables yes, tenant tables no.
	var n int
	if err := api.QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n); err != nil {
		t.Errorf("api login reading sessions: %v", err)
	}
	for _, table := range []string{"events", "event_participants", "orgs", "outbox", "integrations", "audit_log"} {
		err := api.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("bare SELECT on %s as the api login: err %v, want permission denied", table, err)
		}
	}

	// The tenant role still works, and sees only its org (none here).
	if err := withTenant(ctx, api, newID(), "", func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n)
	}); err != nil || n != 0 {
		t.Errorf("withTenant for an unknown org: %d events, err %v; want 0", n, err)
	}
	// The system role can be reached only through withSystem.
	if err := withSystem(ctx, api, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox`).Scan(&n)
	}); err != nil {
		t.Errorf("withSystem as the api login: %v", err)
	}

	// The owner check main.go runs at startup passes for this login.
	var owned int
	api.QueryRowContext(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tableowner = current_user`).Scan(&owned)
	if owned != 0 {
		t.Errorf("api login owns %d tables", owned)
	}
}
