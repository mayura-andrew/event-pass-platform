-- +goose Up
-- =============================================================================
-- Migration 014: pass assets (SaaS milestone M6)
-- =============================================================================
-- Images an organization uploads for its pass designs: full artwork made in
-- another tool, logos, sponsor strips. A design refers to one as
-- "asset:<id>". Uploads are decoded and re-encoded (PNG or JPEG) before they
-- are stored, which drops metadata such as camera location. Assets are never
-- changed or deleted, because saved design versions refer to them; the same
-- image uploaded twice is stored once.
--
-- Safe to re-run.

CREATE TABLE IF NOT EXISTS pass_assets (
    id           UUID PRIMARY KEY,
    org_id       UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    content_type VARCHAR(20) NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg')),
    width        INTEGER NOT NULL CHECK (width > 0),
    height       INTEGER NOT NULL CHECK (height > 0),
    data         BYTEA NOT NULL,
    sha256       BYTEA NOT NULL,
    created_by   UUID REFERENCES accounts(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, id),
    UNIQUE (org_id, sha256)
);

ALTER TABLE pass_assets ENABLE ROW LEVEL SECURITY;
GRANT SELECT, INSERT ON pass_assets TO event_pass_tenant;
DROP POLICY IF EXISTS pass_assets_org ON pass_assets;
CREATE POLICY pass_assets_org ON pass_assets FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
