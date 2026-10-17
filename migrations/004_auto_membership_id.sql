-- =============================================================================
-- NASA Space Apps Challenge: auto-generated membership IDs (NSAC-001000, ...)
-- Run after 003_rbac_and_emailing.sql. Safe to re-run.
-- =============================================================================
-- Delegates added from the dashboard (one at a time or by CSV upload) get
-- their membership ID from this sequence. The API reserves numbers with
-- nextval() before inserting, because it needs the ID to compute the QR
-- signature; the column default below covers rows inserted any other way
-- (SQL editor), which still need a qr_signature supplied by hand.
--
-- IDs imported earlier by scripts/seed keep their own format (NSAC-1092):
-- the generated ones always have six digits, so the two never collide.

-- -----------------------------------------------------------------------------
-- 1. Sequence
-- -----------------------------------------------------------------------------
-- MAXVALUE keeps every ID at six digits: LPAD truncates longer input, so
-- 1000000 would come out as '100000' and clash with an existing ID. Running
-- out raises an error instead.
CREATE SEQUENCE IF NOT EXISTS participant_membership_seq
    AS integer
    START WITH 1000
    MINVALUE 1000
    MAXVALUE 999999
    NO CYCLE;

-- Never hand out an ID that is already taken: move the sequence past any
-- six-digit NSAC- ID already stored (one typed by hand, or a restored dump).
-- It only ever moves forward, so re-running this file is harmless.
SELECT setval(
    'participant_membership_seq',
    GREATEST(
        1000,
        COALESCE((SELECT MAX(substring(membership_id FROM '^NSAC-([0-9]{6})$')::integer) + 1
                  FROM participants), 0),
        (SELECT CASE WHEN is_called THEN last_value + 1 ELSE last_value END
         FROM participant_membership_seq)
    ),
    false);

-- -----------------------------------------------------------------------------
-- 2. Column default
-- -----------------------------------------------------------------------------
ALTER TABLE participants
    ALTER COLUMN membership_id
    SET DEFAULT 'NSAC-' || LPAD(nextval('participant_membership_seq')::TEXT, 6, '0');

-- Dropping the column (or table) drops the sequence with it.
ALTER SEQUENCE participant_membership_seq OWNED BY participants.membership_id;

-- -----------------------------------------------------------------------------
-- 3. Supabase hardening
-- -----------------------------------------------------------------------------
-- Supabase grants its API roles access to new objects in the public schema.
-- Calling nextval() through the REST API would burn membership IDs, so take
-- that away; the Go API connects as postgres and is unaffected. The roles
-- don't exist on a plain PostgreSQL (local Docker), hence the check.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'anon') THEN
        REVOKE ALL ON SEQUENCE participant_membership_seq FROM anon;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'authenticated') THEN
        REVOKE ALL ON SEQUENCE participant_membership_seq FROM authenticated;
    END IF;
END $$;
