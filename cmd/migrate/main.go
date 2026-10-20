// Command migrate applies the SQL migrations in migrations/ with goose.
//
//	DATABASE_URL=postgres://... migrate up      apply every pending migration
//	DATABASE_URL=postgres://... migrate status  list applied and pending ones
//
// Migrations 001-006 predate goose and are safe to re-run, so a database set
// up by hand is brought under goose by running "up" once.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/pressly/goose/v3"

	"github.com/nasa-spaceapps/nsac-qr/internal/repository/postgres"
	"github.com/nasa-spaceapps/nsac-qr/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("migrate failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
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

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	switch cmd {
	case "up", "status", "version":
		return goose.RunContext(ctx, cmd, db, ".")
	default:
		return fmt.Errorf("unknown command %q (use up, status or version)", cmd)
	}
}
