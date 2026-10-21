-- +goose Up
-- =============================================================================
-- Migration 015: participant profiles and the participant map (beta)
-- =============================================================================
-- On their pass page a participant can add a photo, a one-line headline and
-- links (LinkedIn, website, GitHub), and choose whether other participants
-- of the same event can see them. The participant map (beta, off unless the
-- organizer turns it on) shows public profiles whose owners chose to share
-- their live location; a location older than a few minutes is not shown, and
-- stopping deletes it.
--
-- Profiles are per registration: public means "to this event's
-- participants", nowhere else.
--
-- Safe to re-run.

CREATE TABLE IF NOT EXISTS participant_profiles (
    participant_id   UUID PRIMARY KEY,
    org_id           UUID NOT NULL,
    event_id         UUID NOT NULL,
    headline         VARCHAR(100) NOT NULL DEFAULT '',
    links            JSONB NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(links) = 'object'),
    photo            BYTEA,               -- 400 x 400 JPEG, re-encoded
    photo_updated_at TIMESTAMPTZ,
    visibility       VARCHAR(10) NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'public')),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, participant_id) REFERENCES event_participants (org_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_participant_profiles_public ON participant_profiles (event_id) WHERE visibility = 'public';

-- Live locations: one row per participant while they share.
CREATE TABLE IF NOT EXISTS participant_locations (
    participant_id UUID PRIMARY KEY,
    org_id         UUID NOT NULL,
    event_id       UUID NOT NULL,
    lat            DOUBLE PRECISION NOT NULL CHECK (lat BETWEEN -90 AND 90),
    lng            DOUBLE PRECISION NOT NULL CHECK (lng BETWEEN -180 AND 180),
    accuracy_m     REAL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, participant_id) REFERENCES event_participants (org_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_participant_locations_event ON participant_locations (event_id, updated_at);

-- The organizer's switch for the map, per event.
CREATE TABLE IF NOT EXISTS participant_map_settings (
    event_id   UUID PRIMARY KEY,
    org_id     UUID NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE
);

-- What participants think of the beta.
CREATE TABLE IF NOT EXISTS beta_feedback (
    id             UUID PRIMARY KEY,
    org_id         UUID NOT NULL,
    event_id       UUID NOT NULL,
    participant_id UUID,
    feature        VARCHAR(30) NOT NULL DEFAULT 'participant_map',
    rating         SMALLINT NOT NULL CHECK (rating IN (-1, 1)),
    comment        VARCHAR(1000) NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (org_id, event_id) REFERENCES events (org_id, id) ON DELETE CASCADE,
    FOREIGN KEY (org_id, participant_id) REFERENCES event_participants (org_id, id) ON DELETE SET NULL (participant_id)
);
CREATE INDEX IF NOT EXISTS idx_beta_feedback_event ON beta_feedback (event_id, created_at DESC);

-- Organizers (tenant role) read and manage these within their org.
-- Participants' own reads and writes run on the org's system path after the
-- service has checked they're registered for the event.
ALTER TABLE participant_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE participant_locations ENABLE ROW LEVEL SECURITY;
ALTER TABLE participant_map_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE beta_feedback ENABLE ROW LEVEL SECURITY;
GRANT SELECT, INSERT, UPDATE, DELETE ON participant_profiles, participant_locations, participant_map_settings TO nsac_tenant;
GRANT SELECT, INSERT ON beta_feedback TO nsac_tenant;
DROP POLICY IF EXISTS participant_profiles_org ON participant_profiles;
CREATE POLICY participant_profiles_org ON participant_profiles FOR ALL TO nsac_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS participant_locations_org ON participant_locations;
CREATE POLICY participant_locations_org ON participant_locations FOR ALL TO nsac_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS participant_map_settings_org ON participant_map_settings;
CREATE POLICY participant_map_settings_org ON participant_map_settings FOR ALL TO nsac_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
DROP POLICY IF EXISTS beta_feedback_org ON beta_feedback;
CREATE POLICY beta_feedback_org ON beta_feedback FOR ALL TO nsac_tenant
    USING (org_id = app_org_id()) WITH CHECK (org_id = app_org_id());
