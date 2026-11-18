# 10. Non-owner database logins, forced RLS and an explicit system path

Date: 2026-11-18 · Status: accepted · Supersedes the "pre-tenant lookups run
as the connecting user" part of ADR 3

## Context

The security review of 17 Nov 2026 (finding F1) found that RLS applied only
inside `withTenant`. The API connected as the tables' owner, so sign-in,
portal email linking, the outbox, integration jobs and every token lookup
ran outside RLS, and a repository method that forgot `withTenant` would
silently read every organization.

## Decision

Roles (migrations 017 and 018):

| Role | Login? | Direct access | Can `SET LOCAL ROLE` to |
| --- | --- | --- | --- |
| owner (migrator) | yes | owns the tables; used only by `event-pass-migrate` | — |
| `event_pass_api` | group, `NOINHERIT` | `accounts`, `auth_identities`, `sessions`, `login_links`, `rate_limits` | tenant, system |
| `event_pass_worker` | group, `NOINHERIT` | nothing | tenant, system |
| `event_pass_tenant` | no | per-org policies (ADR 3) | — |
| `event_pass_system` | no | `system_all` policy on every table | — |

- The API's login is created `IN ROLE event_pass_api`, the worker's `IN ROLE
  event_pass_worker`. `NOINHERIT` means membership allows `SET ROLE` but the
  tenant and system privileges never apply to a bare query, which fails with
  "permission denied".
- Every table has `FORCE ROW LEVEL SECURITY`, so the owner gets no rows
  either except through the policies.
- `withSystem` (internal/repository/postgres/tenant.go) is the only way to
  `event_pass_system`. Its callers are listed, with a reason each, in
  `systemCallers` in `tenant_allowlist_test.go`, and the same test fails on
  any direct `*sql.DB` query outside `directDBAllowed`. Adding to either list
  is a reviewed security decision.
- With `APP_ENV=production` the API and worker refuse to start when the
  database login is a superuser, has BYPASSRLS, or owns the tables.
- `keep_one_org_owner()` is now `SECURITY INVOKER`: with FORCE, a definer
  function owned by the owner would see no rows.

The worker (`cmd/worker`) runs the outbox, Google sync, retention and the
rate limit sweep with `RUN_WORKERS=false` on the API. The live feed keeps its
poll (ADR 8) and adds `NOTIFY event_pass_feed '<event id>'` as a fast path
across replicas; `LISTEN` silently does nothing through a transaction pooler,
and the poll still covers.

## Consequences

- Production needs three connection strings: the owner for migrations, an
  `event_pass_api` login, an `event_pass_worker` login (deploy/ec2/.env.example).
- Data-changing migrations must `SET LOCAL ROLE event_pass_system` first, or
  their statements see no rows.
- Local development may still connect as the owner (a warning is logged); the
  owner is a member of both roles. Docker's `POSTGRES_USER` is a superuser and
  bypasses RLS entirely, which is why the static allowlist test, not local
  runs, is what catches a forgotten scope. `TestAPILoginCannotSkipTenantScope`
  proves the database side with a real `event_pass_api` login.
