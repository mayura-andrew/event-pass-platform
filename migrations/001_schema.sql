-- =============================================================================
-- NASA Space Apps Challenge: Offline-first QR delegate management
-- Supabase / PostgreSQL schema (run once in the Supabase SQL editor or psql)
-- =============================================================================

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- -----------------------------------------------------------------------------
-- 1. Admins (scanner operators and dashboard users)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS admins (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    email         VARCHAR(255) UNIQUE NOT NULL,          -- stored lower-cased
    password_hash VARCHAR(255) NOT NULL,                 -- bcrypt
    full_name     VARCHAR(100) NOT NULL,
    role          VARCHAR(50)  NOT NULL DEFAULT 'admin'
                  CHECK (role IN ('superadmin', 'admin', 'volunteer')),
    is_active     BOOLEAN      NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- -----------------------------------------------------------------------------
-- 2. Venues (Google Maps location mapping)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS venues (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name         VARCHAR(150)   NOT NULL,
    hall_section VARCHAR(100)   NOT NULL,
    latitude     NUMERIC(10, 8) NOT NULL CHECK (latitude  BETWEEN -90  AND 90),
    longitude    NUMERIC(11, 8) NOT NULL CHECK (longitude BETWEEN -180 AND 180),
    description  TEXT,
    created_at   TIMESTAMPTZ    NOT NULL DEFAULT NOW(),
    -- Lets the seed script upsert venues by their human-readable identity.
    CONSTRAINT uq_venue_name_section UNIQUE (name, hall_section)
);

-- -----------------------------------------------------------------------------
-- 3. Participants
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS participants (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    membership_id     VARCHAR(100) UNIQUE NOT NULL,
    email             VARCHAR(255) UNIQUE NOT NULL,      -- stored lower-cased
    full_name         VARCHAR(150) NOT NULL,
    team_name         VARCHAR(150),
    is_confirmed      BOOLEAN      NOT NULL DEFAULT false,
    qr_signature      VARCHAR(255) NOT NULL,             -- hex HMAC-SHA256(membership_id:email)
    assigned_venue_id UUID REFERENCES venues(id) ON DELETE SET NULL,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- -----------------------------------------------------------------------------
-- 4. Scan scenarios (managed from the admin API)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS scan_scenarios (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name             VARCHAR(100) NOT NULL,              -- "Day 1 Lunch"
    code             VARCHAR(50)  UNIQUE NOT NULL,       -- "D1_LUNCH"
    description      TEXT,
    is_active        BOOLEAN      NOT NULL DEFAULT true,
    allow_duplicates BOOLEAN      NOT NULL DEFAULT false,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- -----------------------------------------------------------------------------
-- 5. Scans (audit log + claim ledger)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS scans (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- Single-claim scenarios: {admin_id}_{participant_id}_{scenario_id}
    -- Repeatable scenarios:   {admin_id}_{participant_id}_{scenario_id}_{scan_uuid}
    idempotency_key     VARCHAR(255) UNIQUE NOT NULL,
    participant_id      UUID NOT NULL REFERENCES participants(id)   ON DELETE CASCADE,
    scenario_id         UUID NOT NULL REFERENCES scan_scenarios(id) ON DELETE RESTRICT,
    scanned_by_admin_id UUID NOT NULL REFERENCES admins(id)         ON DELETE RESTRICT,
    scanned_at_device   TIMESTAMPTZ NOT NULL,                       -- device clock
    synced_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),         -- server clock
    status              VARCHAR(30) NOT NULL DEFAULT 'VALID'
                        CHECK (status IN ('VALID', 'DUPLICATE_REJECTED')),
    -- Copied from scan_scenarios.allow_duplicates at insert time (inverted).
    -- A partial index cannot reference another table, so without this column
    -- the unique claim rule below would also block repeatable scenarios.
    enforce_unique      BOOLEAN NOT NULL DEFAULT true
);

-- -----------------------------------------------------------------------------
-- Indexes
-- -----------------------------------------------------------------------------
-- participants.membership_id, participants.email and scans.idempotency_key are
-- already indexed by their UNIQUE constraints; a second plain index on the same
-- column would only add write cost, so none is created here.

CREATE INDEX IF NOT EXISTS idx_participants_confirmed
    ON participants (is_confirmed) WHERE is_confirmed;

CREATE INDEX IF NOT EXISTS idx_scans_participant_scenario
    ON scans (participant_id, scenario_id);

-- "First scan wins": at most one VALID claim per participant per single-claim
-- scenario. Concurrent syncs from different devices serialize on this index.
CREATE UNIQUE INDEX IF NOT EXISTS idx_unique_valid_claim
    ON scans (participant_id, scenario_id)
    WHERE status = 'VALID' AND enforce_unique;

-- -----------------------------------------------------------------------------
-- Supabase hardening
-- -----------------------------------------------------------------------------
-- Tables in the public schema are reachable through Supabase's auto-generated
-- REST API with the anon key. Enabling RLS with no policies blocks that path
-- entirely; the Go API connects as the postgres role, which bypasses RLS.
ALTER TABLE admins         ENABLE ROW LEVEL SECURITY;
ALTER TABLE venues         ENABLE ROW LEVEL SECURITY;
ALTER TABLE participants   ENABLE ROW LEVEL SECURITY;
ALTER TABLE scan_scenarios ENABLE ROW LEVEL SECURITY;
ALTER TABLE scans          ENABLE ROW LEVEL SECURITY;

-- -----------------------------------------------------------------------------
-- Starter scenarios (safe to re-run)
-- -----------------------------------------------------------------------------
INSERT INTO scan_scenarios (name, code, description, allow_duplicates) VALUES
    ('Main gate check-in', 'CHECK_IN',  'First entry to the venue',          false),
    ('Day 1 lunch',        'D1_LUNCH',  'One lunch per delegate on day 1',   false),
    ('Swag kit pickup',    'SWAG_KIT',  'One kit per delegate',              false),
    ('Hall re-entry',      'RE_ENTRY',  'Repeatable entry after check-in',   true)
ON CONFLICT (code) DO NOTHING;
