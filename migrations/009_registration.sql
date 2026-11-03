-- +goose Up
-- =============================================================================
-- Migration 009: publishing, registration forms and event participants
-- (SaaS milestone M3)
-- =============================================================================
-- A published event has a public page where people register. Each person's
-- registration is one row in event_participants (same email in two events =
-- two rows, one account).
--
-- Safe to re-run.

-- -----------------------------------------------------------------------------
-- 1. Publishing and registration settings on events
-- -----------------------------------------------------------------------------
ALTER TABLE events ADD COLUMN IF NOT EXISTS published_at TIMESTAMPTZ;
-- open: anyone with the page registers; approval: organizers approve first.
ALTER TABLE events ADD COLUMN IF NOT EXISTS reg_mode VARCHAR(20) NOT NULL DEFAULT 'open';
ALTER TABLE events ADD COLUMN IF NOT EXISTS reg_opens_at TIMESTAMPTZ;
ALTER TABLE events ADD COLUMN IF NOT EXISTS reg_closes_at TIMESTAMPTZ;
ALTER TABLE events ADD COLUMN IF NOT EXISTS waitlist BOOLEAN NOT NULL DEFAULT true;
-- Pass ID prefix, e.g. CSA in CSA-7KQ4-M2XD.
ALTER TABLE events ADD COLUMN IF NOT EXISTS pass_prefix VARCHAR(6) NOT NULL DEFAULT 'EVT';

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'events_reg_mode_check') THEN
        ALTER TABLE events ADD CONSTRAINT events_reg_mode_check CHECK (reg_mode IN ('open', 'approval'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'events_pass_prefix_check') THEN
        ALTER TABLE events ADD CONSTRAINT events_pass_prefix_check CHECK (pass_prefix ~ '^[A-Z0-9]{2,6}$');
    END IF;
END $$;
-- +goose StatementEnd

-- -----------------------------------------------------------------------------
-- 2. Registration form fields
-- -----------------------------------------------------------------------------
-- Name and email are always asked and are not rows here. workshop_id is for
-- a workshop's extra questions (M4).
CREATE TABLE IF NOT EXISTS form_fields (
    id          UUID PRIMARY KEY,
    org_id      UUID NOT NULL,
    event_id    UUID NOT NULL,
    workshop_id UUID,
    key         VARCHAR(60)  NOT NULL CHECK (key ~ '^[a-z][a-z0-9_]{0,59}$'),
    type        VARCHAR(20)  NOT NULL CHECK (type IN ('text', 'long_text', 'email', 'phone', 'number', 'date',
                                                      'select', 'multi_select', 'checkbox', 'consent', 'url')),
    -- {"label", "help", "required", "options": [...], "visibility": "pass|participant|staff|admin"}
    config      JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_form_fields_event_key ON form_fields (event_id, key) WHERE workshop_id IS NULL;

-- -----------------------------------------------------------------------------
-- 3. Event participants
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS event_participants (
    id            UUID PRIMARY KEY,
    org_id        UUID NOT NULL,
    event_id      UUID NOT NULL,
    -- Set when the person signs in with this email (portal: My passes).
    account_id    UUID REFERENCES accounts(id) ON DELETE SET NULL,
    email         VARCHAR(254) NOT NULL CHECK (email = lower(email)),
    full_name     VARCHAR(150) NOT NULL,
    -- pending: waiting for approval; waitlisted: the event was full.
    status        VARCHAR(20)  NOT NULL DEFAULT 'registered'
                  CHECK (status IN ('pending', 'registered', 'waitlisted', 'cancelled')),
    pass_id       VARCHAR(20)  NOT NULL,     -- e.g. CSA-7KQ4-M2XD, printed and spoken
    answers       JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(answers) = 'object'),
    source        VARCHAR(20)  NOT NULL DEFAULT 'form' CHECK (source IN ('form', 'organizer', 'import')),
    -- Imported from the current build: its QR payload stays valid.
    registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    UNIQUE (event_id, email),
    UNIQUE (event_id, pass_id),
    UNIQUE (org_id, id)
);
CREATE INDEX IF NOT EXISTS idx_event_participants_event ON event_participants (event_id, status, registered_at);
CREATE INDEX IF NOT EXISTS idx_event_participants_account ON event_participants (account_id) WHERE account_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_event_participants_email ON event_participants (email);

-- -----------------------------------------------------------------------------
-- 4. Row Level Security
-- -----------------------------------------------------------------------------
ALTER TABLE form_fields        ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_participants ENABLE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON form_fields, event_participants TO event_pass_tenant;

DROP POLICY IF EXISTS form_fields_all ON form_fields;
CREATE POLICY form_fields_all ON form_fields FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

-- Participants: the current org's, plus (portal) the caller's own passes in
-- any org. Writes only in the current org.
DROP POLICY IF EXISTS event_participants_select ON event_participants;
CREATE POLICY event_participants_select ON event_participants FOR SELECT TO event_pass_tenant
    USING (org_id = app_org_id() OR account_id = app_account_id());
DROP POLICY IF EXISTS event_participants_insert ON event_participants;
CREATE POLICY event_participants_insert ON event_participants FOR INSERT TO event_pass_tenant
    WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS event_participants_update ON event_participants;
CREATE POLICY event_participants_update ON event_participants FOR UPDATE TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS event_participants_delete ON event_participants;
CREATE POLICY event_participants_delete ON event_participants FOR DELETE TO event_pass_tenant
    USING (org_id = app_org_id());

-- The portal shows each pass with its event and org, across orgs: reading
-- only. Changes stay in the current org (one policy per command, so the
-- portal exception can never reach an UPDATE or DELETE).
DROP POLICY IF EXISTS events_all ON events;
DROP POLICY IF EXISTS events_select ON events;
CREATE POLICY events_select ON events FOR SELECT TO event_pass_tenant
    USING (org_id = app_org_id()
           OR id IN (SELECT p.event_id FROM event_participants p WHERE p.account_id = app_account_id()));
DROP POLICY IF EXISTS events_insert ON events;
CREATE POLICY events_insert ON events FOR INSERT TO event_pass_tenant WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS events_update ON events;
CREATE POLICY events_update ON events FOR UPDATE TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS events_delete ON events;
CREATE POLICY events_delete ON events FOR DELETE TO event_pass_tenant USING (org_id = app_org_id());
DROP POLICY IF EXISTS orgs_select ON orgs;
CREATE POLICY orgs_select ON orgs FOR SELECT TO event_pass_tenant
    USING (id = app_org_id()
           OR id IN (SELECT m.org_id FROM org_members m WHERE m.account_id = app_account_id())
           OR id IN (SELECT s.org_id FROM event_shares s WHERE s.account_id = app_account_id())
           OR id IN (SELECT p.org_id FROM event_participants p WHERE p.account_id = app_account_id()));
