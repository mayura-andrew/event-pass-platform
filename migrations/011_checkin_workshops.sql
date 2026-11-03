-- +goose Up
-- =============================================================================
-- Migration 011: workshops, checkpoints and check-ins (SaaS milestone M4)
-- =============================================================================
-- One pass per participant works at every workshop of the event. Staff record
-- checkpoints by scanning: confirm participation, check in, one per workshop,
-- plus custom ones (lunch, kit), each with rules (prerequisites, once or
-- repeatable, a time window, which roles may record it).
--
-- check-ins are append-only: an undo sets undone_at, nothing is deleted.
-- "Once" checkpoints are enforced by a partial unique index, so two phones
-- scanning the same person at the same moment can't both record it.
--
-- Safe to re-run.

-- -----------------------------------------------------------------------------
-- 1. Workshops and sign-ups
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS workshops (
    id          UUID PRIMARY KEY,
    org_id      UUID NOT NULL,
    event_id    UUID NOT NULL,
    title       VARCHAR(150) NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    speakers    VARCHAR(300) NOT NULL DEFAULT '',
    room        VARCHAR(100) NOT NULL DEFAULT '',
    starts_at   TIMESTAMPTZ,
    ends_at     TIMESTAMPTZ,
    capacity    INTEGER CHECK (capacity > 0),
    -- everyone: all participants may attend; opt_in: they sign up in the
    -- portal; invite: organizers add people.
    signup_mode VARCHAR(20) NOT NULL DEFAULT 'opt_in' CHECK (signup_mode IN ('everyone', 'opt_in', 'invite')),
    waitlist    BOOLEAN NOT NULL DEFAULT true,
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at >= starts_at),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    UNIQUE (org_id, id)
);
CREATE INDEX IF NOT EXISTS idx_workshops_event ON workshops (event_id, starts_at);

CREATE TABLE IF NOT EXISTS workshop_signups (
    org_id         UUID NOT NULL,
    workshop_id    UUID NOT NULL,
    participant_id UUID NOT NULL,
    status         VARCHAR(20) NOT NULL DEFAULT 'signed_up' CHECK (status IN ('signed_up', 'waitlisted', 'cancelled')),
    -- Waitlist order; also when they signed up.
    position       BIGINT NOT NULL DEFAULT (extract(epoch FROM clock_timestamp()) * 1000000)::bigint,
    added_by       UUID REFERENCES accounts(id) ON DELETE SET NULL,  -- staff who added them, if any
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workshop_id, participant_id),
    FOREIGN KEY (org_id, workshop_id) REFERENCES workshops (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, participant_id) REFERENCES event_participants (org_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_workshop_signups_participant ON workshop_signups (participant_id);

-- -----------------------------------------------------------------------------
-- 2. Checkpoints
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS checkpoints (
    id          UUID PRIMARY KEY,
    org_id      UUID NOT NULL,
    event_id    UUID NOT NULL,
    workshop_id UUID,                      -- the workshop's attendance checkpoint
    name        VARCHAR(100) NOT NULL,
    kind        VARCHAR(20) NOT NULL CHECK (kind IN ('confirm', 'check_in', 'workshop', 'custom')),
    -- {"requires": [checkpoint ids], "repeatable": bool, "opens_at", "closes_at",
    --  "roles": ["owner", "editor", "checkin"]}
    rules       JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(rules) = 'object'),
    position    INTEGER NOT NULL DEFAULT 0,
    archived_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, workshop_id) REFERENCES workshops (org_id, id) ON DELETE CASCADE,
    UNIQUE (org_id, id)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_event ON checkpoints (event_id, position);
CREATE UNIQUE INDEX IF NOT EXISTS uq_checkpoints_workshop ON checkpoints (workshop_id) WHERE workshop_id IS NOT NULL;

