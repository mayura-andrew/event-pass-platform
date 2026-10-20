-- +goose Up
-- Applied as one statement: every migration up to 006 was written to be
-- re-run safely by hand, and goose runs it once more on databases set up
-- before goose (see docs/decisions/0002-goose-migrations.md).
-- +goose StatementBegin
-- =============================================================================
-- NASA Space Apps Challenge: custom delegate fields (JSONB)
-- Run after 004_auto_membership_id.sql. Safe to re-run.
-- =============================================================================
-- Registration forms change from event to event (food preference, T-shirt
-- size, a guardian's phone number...). Instead of a column per question, extra
-- CSV columns are stored per delegate in participants.custom_attributes, and
-- participant_field_definitions says what each key is called and where it
-- appears.

-- -----------------------------------------------------------------------------
-- 1. Per-delegate values
-- -----------------------------------------------------------------------------
-- A flat object: {"food_preference": "Vegetarian", "tshirt_size": "M"}.
-- Keys are field_key values below; a missing key means "not given".
ALTER TABLE participants
    ADD COLUMN IF NOT EXISTS custom_attributes JSONB NOT NULL DEFAULT '{}'::jsonb;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_participants_custom_attrs') THEN
        -- Always an object, and bounded: the API caps a value at 500
        -- characters, so 16 KB per delegate only stops something gone wrong
        -- (it all ends up in every scanner's manifest).
        ALTER TABLE participants
            ADD CONSTRAINT chk_participants_custom_attrs
            CHECK (jsonb_typeof(custom_attributes) = 'object'
                   AND pg_column_size(custom_attributes) <= 16384);
    END IF;
END $$;

-- Dashboard filters match with containment (custom_attributes @> '{"k": v}')
-- and "not given" with the ? operator. The default jsonb_ops class serves
-- both; jsonb_path_ops would be smaller but can't answer ?.
CREATE INDEX IF NOT EXISTS idx_participants_custom_attrs
    ON participants USING GIN (custom_attributes);

-- -----------------------------------------------------------------------------
-- 2. Field definitions
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS participant_field_definitions (
    id                       UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- snake_case key inside custom_attributes. Never one of the fixed
    -- participant columns, so a CSV header can't shadow them.
    field_key                VARCHAR(50)  NOT NULL UNIQUE
                             CHECK (field_key ~ '^[a-z][a-z0-9_]{0,49}$')
                             CHECK (field_key NOT IN ('id', 'membership_id', 'email', 'full_name',
                                    'team_name', 'date_of_birth', 'venue_id', 'assigned_venue_id',
                                    'is_confirmed', 'qr_signature', 'created_at', 'custom_attributes',
                                    'venue_name', 'hall_section')),
    field_label              VARCHAR(100) NOT NULL CHECK (length(btrim(field_label)) > 0),
    -- How values are validated and stored in the JSON: text -> string,
    -- number -> number, boolean -> true/false, date -> 'YYYY-MM-DD' string.
    data_type                VARCHAR(20)  NOT NULL DEFAULT 'text'
                             CHECK (data_type IN ('text', 'number', 'boolean', 'date')),
    -- Copied to every scanner phone and shown after each scan. Off by
    -- default: turning it on puts these values on every volunteer's phone.
    show_in_scanner_popup    BOOLEAN      NOT NULL DEFAULT false,
    show_in_dashboard_filter BOOLEAN      NOT NULL DEFAULT false,
    sort_order               INTEGER      NOT NULL DEFAULT 100,
    -- seed (this file) | manual (added on the Fields page) | csv (found in an upload)
    source                   VARCHAR(10)  NOT NULL DEFAULT 'manual'
                             CHECK (source IN ('seed', 'manual', 'csv')),
    created_by               UUID REFERENCES admins(id) ON DELETE SET NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

ALTER TABLE participant_field_definitions ENABLE ROW LEVEL SECURITY;

-- Common registration questions, so the first upload has sensible labels.
-- Guardian contact stays off the scanner until an organizer decides
-- otherwise. DO NOTHING keeps whatever organizers have changed since.
INSERT INTO participant_field_definitions
    (field_key, field_label, data_type, show_in_scanner_popup, show_in_dashboard_filter, sort_order, source)
VALUES
    ('food_preference', 'Food Preference',           'text', true,  true,  10, 'seed'),
    ('tshirt_size',     'T-Shirt Size',              'text', false, true,  20, 'seed'),
    ('parent_contact',  'Parent / Guardian Contact', 'text', false, false, 30, 'seed')
ON CONFLICT (field_key) DO NOTHING;

-- +goose StatementEnd
