package postgres

// Static guard for tenant isolation (security review F1). It runs without a
// database, so it runs on every `go test ./...`.
//
// Production connects as a member of event_pass_api, which can read only the
// sign-in tables directly (migration 017). Everything else has to go through
// withTenant (one org / one account) or withSystem (cross-tenant, privileged).
// This test makes both rules visible in review:
//
//   - a method may call r.db.Query/Exec/Begin/... directly only if it is in
//     directDBAllowed (sign-in tables, or the two helpers themselves);
//   - a function may call withSystem only if it is in systemCallers.
//
// Adding to either list is a security decision: say why in the comment.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// directDBAllowed: functions that may use *sql.DB without switching role.
var directDBAllowed = map[string]string{
	"withTenant": "switches to event_pass_tenant",
	"withSystem": "switches to event_pass_system",
	// Sign-in tables, granted to event_pass_api directly (no tenant data).
	"AccountRepo.CreateLoginLink":   "login_links",
	"AccountRepo.RecentLoginLinks":  "login_links",
	"AccountRepo.ConsumeLoginLink":  "login_links",
	"AccountRepo.SignIn":            "accounts, auth_identities",
	"AccountRepo.GetAccount":        "accounts",
	"AccountRepo.FindByEmail":       "accounts",
	"AccountRepo.CreateSession":     "sessions",
	"AccountRepo.SessionByToken":    "sessions, accounts",
	"AccountRepo.RevokeSession":     "sessions",
	"AccountRepo.TouchSession":      "sessions",
	"AccountRepo.ListSessions":      "sessions",
	"AccountRepo.RevokeSessionByID": "sessions",
	"AccountRepo.RevokeAllSessions": "sessions",
	"RateLimitRepo.Hit":             "rate_limits (no tenant data)",
	"RateLimitRepo.Sweep":           "rate_limits (no tenant data)",
}

// systemCallers: every function allowed to run as event_pass_system, and why.
var systemCallers = map[string]string{
	"OutboxRepo.Claim":              "outbox worker serves every org",
	"OutboxRepo.MarkSent":           "outbox worker",
	"OutboxRepo.MarkFailed":         "outbox worker",
	"HealthRepo.EmailDelay":         "health: oldest overdue email across orgs",
	"EventRepo.EventByLink":         "view-only link token is the credential; org unknown",
	"RegistrationRepo.PublicEvent":  "public page by slug; org unknown until resolved",
	"CheckinRepo.ActiveEvents":      "live feed poll across orgs",
	"OrgRepo.InviteByToken":         "invite token is the credential; org unknown",
	"IntegrationRepo.Due":           "integration worker serves every org",
	"IntegrationRepo.Finished":      "integration worker",
	"IntegrationRepo.Secret":        "sealed refresh token column is not granted to the tenant role",
	"PortalRepo.PendingClaims":      "registrations in other orgs with the signed-in account's own email",
	"PortalRepo.ClaimPass":          "links one such registration after the holder confirms",
	"PortalRepo.DeclineClaim":       "hides one such registration",
	"RetentionRepo.ClearBirthDates": "retention job across orgs",
	"RetentionRepo.ExpireLocations": "retention job across orgs",
	"CheckinRepo.Notify":            "live feed NOTIFY carries only an event ID",
}

var dbMethods = map[string]bool{
	"QueryContext": true, "QueryRowContext": true, "ExecContext": true, "BeginTx": true,
	"Query": true, "QueryRow": true, "Exec": true, "Begin": true,
	"PrepareContext": true, "Prepare": true, "Conn": true,
}

func TestRepositoryDatabaseAccessIsScoped(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var problems []string
	seenSystem := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			fname := funcName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "withSystem" {
						seenSystem[fname] = true
						if _, ok := systemCallers[fname]; !ok {
							problems = append(problems, fset.Position(call.Pos()).String()+": "+fname+
								" runs as event_pass_system but is not in systemCallers")
						}
					}
				case *ast.SelectorExpr:
					if dbMethods[fun.Sel.Name] && isDBHandle(fun.X) {
						if _, ok := directDBAllowed[fname]; !ok {
							problems = append(problems, fset.Position(call.Pos()).String()+": "+fname+
								" queries *sql.DB directly; use withTenant or withSystem")
						}
					}
				}
				return true
			})
		}
	}
	// A stale entry would let a future function of that name in silently.
	for name := range systemCallers {
		if !seenSystem[name] {
			problems = append(problems, "systemCallers lists "+name+", which no longer calls withSystem; remove it")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// isDBHandle matches r.db and a bare db, the *sql.DB names this package uses.
func isDBHandle(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name == "db"
	case *ast.SelectorExpr:
		return v.Sel.Name == "db"
	}
	return false
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
