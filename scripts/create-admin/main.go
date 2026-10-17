// Command create-admin adds (or resets the password of) a scanner/dashboard user.
//
//	export DATABASE_URL=postgres://...
//	ADMIN_PASSWORD='...' go run ./scripts/create-admin -email gate1@spaceapps.org -name "Gate 1" -role scanner
//
// The password is read from ADMIN_PASSWORD (or stdin) so it never lands in
// shell history as a flag.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	email := flag.String("email", "", "admin email (required)")
	name := flag.String("name", "", "full name (required)")
	role := flag.String("role", "scanner", "superadmin | admin | scanner")
	flag.Parse()

	if *email == "" || *name == "" {
		flag.Usage()
		os.Exit(2)
	}
	switch *role {
	case "superadmin", "admin", "scanner":
	default:
		log.Fatalf("invalid role %q", *role)
	}

	password := os.Getenv("ADMIN_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "Password: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		password = strings.TrimRight(line, "\r\n")
	}
	if len(password) < 10 {
		log.Fatal("password must be at least 10 characters")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var id string
	err = db.QueryRowContext(ctx, `
		INSERT INTO admins (email, password_hash, full_name, role)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (email) DO UPDATE SET
			password_hash = EXCLUDED.password_hash,
			full_name = EXCLUDED.full_name,
			role = EXCLUDED.role,
			is_active = true,
			-- A password set here is the user's own, not a temporary one.
			force_password_change = false,
			password_changed_at = NOW()
		RETURNING id`,
		strings.ToLower(strings.TrimSpace(*email)), string(hash), *name, *role,
	).Scan(&id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("admin %s saved (id %s, role %s)\n", *email, id, *role)
	if *role == "scanner" {
		fmt.Println("note: scanner accounts only see checkpoints granted to them; assign them on the dashboard's Staff page")
	}
}