-- -----------------------------------------------------------------------------
-- 3. Check-ins (append-only)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS checkins (
    id               UUID PRIMARY KEY,
    org_id           UUID NOT NULL,
    event_id         UUID NOT NULL,
    participant_id   UUID NOT NULL,
    checkpoint_id    UUID NOT NULL,
    -- The device's own ID for this scan: retries return the first result.
    idempotency_key  VARCHAR(80) NOT NULL,
    staff_account_id UUID REFERENCES accounts(id) ON DELETE SET NULL,
    device_id        VARCHAR(80) NOT NULL DEFAULT '',
    client_time      TIMESTAMPTZ,
    -- Copied from the checkpoint at insert (not repeatable): the partial
    -- index below can't look at another table.
    once             BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    undone_at        TIMESTAMPTZ,
    undone_by        UUID REFERENCES accounts(id) ON DELETE SET NULL,
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, participant_id) REFERENCES event_participants (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, checkpoint_id) REFERENCES checkpoints (org_id, id) ON DELETE CASCADE,
    UNIQUE (event_id, idempotency_key)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_checkins_once
    ON checkins (checkpoint_id, participant_id) WHERE once AND undone_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_checkins_event_time ON checkins (event_id, created_at);
CREATE INDEX IF NOT EXISTS idx_checkins_participant ON checkins (participant_id, checkpoint_id);

-- -----------------------------------------------------------------------------
-- 4. Default checkpoints: the current build's confirm -> check in -> kit flow
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION add_default_checkpoints(p_org UUID, p_event UUID) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    check_in UUID := gen_random_uuid();
BEGIN
    IF EXISTS (SELECT 1 FROM checkpoints WHERE event_id = p_event) THEN
        RETURN;
    END IF;
    INSERT INTO checkpoints (id, org_id, event_id, name, kind, rules, position) VALUES
        (gen_random_uuid(), p_org, p_event, 'Confirm participation', 'confirm', '{}', 1),
        (check_in,          p_org, p_event, 'Check in',              'check_in', '{}', 2),
        (gen_random_uuid(), p_org, p_event, 'Kit',                   'custom',
         jsonb_build_object('requires', jsonb_build_array(check_in)), 3);
END $$;
-- +goose StatementEnd

-- Existing events get them once; new events get them from the API.
-- +goose StatementBegin
DO $$
DECLARE e RECORD;
BEGIN
    FOR e IN SELECT org_id, id FROM events LOOP
        PERFORM add_default_checkpoints(e.org_id, e.id);
    END LOOP;
END $$;
-- +goose StatementEnd

-- -----------------------------------------------------------------------------
-- 5. Row Level Security
-- -----------------------------------------------------------------------------
ALTER TABLE workshops        ENABLE ROW LEVEL SECURITY;
ALTER TABLE workshop_signups ENABLE ROW LEVEL SECURITY;
ALTER TABLE checkpoints      ENABLE ROW LEVEL SECURITY;
ALTER TABLE checkins         ENABLE ROW LEVEL SECURITY;

GRANT SELECT, INSERT, UPDATE, DELETE ON workshops, workshop_signups, checkpoints TO event_pass_tenant;
-- Append-only: an undo is an UPDATE of undone_at, never a DELETE.
GRANT SELECT, INSERT, UPDATE (undone_at, undone_by) ON checkins TO event_pass_tenant;
GRANT EXECUTE ON FUNCTION add_default_checkpoints(UUID, UUID) TO event_pass_tenant;

-- Workshops and sign-ups: the current org's; participants also read the
-- workshops of events they're registered for and their own sign-ups (portal).
DROP POLICY IF EXISTS workshops_select ON workshops;
CREATE POLICY workshops_select ON workshops FOR SELECT TO event_pass_tenant
    USING (org_id = app_org_id()
           OR event_id IN (SELECT p.event_id FROM event_participants p WHERE p.account_id = app_account_id()));
DROP POLICY IF EXISTS workshops_write ON workshops;
CREATE POLICY workshops_write ON workshops FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

DROP POLICY IF EXISTS workshop_signups_all ON workshop_signups;
CREATE POLICY workshop_signups_all ON workshop_signups FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

DROP POLICY IF EXISTS checkpoints_all ON checkpoints;
CREATE POLICY checkpoints_all ON checkpoints FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

DROP POLICY IF EXISTS checkins_select ON checkins;
CREATE POLICY checkins_select ON checkins FOR SELECT TO event_pass_tenant USING (org_id = app_org_id());
DROP POLICY IF EXISTS checkins_insert ON checkins;
-- Recorded as the signed-in staff member, never on someone's behalf.
CREATE POLICY checkins_insert ON checkins FOR INSERT TO event_pass_tenant
    WITH CHECK (org_id = app_org_id() AND staff_account_id IS NOT DISTINCT FROM app_account_id());
DROP POLICY IF EXISTS checkins_undo ON checkins;
CREATE POLICY checkins_undo ON checkins FOR UPDATE TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
