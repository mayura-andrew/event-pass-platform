-- +goose Up
-- =============================================================================
-- Migration 017: database roles and forced Row Level Security
-- =============================================================================
-- Before this migration the API connected as the tables' owner. PostgreSQL
-- skips Row Level Security for the owner, so any query that did not switch to
-- event_pass_tenant (sign-in, the outbox, integrations, public lookups) could
-- read and write every organization, and a query that forgot withTenant would
-- too.
--
-- Roles after this migration (see docs/decisions/0010-database-roles.md):
--
--   owner (the migrator)  owns the tables and runs migrations. The API must
--                         not connect as it (APP_ENV=production refuses to).
--   event_pass_api        NOLOGIN NOINHERIT group for the API's login user.
--                         Directly it may only touch the pre-tenant sign-in
--                         tables (accounts, auth_identities, sessions,
--                         login_links). Everything else needs SET LOCAL ROLE.
--   event_pass_tenant     (migration 007) one org / one account, via
--                         withTenant.
--   event_pass_system     cross-tenant work that must stay privileged: the
--                         outbox and integration workers, token lookups
--                         (invite, view link, public page), health. Reached
--                         only through withSystem, which the repository
--                         allowlist test keeps in a few named places.
--
-- FORCE ROW LEVEL SECURITY makes the policies apply to the owner as well, so
-- even an owner connection sees nothing outside withTenant / withSystem.
--
-- Production login (run once as the owner, with a strong password):
--
--   CREATE ROLE event_pass_app LOGIN PASSWORD '...' IN ROLE event_pass_api;
--
-- then point the API's DATABASE_URL at event_pass_app and keep the owner's
-- URL for event-pass-migrate only.
--
-- Future migrations that change data must SET LOCAL ROLE event_pass_system
-- first: with FORCE, the owner's own statements see no rows.
--
-- Safe to re-run.

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'event_pass_system') THEN
        CREATE ROLE event_pass_system NOLOGIN NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'event_pass_api') THEN
        -- NOINHERIT: membership lets the login SET ROLE to the tenant and
        -- system roles, but their privileges never apply to a bare query.
        CREATE ROLE event_pass_api NOLOGIN NOINHERIT NOBYPASSRLS;
    END IF;
END $$;
-- +goose StatementEnd

GRANT event_pass_tenant, event_pass_system TO event_pass_api;
-- The owner keeps both so local development (and the tests) can still
-- connect as it; FORCE below means it gains nothing by skipping them.
GRANT event_pass_system TO CURRENT_USER;

GRANT USAGE ON SCHEMA public TO event_pass_api, event_pass_tenant, event_pass_system;
GRANT EXECUTE ON FUNCTION app_org_id(), app_account_id() TO event_pass_system;
GRANT EXECUTE ON FUNCTION add_default_checkpoints(UUID, UUID) TO event_pass_system;

-- The org-owner guard used to be SECURITY DEFINER (run as the owner, outside
-- RLS). With FORCE the owner sees no rows, so run it as the caller: a tenant
-- sees its own org's members, and system sees all.
ALTER FUNCTION keep_one_org_owner() SECURITY INVOKER;

-- Pre-tenant sign-in tables: the API login reads and writes them directly.
GRANT SELECT, INSERT, UPDATE ON accounts, sessions, login_links TO event_pass_api;
GRANT SELECT, INSERT ON auth_identities TO event_pass_api;

-- +goose StatementBegin
DO $$
DECLARE
    t text;
BEGIN
    FOR t IN
        SELECT tablename FROM pg_tables
        WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO event_pass_system', t);
        EXECUTE format('DROP POLICY IF EXISTS system_all ON %I', t);
        EXECUTE format('CREATE POLICY system_all ON %I FOR ALL TO event_pass_system USING (true) WITH CHECK (true)', t);
    END LOOP;

    FOREACH t IN ARRAY ARRAY['accounts', 'auth_identities', 'sessions', 'login_links'] LOOP
        EXECUTE format('DROP POLICY IF EXISTS api_signin ON %I', t);
        EXECUTE format('CREATE POLICY api_signin ON %I FOR ALL TO event_pass_api USING (true) WITH CHECK (true)', t);
    END LOOP;
END $$;
-- +goose StatementEnd

GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO event_pass_system;
