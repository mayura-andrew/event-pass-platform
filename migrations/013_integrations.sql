-- +goose Up
-- =============================================================================
-- Migration 013: integrations (SaaS milestone M5)
-- =============================================================================
-- Per-event connections to Google: a spreadsheet kept up to date
-- (google_sheets, platform -> sheet only) and a Google Form whose responses
-- become participants (google_form). The Google refresh token is stored
-- encrypted (AES-256-GCM, internal/secrets); the API never returns it.
--
-- Safe to re-run.

CREATE TABLE IF NOT EXISTS integrations (
    id             UUID PRIMARY KEY,
    org_id         UUID NOT NULL,
    event_id       UUID NOT NULL,
    kind           VARCHAR(20) NOT NULL CHECK (kind IN ('google_sheets', 'google_form')),
    -- Sheets: {"spreadsheet_id", "url", "title"}; forms: {"form_id", "title",
    -- "name_question", "email_question", "map": {question_id: field_key}}
    config         JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(config) = 'object'),
    secret         BYTEA,                 -- encrypted refresh token
    google_email   VARCHAR(254) NOT NULL DEFAULT '',
    status         VARCHAR(20) NOT NULL DEFAULT 'connected' CHECK (status IN ('connected', 'error', 'paused')),
    last_synced_at TIMESTAMPTZ,
    last_error     TEXT NOT NULL DEFAULT '',
    imported       INTEGER NOT NULL DEFAULT 0, -- forms: participants created so far
    connected_by   UUID REFERENCES accounts(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    UNIQUE (event_id, kind)
);

-- Form responses imported as participants: each response once.
ALTER TABLE event_participants ADD COLUMN IF NOT EXISTS external_ref VARCHAR(200);
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_participants_external ON event_participants (event_id, external_ref)
    WHERE external_ref IS NOT NULL;

ALTER TABLE integrations ENABLE ROW LEVEL SECURITY;
-- The tenant role manages the event's integrations but can't read the secret
-- (the worker, as the connecting user, decrypts it).
GRANT SELECT (id, org_id, event_id, kind, config, google_email, status, last_synced_at, last_error, imported,
              connected_by, created_at, updated_at) ON integrations TO event_pass_tenant;
GRANT INSERT, UPDATE, DELETE ON integrations TO event_pass_tenant;
DROP POLICY IF EXISTS integrations_all ON integrations;
CREATE POLICY integrations_all ON integrations FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
