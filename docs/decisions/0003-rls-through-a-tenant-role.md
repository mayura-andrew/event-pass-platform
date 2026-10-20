# 3. Row Level Security through SET LOCAL ROLE nsac_tenant

Date: 2026-10-20 · Status: accepted

## Context

The spec's second isolation wall is RLS on every tenant table with
`org_id = current_setting('app.org_id')`, and an application database role
without BYPASSRLS. Today the API connects to Supabase as the tables' owner,
which RLS ignores, and the current build depends on that connection.

## Decision

- Migration 007 creates `nsac_tenant` (NOLOGIN, NOBYPASSRLS), grants it to
  the migrating user, and gives it rights on tenant tables only (plus read on
  `accounts`). Policies are written `TO nsac_tenant`.
- Every tenant query runs in `withTenant`: a transaction that does
  `SET LOCAL ROLE nsac_tenant` and sets `app.org_id` and `app.account_id`
  with `set_config(..., true)`. RLS applies there even though the connection
  is the owner, and nothing outlives the transaction (safe with poolers).
- Pre-tenant lookups by secret token (sessions, sign-in links, invite tokens)
  run as the connecting user, outside RLS: the token is the credential.
- Wall 1: repository functions for tenant data take `authz.OrgScope`, built
  only from a membership looked up for the signed-in account.

## Consequences

Switching the whole app to a non-owner login role (migration step 5) becomes
a configuration change rather than a rewrite. Tests prove the wall at three
levels: RLS tests with no WHERE clause, repository tests with foreign IDs,
and the HTTP isolation suite over every org route. To verify on Supabase
staging: that its `postgres` role can `GRANT nsac_tenant TO CURRENT_USER`
and `SET ROLE` to it (CREATEROLE rules changed in PostgreSQL 16).
