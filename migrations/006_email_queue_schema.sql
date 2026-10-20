-- +goose Up
-- Applied as one statement: every migration up to 006 was written to be
-- re-run safely by hand, and goose runs it once more on databases set up
-- before goose (see docs/decisions/0002-goose-migrations.md).
-- +goose StatementBegin
-- =============================================================================
-- NASA Space Apps Challenge: delegate pass email queue
-- Run after 005_jsonb_dynamic_schema.sql. Safe to re-run.
-- =============================================================================
-- Pass emails go out through a Gmail account, which throttles bursts and caps
-- a day at about 500 messages (2,000 on Workspace). Instead of sending inside
-- the request, the dashboard queues one row per delegate here and a worker in
-- the API sends one every couple of seconds. The queue survives restarts and
-- deploys; a delegate is never queued twice at the same time.

-- -----------------------------------------------------------------------------
-- 1. Jobs
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS email_jobs (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    participant_id  UUID NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
    -- Copied when queued, so the log shows where each email went even if
    -- the delegate's details change later.
    recipient_email VARCHAR(255) NOT NULL,
    kind            VARCHAR(30)  NOT NULL DEFAULT 'pass_confirmation'
                    CHECK (kind IN ('pass_confirmation')),
    -- pending   waiting for its turn (scheduled_at)
    -- sending   claimed by a worker; back to pending if it stalls (crash)
    -- sent      accepted by the mail server
    -- failed    gave up: rejected address, or out of attempts
    -- cancelled no longer applicable (delegate unconfirmed before sending)
    status          VARCHAR(20)  NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'sending', 'sent', 'failed', 'cancelled')),
    attempts        INTEGER      NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts    INTEGER      NOT NULL DEFAULT 3 CHECK (max_attempts BETWEEN 1 AND 10),
    last_error      TEXT,
    scheduled_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    locked_at       TIMESTAMPTZ,  -- when a worker claimed it (status = sending)
    sent_at         TIMESTAMPTZ,
    created_by      UUID REFERENCES admins(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- The worker's "next job" query and the status counts.
CREATE INDEX IF NOT EXISTS idx_email_jobs_status_scheduled
    ON email_jobs (status, scheduled_at);

-- At most one queued or in-flight email per delegate and kind, so a double
-- click on "bulk enqueue" (or two organizers at once) can't send twice.
CREATE UNIQUE INDEX IF NOT EXISTS uq_email_jobs_active
    ON email_jobs (participant_id, kind)
    WHERE status IN ('pending', 'sending');

-- "Who has already been emailed" for bulk enqueue and the delegate history.
CREATE INDEX IF NOT EXISTS idx_email_jobs_participant
    ON email_jobs (participant_id, kind, status);

-- Messages sent in the last 24 hours, for the daily sending cap.
CREATE INDEX IF NOT EXISTS idx_email_jobs_sent_at
    ON email_jobs (sent_at)
    WHERE status = 'sent';

ALTER TABLE email_jobs ENABLE ROW LEVEL SECURITY;

-- -----------------------------------------------------------------------------
-- 2. Queue state (one row)
-- -----------------------------------------------------------------------------
-- Pausing is stored, not kept in memory, so a paused queue stays paused
-- through a deploy or restart. The worker also pauses itself, with a reason,
-- when sending can't work for anyone (wrong app password, Gmail's daily cap).
CREATE TABLE IF NOT EXISTS email_queue_state (
    id           BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    paused       BOOLEAN     NOT NULL DEFAULT false,
    pause_reason TEXT,        -- NULL when paused by an organizer
    updated_by   UUID REFERENCES admins(id) ON DELETE SET NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO email_queue_state (id) VALUES (true) ON CONFLICT (id) DO NOTHING;

ALTER TABLE email_queue_state ENABLE ROW LEVEL SECURITY;

-- +goose StatementEnd
