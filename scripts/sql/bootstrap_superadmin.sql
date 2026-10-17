-- =============================================================================
-- Create the first superadmin from the Supabase SQL editor (no Go needed).
-- Run AFTER migrations 001, 002 and 003.
-- =============================================================================
-- 1. Replace the three values marked CHANGE ME below.
-- 2. Run the whole script once.
-- 3. Sign in at https://<your app>/admin/ with that email and password. You'll
--    be asked to choose your own password straight away.
--
-- The password typed here is TEMPORARY on purpose: the SQL editor keeps a
-- query history, so this one stops working the moment you set your own at
-- first sign-in. Prefer `go run ./scripts/create-admin` when Go is available;
-- it never writes the password anywhere.
--
-- Re-running with the same email resets that account to this temporary
-- password and makes it an active superadmin again.

-- Supabase installs pgcrypto in the "extensions" schema; this is a no-op there.
CREATE SCHEMA IF NOT EXISTS extensions;
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA extensions;

WITH input AS (
    SELECT lower(trim('lead@example.org'))       AS email,      -- CHANGE ME
           trim('Event Lead')                    AS full_name,  -- CHANGE ME
           'Temporary-Password-Change-Me-1'      AS password    -- CHANGE ME (12+ characters)
), checked AS (
    SELECT * FROM input
    WHERE length(password) >= 12
      AND email LIKE '%_@_%._%'
      AND email <> 'lead@example.org'           -- refuse the unedited example
)
INSERT INTO admins (email, password_hash, full_name, role, is_active, force_password_change)
SELECT email,
       -- bcrypt, cost 10: the same format the Go API writes and checks.
       extensions.crypt(password, extensions.gen_salt('bf', 10)),
       full_name, 'superadmin', true, true
FROM checked
ON CONFLICT (email) DO UPDATE SET
    password_hash         = EXCLUDED.password_hash,
    full_name             = EXCLUDED.full_name,
    role                  = 'superadmin',
    is_active             = true,
    force_password_change = true
RETURNING email, role, 'temporary password set; sign in at /admin/ to choose your own' AS next_step;

-- No row returned above = nothing was saved: the email still says
-- lead@example.org, isn't an address, or the password is under 12 characters.
