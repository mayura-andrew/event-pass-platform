// Command legacy-import copies the current build's confirmed delegates into
// an event of an organization (SaaS migration: "existing participants ->
// first event of the default org").
//
//	export DATABASE_URL='postgres://...'
//	go run ./cmd/legacy-import -org colombo-space -owner lead@example.org -event "Space Apps Colombo 2025"
//	go run ./cmd/legacy-import -org colombo-space -owner lead@example.org -event-id <uuid>   # into an existing event
//
// The owner must have signed in once and be an owner or admin of the org.
// Each delegate keeps their UUID, membership ID (as pass ID) and QR
// signature, so the QR codes already sent keep working. Nobody is emailed.
// Safe to run again: people already imported are skipped.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
)

func main() {
	if err := run(); err != nil {
		slog.Error("legacy import failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	org := flag.String("org", "", "organization address (the slug in /o/{slug})")
	owner := flag.String("owner", "", "email of an owner or admin of the org; owns a new event")
	name := flag.String("event", "", "name for a new event")
	eventID := flag.String("event-id", "", "import into this existing event instead")
	yes := flag.Bool("yes", false, "don't ask for confirmation")
	flag.Parse()
	if *org == "" || *owner == "" || (*name == "") == (*eventID == "") {
		flag.Usage()
		return fmt.Errorf("set -org, -owner and one of -event or -event-id")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	ctx := context.Background()
	db, err := postgres.Open(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer db.Close()

	var dbName string
	var delegates int
	db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName)
	db.QueryRowContext(ctx, `SELECT count(*) FROM participants WHERE is_confirmed`).Scan(&delegates)
	target := "a new event " + fmt.Sprintf("%q", *name)
	if *eventID != "" {
		target = "event " + *eventID
	}
	fmt.Printf("Database %s: copy %d confirmed delegates into %s of /o/%s.\n", dbName, delegates, target, *org)
	if !*yes {
		fmt.Print("Continue? [y/N] ")
		answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return fmt.Errorf("cancelled")
		}
	}
	res, err := postgres.ImportLegacy(ctx, db, *org, *owner, *name, *eventID)
	if err != nil {
		return err
	}
	verb := "Into existing event"
	if res.Created {
		verb = "Created event"
	}
	fmt.Printf("%s /o/%s/events/%s (%s)\nImported %d delegates, skipped %d already there; added %d questions.\n",
		verb, *org, res.EventID, res.EventSlug, res.Imported, res.Skipped, res.Fields)
	return nil
}
