-- +goose Up
-- =============================================================================
-- Migration 010: email outbox (SaaS milestone M3)
-- =============================================================================
-- Emails to participants (pass issued, waiting for approval, waitlist,
-- cancelled) are rows here, written in the same transaction as the change
-- that causes them, so a registration and its email commit together. The
-- worker claims rows with FOR UPDATE SKIP LOCKED, sends, and retries with
-- backoff; a crashed send is picked up again when its lease runs out.
--
-- Safe to re-run.

CREATE TABLE IF NOT EXISTS outbox (
    id              UUID PRIMARY KEY,
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    event_id        UUID,
    participant_id  UUID,
    to_email        VARCHAR(254) NOT NULL,
    template_key    VARCHAR(40)  NOT NULL
                    CHECK (template_key IN ('pass_issued', 'registration_received', 'waitlisted', 'registration_cancelled')),
    vars            JSONB NOT NULL DEFAULT '{}'::jsonb,
    status          VARCHAR(20)  NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'sending', 'sent', 'failed')),
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    locked_until    TIMESTAMPTZ,
    provider_message_id TEXT,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at         TIMESTAMPTZ,
    FOREIGN KEY (org_id, participant_id) REFERENCES event_participants (org_id, id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox (next_attempt_at) WHERE status IN ('queued', 'sending');
CREATE INDEX IF NOT EXISTS idx_outbox_participant ON outbox (participant_id, created_at DESC);

ALTER TABLE outbox ENABLE ROW LEVEL SECURITY;
-- Tenant transactions queue emails and read their status; the worker runs
-- as the connecting user across orgs.
GRANT SELECT, INSERT ON outbox TO event_pass_tenant;
DROP POLICY IF EXISTS outbox_select ON outbox;
CREATE POLICY outbox_select ON outbox FOR SELECT TO event_pass_tenant USING (org_id = app_org_id());
DROP POLICY IF EXISTS outbox_insert ON outbox;
CREATE POLICY outbox_insert ON outbox FOR INSERT TO event_pass_tenant WITH CHECK (org_id = app_org_id());
