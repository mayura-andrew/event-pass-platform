-- +goose Up
-- Applied as one statement: every migration up to 006 was written to be
-- re-run safely by hand, and goose runs it once more on databases set up
-- before goose (see docs/decisions/0002-goose-migrations.md).
-- +goose StatementBegin
-- =============================================================================
-- NASA Space Apps Challenge: admin dashboard additions
-- Run after 001_schema.sql. Every statement is idempotent, so re-running is safe.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1. Participant date of birth (minor safeguarding)
-- -----------------------------------------------------------------------------
-- Nullable: older imports and forms without the question leave it empty, and
-- the dashboard reports those delegates as "date of birth missing" rather than
-- guessing an age group.
ALTER TABLE participants ADD COLUMN IF NOT EXISTS date_of_birth DATE;

-- Rejects obvious typos (year 0202, a birth date in the future). NOT VALID
-- skips checking rows that already exist, so the migration can't fail on old
-- data; new and updated rows are checked.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_participants_dob_range') THEN
        ALTER TABLE participants
            ADD CONSTRAINT chk_participants_dob_range
            CHECK (date_of_birth IS NULL OR date_of_birth BETWEEN DATE '1900-01-01' AND CURRENT_DATE)
            NOT VALID;
    END IF;
END $$;

-- Age filters compare date_of_birth with a single cut-off date, so a plain
-- b-tree index serves both "under 18" (> cut-off) and "18+" (<= cut-off).
CREATE INDEX IF NOT EXISTS idx_participants_dob
    ON participants (date_of_birth);

CREATE INDEX IF NOT EXISTS idx_participants_venue
    ON participants (assigned_venue_id);

-- -----------------------------------------------------------------------------
-- 2. Age helpers for ad-hoc queries in the Supabase SQL editor
-- -----------------------------------------------------------------------------
-- The Go repository writes the same predicates inline, as the spec defines them:
--   Under 18: date_of_birth >  (CURRENT_DATE - INTERVAL '18 years')
--   18+:      date_of_birth <= (CURRENT_DATE - INTERVAL '18 years')
-- CURRENT_DATE is evaluated in the session time zone (UTC on Supabase).

-- Returns 'UNDER_18', 'ADULT' or 'UNKNOWN'. STABLE, not IMMUTABLE, because the
-- answer changes as CURRENT_DATE moves.
CREATE OR REPLACE FUNCTION participant_age_group(dob DATE)
RETURNS TEXT
LANGUAGE sql
STABLE
AS $$
    SELECT CASE
        WHEN dob IS NULL THEN 'UNKNOWN'
        WHEN dob > (CURRENT_DATE - INTERVAL '18 years') THEN 'UNDER_18'
        ELSE 'ADULT'
    END
$$;

-- One row per participant with computed age. security_invoker makes the view
-- respect the caller's RLS (PostgreSQL 15+, which Supabase runs), so it does
-- not reopen the tables that 001_schema.sql locked down.
CREATE OR REPLACE VIEW v_participant_ages
WITH (security_invoker = true) AS
SELECT
    p.id,
    p.membership_id,
    p.full_name,
    p.team_name,
    p.is_confirmed,
    p.assigned_venue_id,
    p.date_of_birth,
    CASE WHEN p.date_of_birth IS NULL THEN NULL
         ELSE date_part('year', age(CURRENT_DATE, p.date_of_birth))::int
    END                                    AS age_years,
    participant_age_group(p.date_of_birth) AS age_group
FROM participants p;

-- -----------------------------------------------------------------------------
-- 3. Scan provenance and manual override audit
-- -----------------------------------------------------------------------------
-- entry_method tells the audit log how a scan was made:
--   QR               scanned from a pass on a volunteer phone (existing rows)
--   MANUAL_OVERRIDE  recorded from the dashboard without a QR (dead phone, etc.)
ALTER TABLE scans ADD COLUMN IF NOT EXISTS entry_method VARCHAR(20) NOT NULL DEFAULT 'QR';
ALTER TABLE scans ADD COLUMN IF NOT EXISTS override_reason TEXT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_scans_entry_method') THEN
        ALTER TABLE scans
            ADD CONSTRAINT chk_scans_entry_method
            CHECK (entry_method IN ('QR', 'MANUAL_OVERRIDE'));
    END IF;
    -- An override without a written reason is not auditable, so the database
    -- refuses it even if a future code path forgets to validate.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_scans_override_reason') THEN
        ALTER TABLE scans
            ADD CONSTRAINT chk_scans_override_reason
            CHECK (entry_method <> 'MANUAL_OVERRIDE' OR length(btrim(override_reason)) >= 10);
    END IF;
END $$;

-- The live feed and throughput chart read the newest scans by server time.
CREATE INDEX IF NOT EXISTS idx_scans_synced_at
    ON scans (synced_at DESC);

-- Claim lookups for the dashboard table ("claimed for scenario X?").
CREATE INDEX IF NOT EXISTS idx_scans_scenario_valid
    ON scans (scenario_id, participant_id)
    WHERE status = 'VALID';

-- -----------------------------------------------------------------------------
-- 4. Scanner device heartbeats (sync queue health)
-- -----------------------------------------------------------------------------
-- Scans waiting in a phone's IndexedDB are invisible to the server until they
-- upload, so each scanner reports its queue counts whenever it is online. A
-- device that stops reporting is itself the warning sign: its last_seen_at
-- ages and the dashboard flags it.
CREATE TABLE IF NOT EXISTS scanner_devices (
    device_id        UUID PRIMARY KEY,                 -- generated once per browser
    admin_id         UUID NOT NULL REFERENCES admins(id) ON DELETE CASCADE,
    scenario_id      UUID REFERENCES scan_scenarios(id) ON DELETE SET NULL,
    pending_count    INTEGER NOT NULL DEFAULT 0 CHECK (pending_count  >= 0),
    -- Pending scans made by a different volunteer who used this phone earlier;
    -- they upload only when that volunteer signs in here again.
    pending_others   INTEGER NOT NULL DEFAULT 0 CHECK (pending_others >= 0),
    rejected_count   INTEGER NOT NULL DEFAULT 0 CHECK (rejected_count >= 0),
    invalid_count    INTEGER NOT NULL DEFAULT 0 CHECK (invalid_count  >= 0),
    manifest_count   INTEGER NOT NULL DEFAULT 0 CHECK (manifest_count >= 0),
    last_sync_at     TIMESTAMPTZ,                      -- device clock
    is_online        BOOLEAN NOT NULL DEFAULT true,    -- navigator.onLine at send time
    user_agent       VARCHAR(300),
    first_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW() -- server clock
);

CREATE INDEX IF NOT EXISTS idx_scanner_devices_last_seen
    ON scanner_devices (last_seen_at DESC);

ALTER TABLE scanner_devices ENABLE ROW LEVEL SECURITY;

-- +goose StatementEnd
