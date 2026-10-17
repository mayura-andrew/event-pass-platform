-- =============================================================================
-- NASA Space Apps Challenge: staff roles, checkpoint grants, emailed credentials
-- Run after 002_dashboard_schema.sql. Safe to re-run.
-- =============================================================================

-- -----------------------------------------------------------------------------
-- 1. Account state
-- -----------------------------------------------------------------------------
-- is_active exists since 001; repeated here so this file also documents it.
ALTER TABLE admins ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT true;

-- Set when an account is given a generated temporary password (new staff,
-- "resend credentials"). While true, the API accepts that password only for
-- setting a new one.
ALTER TABLE admins ADD COLUMN IF NOT EXISTS force_password_change BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE admins ADD COLUMN IF NOT EXISTS password_changed_at TIMESTAMPTZ;
ALTER TABLE admins ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMPTZ;
ALTER TABLE admins ADD COLUMN IF NOT EXISTS created_by UUID REFERENCES admins(id) ON DELETE SET NULL;

-- -----------------------------------------------------------------------------
-- 2. Roles: superadmin | admin | scanner
-- -----------------------------------------------------------------------------
-- 'volunteer' is renamed 'scanner'. The inline CHECK from 001 is named
-- admins_role_check by PostgreSQL; it is replaced, not altered.
ALTER TABLE admins DROP CONSTRAINT IF EXISTS admins_role_check;
UPDATE admins SET role = 'scanner' WHERE role = 'volunteer';
ALTER TABLE admins
    ADD CONSTRAINT admins_role_check CHECK (role IN ('superadmin', 'admin', 'scanner'));
-- New rows default to the least-privileged role.
ALTER TABLE admins ALTER COLUMN role SET DEFAULT 'scanner';

-- -----------------------------------------------------------------------------
-- 3. Checkpoint grants
-- -----------------------------------------------------------------------------
-- A scanner account sees, and may upload scans for, only the scenarios granted
-- here. Admins and superadmins can scan every scenario and need no rows.
CREATE TABLE IF NOT EXISTS staff_scenario_grants (
    admin_id    UUID NOT NULL REFERENCES admins(id)         ON DELETE CASCADE,
    scenario_id UUID NOT NULL REFERENCES scan_scenarios(id) ON DELETE CASCADE,
    assigned_by UUID          REFERENCES admins(id)         ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- The primary key's index is named idx_staff_grants_admin: admin_id leads,
    -- so it serves the per-request "what may this account scan" lookup, and a
    -- separate index on admin_id would only duplicate it.
    CONSTRAINT idx_staff_grants_admin PRIMARY KEY (admin_id, scenario_id)
);

-- "Who is assigned to this checkpoint" (dashboard, scenario deletion checks).
CREATE INDEX IF NOT EXISTS idx_staff_grants_scenario
    ON staff_scenario_grants (scenario_id);

ALTER TABLE staff_scenario_grants ENABLE ROW LEVEL SECURITY;

-- -----------------------------------------------------------------------------
-- 4. Keep existing scanner accounts working
-- -----------------------------------------------------------------------------
-- Before this migration every account could scan every scenario. Grant all
-- scenarios to existing scanner accounts so nobody loses their checkpoints on
-- deploy. Runs only while the table is empty, so re-running this file after
-- grants have been edited doesn't hand everyone every new scenario.
INSERT INTO staff_scenario_grants (admin_id, scenario_id)
SELECT a.id, s.id
FROM admins a
CROSS JOIN scan_scenarios s
WHERE a.role = 'scanner'
  AND NOT EXISTS (SELECT 1 FROM staff_scenario_grants)
ON CONFLICT DO NOTHING;
