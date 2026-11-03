-- +goose Up
-- =============================================================================
-- Migration 008: events, sharing and the audit log (SaaS milestone M2)
-- =============================================================================
-- An event belongs to an org and is shared like a Google Doc: its owner, org
-- owners (implicit owner) and org admins (implicit editor) can open it; anyone
-- else needs a share (editor, check-in staff or viewer). A share can go to
-- someone outside the org: a guest, who then sees that org with only the
-- events shared with them. A link share gives read-only access to anyone with
-- the link.
--
-- Wall 3 of tenant isolation: child tables reference events by (org_id, id),
-- so a row can never point at another org's event.
--
-- Safe to re-run.

-- Org-wide switches, e.g. {"external_sharing": false}. Missing keys use the
-- defaults in the API.
ALTER TABLE orgs ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}'::jsonb;

-- -----------------------------------------------------------------------------
-- 1. Events
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS events (
    id          UUID PRIMARY KEY,
    org_id      UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug        VARCHAR(60)  NOT NULL CHECK (slug ~ '^[a-z0-9](?:[a-z0-9-]{0,58}[a-z0-9])?$'),
    name        VARCHAR(150) NOT NULL,
    type        VARCHAR(20)  NOT NULL DEFAULT 'hackathon'
                CHECK (type IN ('hackathon', 'conference', 'workshop_series', 'workshop', 'meetup')),
    -- Live and Finished are derived from the dates of a published event.
    status      VARCHAR(20)  NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
    description TEXT         NOT NULL DEFAULT '',
    starts_at   TIMESTAMPTZ,
    ends_at     TIMESTAMPTZ,
    time_zone   VARCHAR(64)  NOT NULL DEFAULT 'UTC',
    -- {"name", "address", "lat", "lng"}; empty when online or undecided.
    venue       JSONB        NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(venue) = 'object'),
    online_url  TEXT         NOT NULL DEFAULT '',
    capacity    INTEGER      CHECK (capacity > 0),
    visibility  VARCHAR(20)  NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'unlisted', 'private')),
    language    VARCHAR(10)  NOT NULL DEFAULT 'en',
    tags        TEXT[]       NOT NULL DEFAULT '{}',
    -- {"editors_can_share": bool, "viewers_can_export": bool}
    settings    JSONB        NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(settings) = 'object'),
    owner_id    UUID NOT NULL REFERENCES accounts(id),
    created_by  UUID REFERENCES accounts(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    archived_at TIMESTAMPTZ,
    deleted_at  TIMESTAMPTZ,   -- in Trash; restorable for 30 days
    CHECK (ends_at IS NULL OR starts_at IS NULL OR ends_at >= starts_at),
    UNIQUE (org_id, slug),
    UNIQUE (org_id, id)        -- target of the composite foreign keys below
);
CREATE INDEX IF NOT EXISTS idx_events_org_owner ON events (org_id, owner_id);

