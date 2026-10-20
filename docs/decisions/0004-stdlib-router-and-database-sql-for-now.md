# 4. Keep net/http routing and database/sql for now (not chi and sqlc yet)

Date: 2026-10-20 · Status: proposed, revisit at M2

## Context

The spec lists chi, pgx + sqlc and river. The current build uses the Go 1.22
`ServeMux` (method and wildcard patterns) and hand-written queries through
pgx's `database/sql` driver.

## Decision

For M1, stay with both. `ServeMux` covers every route M1 needs, and sqlc
needs a code-generation step and CI check that would have to cover the
current build's queries too. river is not needed until the worker (M3).

## Consequences

No new framework dependencies in M1 (goose is the only addition). If M2's
events, sharing and audit log make hand-written queries a source of bugs,
adopt sqlc then, starting with the new tables.
