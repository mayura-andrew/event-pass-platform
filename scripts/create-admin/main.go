// Command create-admin creates (or resets the password of) a staff account,
// typically the first superadmin of a new database. Later staff are added from
// the dashboard's Staff page, which emails their credentials.
//
//	export DATABASE_URL='postgres://...'          # Supabase: the Session pooler string
//	go run ./scripts/create-admin -email lead@example.org -name "Event lead" -role superadmin
//	go run ./scripts/create-admin -list            # check what's there
//
// The password is typed at a hidden prompt (twice), or read from
// ADMIN_PASSWORD for automation, so it never lands in shell history as a
// flag. Before writing, the command shows which database it is about to
// change and asks for confirmation (-yes skips that).
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

func main() {
	email := flag.String("email", "", "account email (required unless -list)")
	name := flag.String("name", "", "full name (required unless -list)")
	role := flag.String("role", "scanner", "superadmin | admin | scanner")
	temporary := flag.Bool("temporary", false, "make the password temporary: the person must choose a new one at first sign-in")
	list := flag.Bool("list", false, "list existing accounts and exit")
	yes := flag.Bool("yes", false, "don't ask for confirmation")
	flag.Parse()
	log.SetFlags(0)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (Supabase: Project Settings > Database > Connection string > Session pooler)")
	}
	target := describeTarget(dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("cannot connect to %s: %v", target, err)
	}
	if err := checkSchema(ctx, db); err != nil {
		log.Fatal(err)
	}

	if *list {
		if err := listAccounts(ctx, db, target); err != nil {
			log.Fatal(err)
		}
		return
	}

	addr := strings.ToLower(strings.TrimSpace(*email))
	fullName := strings.Join(strings.Fields(*name), " ")
	if addr == "" || fullName == "" {
		flag.Usage()
		os.Exit(2)
	}
	if !strings.Contains(addr, "@") || strings.ContainsAny(addr, " \t\r\n") {
		log.Fatalf("%q is not an email address", *email)
	}
	switch *role {
	case "superadmin", "admin", "scanner":
	default:
		log.Fatalf("invalid role %q (superadmin, admin or scanner)", *role)
	}

	// Say exactly what will happen, and where, before asking for anything.
	var existing string
	err = db.QueryRowContext(ctx, `SELECT role FROM admins WHERE email = $1`, addr).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		fmt.Printf("Will CREATE %s %s (%s) on %s\n", *role, addr, fullName, target)
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Printf("Will RESET %s on %s: currently %s, becomes %s, password replaced\n", addr, target, existing, *role)
	}
	if !*yes && !confirm("Continue? [y/N] ") {
		fmt.Println("Nothing changed.")
		return
	}

	minLen := 10
	if *role == "superadmin" {
		minLen = 12 // the account that can create every other account
	}
	password, err := readPassword(minLen)
	if err != nil {
		log.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}

	var id string
	err = db.QueryRowContext(ctx, `
		INSERT INTO admins (email, password_hash, full_name, role, force_password_change, password_changed_at)
		VALUES ($1, $2, $3, $4, $5, CASE WHEN $5 THEN NULL ELSE NOW() END)
		ON CONFLICT (email) DO UPDATE SET
			password_hash         = EXCLUDED.password_hash,
			full_name             = EXCLUDED.full_name,
			role                  = EXCLUDED.role,
			is_active             = true,
			force_password_change = EXCLUDED.force_password_change,
			password_changed_at   = EXCLUDED.password_changed_at
		RETURNING id`,
		addr, string(hash), fullName, *role, *temporary,
	).Scan(&id)
	if err != nil {
		log.Fatal(err)
	}
	// Managers scan everywhere; grants only apply to scanner accounts.
	if *role != "scanner" {
		if _, err := db.ExecContext(ctx, `DELETE FROM staff_scenario_grants WHERE admin_id = $1`, id); err != nil {
			log.Fatal(err)
		}
	}

	fmt.Printf("Saved %s (%s, id %s) on %s\n", addr, *role, id, target)
	if *temporary {
		fmt.Println("The password is temporary: they'll be asked to choose their own at first sign-in.")
	}
	switch *role {
	case "scanner":
		fmt.Println("Scanner accounts only see checkpoints granted to them; assign them on the dashboard's Staff page.")
	default:
		fmt.Println("Sign in at <your site>/admin/.")
	}
}

// describeTarget names the database without its password, e.g.
// "postgres @ aws-0-ap-south-1.pooler.supabase.com:5432 (user postgres.abcd)".
func describeTarget(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		return "the database in DATABASE_URL"
	}
	return fmt.Sprintf("%s @ %s (user %s)", strings.TrimPrefix(u.Path, "/"), u.Host, u.User.Username())
}

// checkSchema fails clearly when the migrations haven't been run yet.
func checkSchema(ctx context.Context, db *sql.DB) error {
	var ok bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'admins' AND column_name = 'force_password_change'
		) AND to_regclass('staff_scenario_grants') IS NOT NULL`).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("this database is missing the schema: run migrations/001_schema.sql, 002_dashboard_schema.sql and 003_rbac_and_emailing.sql first (Supabase: SQL Editor)")
	}
	return nil
}

func listAccounts(ctx context.Context, db *sql.DB, target string) error {
	rows, err := db.QueryContext(ctx, `
		SELECT email, role, is_active, force_password_change, last_login_at
		FROM admins
		ORDER BY CASE role WHEN 'superadmin' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, email`)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("Accounts on %s:\n", target)
	n := 0
	for rows.Next() {
		var (
			email, role  string
			active, temp bool
			lastLogin    sql.NullTime
		)
		if err := rows.Scan(&email, &role, &active, &temp, &lastLogin); err != nil {
			return err
		}
		status := "active"
		if !active {
			status = "deactivated"
		} else if temp {
			status = "temporary password"
		}
		seen := "never signed in"
		if lastLogin.Valid {
			seen = "last sign-in " + lastLogin.Time.Local().Format("2006-01-02 15:04")
		}
		fmt.Printf("  %-11s %-36s %-19s %s\n", role, email, status, seen)
		n++
	}
	if n == 0 {
		fmt.Println("  (none yet)")
	}
	return rows.Err()
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// readPassword takes ADMIN_PASSWORD if set, else prompts twice without echo.
func readPassword(minLen int) (string, error) {
	p := os.Getenv("ADMIN_PASSWORD")
	if p == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return "", errors.New("no terminal to prompt on: set ADMIN_PASSWORD instead")
		}
		fmt.Print("Password (hidden): ")
		a, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		fmt.Print("Repeat password:   ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("the passwords don't match; nothing changed")
		}
		p = string(a)
	}
	switch {
	case len(p) < minLen:
		return "", fmt.Errorf("use at least %d characters; nothing changed", minLen)
	case len(p) > 72:
		return "", errors.New("at most 72 bytes (bcrypt's limit); nothing changed")
	}
	return p, nil
}