-- -----------------------------------------------------------------------------
-- 2. Shares: a role for an account, or a view-only link
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS event_shares (
    id              UUID PRIMARY KEY,
    org_id          UUID NOT NULL,
    event_id        UUID NOT NULL,
    account_id      UUID REFERENCES accounts(id) ON DELETE CASCADE,
    link_token_hash BYTEA UNIQUE,
    role            VARCHAR(20) NOT NULL CHECK (role IN ('editor', 'checkin', 'viewer')),
    granted_by      UUID REFERENCES accounts(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ,  -- links only
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    CHECK ((account_id IS NULL) <> (link_token_hash IS NULL)),
    -- Check-in access is never granted by link.
    CHECK (link_token_hash IS NULL OR role = 'viewer'),
    UNIQUE (event_id, account_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_event_shares_one_link ON event_shares (event_id) WHERE link_token_hash IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_event_shares_account ON event_shares (account_id) WHERE account_id IS NOT NULL;

-- -----------------------------------------------------------------------------
-- 3. Per-person event state: starred, last opened (for Recent)
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS event_stars (
    org_id     UUID NOT NULL,
    event_id   UUID NOT NULL,
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (account_id, event_id),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS event_opens (
    org_id     UUID NOT NULL,
    event_id   UUID NOT NULL,
    account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    opened_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (account_id, event_id),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE
);

-- -----------------------------------------------------------------------------
-- 4. Event invites: sharing with an address that has no account yet
-- -----------------------------------------------------------------------------
ALTER TABLE invites DROP CONSTRAINT IF EXISTS invites_role_check;
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'invites_role_for_kind') THEN
        ALTER TABLE invites ADD CONSTRAINT invites_role_for_kind CHECK (
            (event_id IS NULL AND role IN ('admin', 'member'))
            OR (event_id IS NOT NULL AND role IN ('editor', 'checkin', 'viewer')));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'invites_event_fkey') THEN
        ALTER TABLE invites ADD CONSTRAINT invites_event_fkey
            FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE;
    END IF;
END $$;
-- +goose StatementEnd
CREATE UNIQUE INDEX IF NOT EXISTS uq_invites_open_event
    ON invites (event_id, email) WHERE event_id IS NOT NULL AND accepted_at IS NULL AND revoked_at IS NULL;

-- -----------------------------------------------------------------------------
-- 5. Audit log (append-only)
-- -----------------------------------------------------------------------------
-- Every permission change, share, invite, event change and deletion. No
-- foreign key on event_id, so history outlives a purged event.
CREATE TABLE IF NOT EXISTS audit_log (
    id               UUID PRIMARY KEY,
    org_id           UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    event_id         UUID,
    actor_account_id UUID REFERENCES accounts(id) ON DELETE SET NULL,
    action           VARCHAR(60) NOT NULL,   -- e.g. event.share.add
    target_type      VARCHAR(30) NOT NULL,   -- event, share, member, invite, link
    target_id        TEXT NOT NULL DEFAULT '',
    diff             JSONB NOT NULL DEFAULT '{}'::jsonb,
    ip               VARCHAR(64) NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_org_time ON audit_log (org_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_event_time ON audit_log (event_id, created_at DESC) WHERE event_id IS NOT NULL;

-- -----------------------------------------------------------------------------
-- 6. Row Level Security
-- -----------------------------------------------------------------------------
ALTER TABLE events       ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_shares ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_stars  ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_opens  ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_log    ENABLE ROW LEVEL SECURITY;

GRANT SELECT, INSERT, UPDATE, DELETE ON events, event_shares, event_stars, event_opens TO event_pass_tenant;
-- Append-only: the tenant role can add entries but never change or remove them.
GRANT SELECT, INSERT ON audit_log TO event_pass_tenant;

-- Guests reach an org through a share, so "my organizations" includes it.
DROP POLICY IF EXISTS orgs_select ON orgs;
CREATE POLICY orgs_select ON orgs FOR SELECT TO event_pass_tenant
    USING (id = app_org_id()
           OR id IN (SELECT m.org_id FROM org_members m WHERE m.account_id = app_account_id())
           OR id IN (SELECT s.org_id FROM event_shares s WHERE s.account_id = app_account_id()));

-- Accounts: also the guests and owners of the current org's events.
DROP POLICY IF EXISTS accounts_select ON accounts;
CREATE POLICY accounts_select ON accounts FOR SELECT TO event_pass_tenant
    USING (id = app_account_id()
           OR id IN (SELECT m.account_id FROM org_members m WHERE m.org_id = app_org_id())
           OR id IN (SELECT s.account_id FROM event_shares s WHERE s.org_id = app_org_id())
           OR id IN (SELECT e.owner_id FROM events e WHERE e.org_id = app_org_id()));

DROP POLICY IF EXISTS events_all ON events;
CREATE POLICY events_all ON events FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());

-- Shares: the current org's, plus the caller's own anywhere (to find the
-- orgs they're a guest of).
DROP POLICY IF EXISTS event_shares_select ON event_shares;
CREATE POLICY event_shares_select ON event_shares FOR SELECT TO event_pass_tenant
    USING (org_id = app_org_id() OR account_id = app_account_id());
DROP POLICY IF EXISTS event_shares_insert ON event_shares;
CREATE POLICY event_shares_insert ON event_shares FOR INSERT TO event_pass_tenant
    WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS event_shares_update ON event_shares;
CREATE POLICY event_shares_update ON event_shares FOR UPDATE TO event_pass_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS event_shares_delete ON event_shares;
CREATE POLICY event_shares_delete ON event_shares FOR DELETE TO event_pass_tenant
    USING (org_id = app_org_id());

-- Stars and opens are personal as well as per-org.
DROP POLICY IF EXISTS event_stars_own ON event_stars;
CREATE POLICY event_stars_own ON event_stars FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id() AND account_id = app_account_id())
    WITH CHECK (org_id = app_org_id() AND account_id = app_account_id());
DROP POLICY IF EXISTS event_opens_own ON event_opens;
CREATE POLICY event_opens_own ON event_opens FOR ALL TO event_pass_tenant
    USING (org_id = app_org_id() AND account_id = app_account_id())
    WITH CHECK (org_id = app_org_id() AND account_id = app_account_id());

DROP POLICY IF EXISTS audit_select ON audit_log;
CREATE POLICY audit_select ON audit_log FOR SELECT TO event_pass_tenant
    USING (org_id = app_org_id());
-- Entries are written as the signed-in account, never on someone's behalf.
DROP POLICY IF EXISTS audit_insert ON audit_log;
CREATE POLICY audit_insert ON audit_log FOR INSERT TO event_pass_tenant
    WITH CHECK (org_id = app_org_id() AND actor_account_id IS NOT DISTINCT FROM app_account_id());
