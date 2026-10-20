# 1. Build the SaaS inside the current binary, next to the current build

Date: 2026-10-20 · Status: accepted

## Context

The spec turns the single-event build into a multi-tenant SaaS and says to
keep the current build running for live events until the pilot gate (M6).
It also proposes a new repository layout (`cmd/server`, `internal/<module>`,
`web/`, `db/migrations`).

## Decision

- New SaaS code ships in the same `cmd/api` binary and database. It uses its
  own routes (`/login`, `/app`, `/o/{org}`, `/api/v1/me`, `/api/v1/orgs`...)
  and its own tables (migration 007) and does not touch the current build's
  routes, tables or JWT staff login.
- New code follows the existing layering (`domain`, `service`,
  `repository/postgres`, `handler/http`) plus the spec's `internal/authz`
  package, and pages live in `web/` (templates and static files, embedded).
- The full module layout and `cmd/server` / `cmd/worker` split happen when
  the current build's code moves under it (migration steps 3-5), not before:
  moving files now would only make the live build's history harder to follow.

## Consequences

One deploy carries both products, so every SaaS change still runs the
current build's tests. The SaaS pages are reachable in production as soon as
007 is applied; they are harmless without org data.
