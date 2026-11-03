// Package postgres implements the repositories on Supabase PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// DefaultMaxConns fits Supabase's session pooler on small plans with room to
// spare: in session mode every open client connection holds one database
// connection from the project's pool (Database Settings > Connection pooling
// > Pool Size), and migrations, the SQL editor, and dashboards need some.
const DefaultMaxConns = 10

// Open connects through pgx's database/sql driver with up to maxConns
// connections (0 = DefaultMaxConns).
//
// Supabase connection notes:
//   - Session pooler (port 5432): use this. maxConns must stay below the
//     project's pool size, or requests wait for "max clients reached".
//   - Transaction pooler (port 6543) does not support prepared statements;
//     append "default_query_exec_mode=simple_protocol" to DATABASE_URL there.
//   - Direct connection (db.<ref>.supabase.co) is IPv6-only.
func Open(ctx context.Context, dsn string, maxConns ...int) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	n := DefaultMaxConns
	if len(maxConns) > 0 && maxConns[0] > 0 {
		n = maxConns[0]
	}
	db.SetMaxOpenConns(n)
	// Idle connections still hold pooler slots in session mode; keep half
	// warm and drop them after 5 idle minutes.
	db.SetMaxIdleConns(max(1, n/2))
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// PostgreSQL error codes this package reacts to.
const (
	pgUniqueViolation      = "23505"
	pgForeignKeyViolation  = "23503"
	pgInvalidTextRepr      = "22P02"
	pgDeadlockDetected     = "40P01"
	pgSerializationFailure = "40001"
	pgLockNotAvailable     = "55P03"
	pgQueryCanceled        = "57014" // statement_timeout or cancelled request
	pgTooManyConnections   = "53300"
)

// pgConstraint is the name of the constraint an error violated, or "".
func pgConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}
