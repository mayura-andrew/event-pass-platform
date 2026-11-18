-- +goose Up
-- =============================================================================
-- Migration 018: security review fixes (phases 2-5)
-- =============================================================================
-- 1. rate_limits: shared fixed-window counters, so login and registration
--    limits hold across API replicas (F3).
-- 2. event_participants.pass_generation: signed into each QR code; reissuing
--    a pass bumps it and old QR codes stop scanning (F4).
-- 3. sessions.last_seen_at: idle timeout and "where you're signed in" (F7).
-- 4. pass_claim_declines: registrations made with someone's email are no
--    longer linked silently; they confirm or decline each one (F5).
-- 5. event_pass_worker: the background jobs' role, without the sign-in
--    tables the API login can read (F8).
--
-- Safe to re-run.

-- 1. Rate limits. UNLOGGED: a crash only resets the current windows.
CREATE UNLOGGED TABLE IF NOT EXISTS rate_limits (
    bucket       VARCHAR(40)  NOT NULL,
    key          VARCHAR(100) NOT NULL,
    window_start TIMESTAMPTZ  NOT NULL,
    hits         INTEGER      NOT NULL DEFAULT 1,
    PRIMARY KEY (bucket, key, window_start)
);
CREATE INDEX IF NOT EXISTS idx_rate_limits_window ON rate_limits (window_start);
GRANT SELECT, INSERT, UPDATE, DELETE ON rate_limits TO event_pass_api;

-- 2. Pass generations.
ALTER TABLE event_participants ADD COLUMN IF NOT EXISTS pass_generation SMALLINT NOT NULL DEFAULT 1;
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_event_participants_pass_generation') THEN
        ALTER TABLE event_participants ADD CONSTRAINT chk_event_participants_pass_generation
            CHECK (pass_generation >= 1);
    END IF;
END $$;
-- +goose StatementEnd

-- 3. Session activity.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
CREATE INDEX IF NOT EXISTS idx_sessions_account_live ON sessions (account_id, last_seen_at DESC) WHERE revoked_at IS NULL;

-- 4. Declined claims ("Not me").
CREATE TABLE IF NOT EXISTS pass_claim_declines (
    participant_id UUID NOT NULL REFERENCES event_participants(id) ON DELETE CASCADE,
    account_id     UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (participant_id, account_id)
);
-- Claims are found by email across orgs.
CREATE INDEX IF NOT EXISTS idx_event_participants_unlinked_email
    ON event_participants (email) WHERE account_id IS NULL;

-- 5. Worker role: like event_pass_api (NOINHERIT, may SET ROLE to tenant and
-- system) but with no direct grants, so it can't read sessions or links.
--   CREATE ROLE event_pass_jobs LOGIN PASSWORD '...' IN ROLE event_pass_worker;
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'event_pass_worker') THEN
        CREATE ROLE event_pass_worker NOLOGIN NOINHERIT NOBYPASSRLS;
    END IF;
END $$;
-- +goose StatementEnd
GRANT event_pass_tenant, event_pass_system TO event_pass_worker;
GRANT USAGE ON SCHEMA public TO event_pass_worker;

-- Forced RLS and the system policy on the new tables (and any table added
-- since 017), plus the API's own policy on rate_limits.
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
END $$;
-- +goose StatementEnd
DROP POLICY IF EXISTS api_rate_limits ON rate_limits;
CREATE POLICY api_rate_limits ON rate_limits FOR ALL TO event_pass_api USING (true) WITH CHECK (true);

-- The tenant role reads pass_generation with the rest of the row (its
-- existing table grants cover new columns).
