package postgres

// Integration tests for migration 007: accounts, orgs and the Row Level
// Security wall. Skipped unless EVENT_PASS_TEST_DATABASE_URL is set (see
// scan_repo_integration_test.go). Rows use it-<tag>-* emails and slugs and
// are deleted afterwards.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mayura-andrew/event-pass-platform/internal/authz"
	"github.com/mayura-andrew/event-pass-platform/internal/domain"
)

type tenancyFixture struct {
	db       *sql.DB
	accounts *AccountRepo
	orgs     *OrgRepo
	tag      string
}

func newTenancyFixture(t *testing.T) *tenancyFixture {
	t.Helper()
	db := openTestDB(t)
	f := &tenancyFixture{db: db, accounts: NewAccountRepo(db), orgs: NewOrgRepo(db), tag: randTag()}
	t.Cleanup(func() {
		ctx := context.Background()
		// Orgs first: removing an account that is an org's last owner is refused.
		db.ExecContext(ctx, `DELETE FROM orgs WHERE slug LIKE $1`, "it-"+f.tag+"-%")
		db.ExecContext(ctx, `DELETE FROM accounts WHERE email LIKE $1`, "it-"+f.tag+"-%")
		db.ExecContext(ctx, `DELETE FROM login_links WHERE email LIKE $1`, "it-"+f.tag+"-%")
	})
	return f
}

