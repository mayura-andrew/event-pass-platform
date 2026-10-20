# 2. Versioned migrations with goose, starting from the current files

Date: 2026-10-20 · Status: accepted

## Context

Migrations 001-006 are applied by hand in the Supabase SQL editor (and by
Docker's init scripts locally). Each is written to be re-run safely. The
spec asks for goose.

## Decision

- Keep the files in `migrations/` with their names. Each of 001-006 gets a
  `-- +goose Up` header and runs as one statement, which works because none
  uses explicit transactions or statements that can't run in one.
- `cmd/migrate` (also in the Docker image as `/app/nsac-qr-migrate`) runs
  `goose up` with the files embedded. On a database set up by hand, the first
  run re-applies 001-006 (harmless by design) and records them.
- goose is pinned to v3.22.1: later releases require Go 1.26 and upgrade pgx
  and x/crypto, which the Dockerfile's Go 1.22 image can't build.
- psql still runs the files unchanged (the goose annotations are comments),
  so Docker's init scripts and the SQL editor keep working.

## Consequences

New migrations must be goose-annotated (`StatementBegin`/`StatementEnd`
around `DO $$` blocks and functions). Down migrations are not written; roll
forward instead.
