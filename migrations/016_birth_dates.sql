-- +goose Up
-- =============================================================================
-- Migration 016: birth dates and the under-18 cut-off (SaaS)
-- =============================================================================
-- Organizers can ask for a birth date on the registration form (a "Birth
-- date" question). The answer is also kept in event_participants.birth_date
-- so minors can be counted and filtered in SQL. A participant is under 18
-- when they haven't had their 18th birthday by the event's age_on date
-- (e.g. 14 November); without one, the event's start date, else today.
--
-- Safe to re-run.

ALTER TABLE events ADD COLUMN IF NOT EXISTS age_on DATE;
ALTER TABLE event_participants ADD COLUMN IF NOT EXISTS birth_date DATE;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_event_participants_birth_date') THEN
        ALTER TABLE event_participants ADD CONSTRAINT chk_event_participants_birth_date
            CHECK (birth_date IS NULL OR birth_date >= DATE '1900-01-01');
    END IF;
END $$;
-- +goose StatementEnd

-- The new question type, and at most one birth date question per event.
ALTER TABLE form_fields DROP CONSTRAINT IF EXISTS form_fields_type_check;
ALTER TABLE form_fields ADD CONSTRAINT form_fields_type_check CHECK (type IN ('text', 'long_text', 'email', 'phone',
    'number', 'date', 'birth_date', 'select', 'multi_select', 'checkbox', 'consent', 'url'));
CREATE UNIQUE INDEX IF NOT EXISTS uq_form_fields_one_birth_date ON form_fields (event_id)
    WHERE type = 'birth_date' AND workshop_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_event_participants_birth_date
    ON event_participants (event_id, birth_date) WHERE birth_date IS NOT NULL;