func (f *tenancyFixture) account(t *testing.T, name string) *domain.Account {
	t.Helper()
	email := fmt.Sprintf("it-%s-%s@test.local", f.tag, name)
	a, err := f.accounts.SignIn(context.Background(), domain.ProviderEmail, email, email, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// org creates an org owned by owner and returns the owner's scope in it.
func (f *tenancyFixture) org(t *testing.T, owner *domain.Account, name string) (authz.OrgScope, *domain.OrgMembership) {
	t.Helper()
	as := authz.NewAccountScope(owner.ID)
	m, err := f.orgs.CreateOrg(context.Background(), as, "Org "+name, fmt.Sprintf("it-%s-%s", f.tag, name))
	if err != nil {
		t.Fatal(err)
	}
	return authz.ForMember(as, m.ID, m.Role), m
}

func (f *tenancyFixture) invite(t *testing.T, sc authz.OrgScope, email, role string) (*domain.Invite, []byte) {
	t.Helper()
	hash := []byte("hash-" + f.tag + "-" + email + "-" + time.Now().String())
	inv, err := f.orgs.CreateInvite(context.Background(), sc, email, role, hash, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return inv, hash
}

func count(t *testing.T, tx *sql.Tx, query string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// Wall 2: with no tenant set, the tenant role sees no tenant rows at all,
// even with no WHERE clause.
func TestRLSNoTenantSeesNothing(t *testing.T) {
	f := newTenancyFixture(t)
	a := f.account(t, "a")
	sc, _ := f.org(t, a, "a")
	f.invite(t, sc, "it-"+f.tag+"-x@test.local", domain.OrgRoleMember)

	err := withTenant(context.Background(), f.db, "", "", func(tx *sql.Tx) error {
		for _, table := range []string{"orgs", "org_members", "invites", "accounts"} {
			if n := count(t, tx, "SELECT count(*) FROM "+table); n != 0 {
				t.Errorf("%s: %d rows visible with no tenant set", table, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Wall 2: scoped to org A, queries that name org B, or name nothing, see and
// change only org A.
func TestRLSScopesReadsAndWrites(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a, b := f.account(t, "a"), f.account(t, "b")
	scA, orgA := f.org(t, a, "a")
	scB, orgB := f.org(t, b, "b")
	f.invite(t, scA, "it-"+f.tag+"-xa@test.local", domain.OrgRoleMember)
	f.invite(t, scB, "it-"+f.tag+"-xb@test.local", domain.OrgRoleMember)

	err := withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM orgs WHERE id <> $1`, orgA.ID); n != 0 {
			t.Errorf("sees %d other orgs", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM org_members WHERE org_id = $1`, orgB.ID); n != 0 {
			t.Errorf("sees %d members of org B", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM invites`); n != 1 {
			t.Errorf("sees %d invites, want only org A's 1", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM accounts WHERE id = $1`, b.ID); n != 0 {
			t.Error("sees an account that isn't in org A")
		}
		res, err := tx.ExecContext(ctx, `UPDATE orgs SET name = 'hijacked' WHERE id = $1`, orgB.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 0 {
			t.Error("updated org B")
		}
		res, err = tx.ExecContext(ctx, `UPDATE invites SET role = 'admin'`)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Errorf("unscoped UPDATE touched %d invites, want org A's 1", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Writing a row into another org is refused outright.
	err = withTenant(ctx, f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO org_members (org_id, account_id, role) VALUES ($1, $2, 'owner')`, orgB.ID, a.ID)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("insert into org B: %v, want an RLS violation (42501)", err)
	}

	var name, role string
	f.db.QueryRowContext(ctx, `SELECT name FROM orgs WHERE id = $1`, orgB.ID).Scan(&name)
	f.db.QueryRowContext(ctx, `SELECT role FROM invites WHERE org_id = $1`, orgB.ID).Scan(&role)
	if name != orgB.Name || role != domain.OrgRoleMember {
		t.Fatalf("org B changed: name %q, invite role %q", name, role)
	}
}

// The tenant role reaches tenant tables only: not sessions, not the current
// build's delegates and staff.
func TestTenantRoleCannotReadOtherTables(t *testing.T) {
	f := newTenancyFixture(t)
	a := f.account(t, "a")
	_, orgA := f.org(t, a, "a")
	for _, table := range []string{"sessions", "login_links", "auth_identities", "participants", "admins", "scans"} {
		err := withTenant(context.Background(), f.db, orgA.ID, a.ID, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(context.Background(), "SELECT 1 FROM "+table+" LIMIT 1")
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("%s: %v, want permission denied", table, err)
		}
	}
}

// Wall 1 at the repository: a scope from org A can't reach org B's rows by ID.
func TestOrgRepoIsolation(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a, b := f.account(t, "a"), f.account(t, "b")
	scA, _ := f.org(t, a, "a")
	scB, orgB := f.org(t, b, "b")
	invB, _ := f.invite(t, scB, "it-"+f.tag+"-xb@test.local", domain.OrgRoleMember)

	if _, err := f.orgs.Membership(ctx, authz.NewAccountScope(a.ID), orgB.Slug); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("membership of org B: %v", err)
	}
	if _, err := f.orgs.GetMember(ctx, scA, b.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("get B's member via A: %v", err)
	}
	if err := f.orgs.SetMemberRole(ctx, scA, b.ID, domain.OrgRoleMember); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("set B's member role via A: %v", err)
	}
	if err := f.orgs.RemoveMember(ctx, scA, b.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("remove B's member via A: %v", err)
	}
	if err := f.orgs.RevokeInvite(ctx, scA, invB.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("revoke B's invite via A: %v", err)
	}
	if err := f.orgs.RevokeInvite(ctx, scA, "not-a-uuid"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("revoke malformed id: %v", err)
	}
	invs, err := f.orgs.ListInvites(ctx, scB)
	if err != nil || len(invs) != 1 {
		t.Fatalf("org B invites after attempts: %v, %v", invs, err)
	}
}

func TestLastOwnerStays(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a, b := f.account(t, "a"), f.account(t, "b")
	sc, org := f.org(t, a, "a")

	if err := f.orgs.RemoveMember(ctx, sc, a.ID); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("remove last owner: %v, want ErrConflict", err)
	}
	if err := f.orgs.SetMemberRole(ctx, sc, a.ID, domain.OrgRoleAdmin); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("demote last owner: %v, want ErrConflict", err)
	}

	_, hash := f.invite(t, sc, b.Email, domain.OrgRoleMember)
	inv, err := f.orgs.InviteByToken(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.orgs.AcceptInvite(ctx, authz.NewAccountScope(b.ID), inv); err != nil {
		t.Fatal(err)
	}
	if err := f.orgs.SetMemberRole(ctx, sc, b.ID, domain.OrgRoleOwner); err != nil {
		t.Fatal(err)
	}
	if err := f.orgs.SetMemberRole(ctx, sc, a.ID, domain.OrgRoleMember); err != nil {
		t.Fatalf("demote with another owner: %v", err)
	}
	// Deleting the org removes every member, owners included.
	if _, err := f.db.ExecContext(ctx, `DELETE FROM orgs WHERE id = $1`, org.ID); err != nil {
		t.Fatalf("delete org: %v", err)
	}
}

func TestInviteLifecycle(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	a, b := f.account(t, "a"), f.account(t, "b")
	sc, org := f.org(t, a, "a")

	first, oldHash := f.invite(t, sc, b.Email, domain.OrgRoleMember)
	// Inviting again replaces the open invite: same row, new token and role.
	again, newHash := f.invite(t, sc, b.Email, domain.OrgRoleAdmin)
	if again.ID != first.ID || again.Role != domain.OrgRoleAdmin {
		t.Fatalf("re-invite: %+v, want same id %s with role admin", again, first.ID)
	}
	if _, err := f.orgs.InviteByToken(ctx, oldHash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("old token still works: %v", err)
	}

	inv, err := f.orgs.InviteByToken(ctx, newHash)
	if err != nil || inv.OrgSlug != org.Slug || inv.InvitedBy != a.Email {
		t.Fatalf("invite details: %+v, %v", inv, err)
	}
	m, err := f.orgs.AcceptInvite(ctx, authz.NewAccountScope(b.ID), inv)
	if err != nil || m.Role != domain.OrgRoleAdmin || m.ID != org.ID {
		t.Fatalf("accept: %+v, %v", m, err)
	}
	if _, err := f.orgs.AcceptInvite(ctx, authz.NewAccountScope(b.ID), inv); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second accept: %v, want ErrNotFound", err)
	}
	if _, err := f.orgs.CreateInvite(ctx, sc, b.Email, domain.OrgRoleMember, []byte("h2-"+f.tag), time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("invite a member: %v, want ErrConflict", err)
	}

	orgs, err := f.orgs.ListMyOrgs(ctx, authz.NewAccountScope(b.ID))
	if err != nil || len(orgs) != 1 || orgs[0].Slug != org.Slug {
		t.Fatalf("b's orgs: %+v, %v", orgs, err)
	}
	members, err := f.orgs.ListMembers(ctx, sc)
	if err != nil || len(members) != 2 || members[0].AccountID != a.ID {
		t.Fatalf("members: %+v, %v", members, err)
	}
}

func TestSignInLinksIdentities(t *testing.T) {
	f := newTenancyFixture(t)
	ctx := context.Background()
	byEmail := f.account(t, "a")
	byGoogle, err := f.accounts.SignIn(ctx, domain.ProviderGoogle, "google-sub-"+f.tag, byEmail.Email, "Ann", "https://lh3.googleusercontent.com/a")
	if err != nil {
		t.Fatal(err)
	}
	if byGoogle.ID != byEmail.ID || byGoogle.Name != "Ann" {
		t.Fatalf("google sign-in made %+v, want account %s named Ann", byGoogle, byEmail.ID)
	}
	again, err := f.accounts.SignIn(ctx, domain.ProviderGoogle, "google-sub-"+f.tag, byEmail.Email, "Renamed", "")
	if err != nil || again.ID != byEmail.ID || again.Name != "Ann" {
		t.Fatalf("second google sign-in: %+v, %v (name must not be overwritten)", again, err)
	}

	// Sessions: live, then revoked.
	s, err := f.accounts.CreateSession(ctx, byEmail.ID, []byte("tok-"+f.tag), "csrf", time.Now().Add(time.Hour), "ua", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if got, acct, err := f.accounts.SessionByToken(ctx, []byte("tok-"+f.tag)); err != nil || got.ID != s.ID || acct.ID != byEmail.ID {
		t.Fatalf("session lookup: %v", err)
	}
	if err := f.accounts.RevokeSession(ctx, []byte("tok-"+f.tag)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.accounts.SessionByToken(ctx, []byte("tok-"+f.tag)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("revoked session: %v", err)
	}

	// Sign-in links are single use.
	link := []byte("link-" + f.tag)
	if err := f.accounts.CreateLoginLink(ctx, link, byEmail.Email, "/app", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if email, next, err := f.accounts.ConsumeLoginLink(ctx, link); err != nil || email != byEmail.Email || next != "/app" {
		t.Fatalf("consume: %q %q %v", email, next, err)
	}
	if _, _, err := f.accounts.ConsumeLoginLink(ctx, link); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second use: %v", err)
	}
}

func TestNewIDIsUUIDv7(t *testing.T) {
	a, b := newID(), newID()
	if len(a) != 36 || a[14] != '7' || !isRFCVariant(a) {
		t.Fatalf("newID() = %s", a)
	}
	if a == b {
		t.Fatal("duplicate ids")
	}
}

func isRFCVariant(s string) bool {
	switch s[19] {
	case '8', '9', 'a', 'b':
		return true
	}
	return false
}
