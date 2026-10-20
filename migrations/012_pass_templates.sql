-- +goose Up
-- =============================================================================
-- Migration 012: pass templates with versions (SaaS milestone M5)
-- =============================================================================
-- An org's pass designs (JSON, rendered by internal/passes). Saving a design
-- adds a version; versions never change. Each event pins one version, so
-- editing a template never changes passes already sent until an editor moves
-- the event to the new version. An event with no template uses the gallery's
-- Classic design. Gallery designs live in code and are copied into an org
-- when chosen.
--
-- Safe to re-run.

CREATE TABLE IF NOT EXISTS pass_templates (
    id              UUID PRIMARY KEY,
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name            VARCHAR(100) NOT NULL,
    current_version INTEGER NOT NULL DEFAULT 1,
    created_by      UUID REFERENCES accounts(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at     TIMESTAMPTZ,
    UNIQUE (org_id, id)
);

CREATE TABLE IF NOT EXISTS pass_template_versions (
    org_id      UUID NOT NULL,
    template_id UUID NOT NULL,
    version     INTEGER NOT NULL CHECK (version > 0),
    design      JSONB NOT NULL CHECK (jsonb_typeof(design) = 'object'),
    created_by  UUID REFERENCES accounts(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (template_id, version),
    FOREIGN KEY (org_id, template_id) REFERENCES pass_templates (org_id, id) ON DELETE CASCADE
);

ALTER TABLE events ADD COLUMN IF NOT EXISTS pass_template_id UUID;
ALTER TABLE events ADD COLUMN IF NOT EXISTS pass_template_version INTEGER;
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'events_pass_template_fkey') THEN
        ALTER TABLE events ADD CONSTRAINT events_pass_template_fkey
            FOREIGN KEY (org_id, pass_template_id) REFERENCES pass_templates (org_id, id); -- templates are archived, never deleted
        ALTER TABLE events ADD CONSTRAINT events_pass_template_version_fkey
            FOREIGN KEY (pass_template_id, pass_template_version) REFERENCES pass_template_versions (template_id, version);
        ALTER TABLE events ADD CONSTRAINT events_pass_template_pinned
            CHECK ((pass_template_id IS NULL) = (pass_template_version IS NULL));
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE pass_templates         ENABLE ROW LEVEL SECURITY;
ALTER TABLE pass_template_versions ENABLE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE ON pass_templates TO nsac_tenant;
-- Versions are append-only.
GRANT SELECT, INSERT ON pass_template_versions TO nsac_tenant;

DROP POLICY IF EXISTS pass_templates_all ON pass_templates;
CREATE POLICY pass_templates_all ON pass_templates FOR ALL TO nsac_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS pass_template_versions_all ON pass_template_versions;
CREATE POLICY pass_template_versions_all ON pass_template_versions FOR ALL TO nsac_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
