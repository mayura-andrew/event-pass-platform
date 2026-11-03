# SaaS progress

Spec: *Space Apps Pass SaaS: Development Spec* (2026-10-20). Decisions:
[docs/decisions](decisions/).

## M1 Foundation: built, gate not yet shown on staging

Gate: sign in with Google and by email link; create an org; invite a member;
the tenant isolation suite and RLS repository tests pass in CI.

| Part | State |
| --- | --- |
| Migration 007: accounts, identities, sessions, sign-in links, orgs, members, invites, RLS via `nsac_tenant` | Done, tested on a fresh and a hand-migrated database |
| goose and `cmd/migrate` (001-006 annotated, safe to re-run) | Done |
| Email link sign-in (one-time, 15 min, 5 per address per 15 min, button to confirm so mail scanners can't use it) | Done |
| Google sign-in (code + PKCE, state, nonce) | Done, tested against a fake Google; needs a real OAuth client |
| Server-side sessions (12 h, hashed tokens, CSRF token, sign out) | Done |
| Orgs: create, list, members, roles, invite by email, accept, withdraw, leave; last owner protected in the database | Done (JSON API and pages) |
| `authz` role matrix (org roles) and `OrgScope` | Done |
| Tests: RLS (no tenant sees nothing; scoped reads/writes), repository isolation, HTTP isolation over every org route, role matrix over HTTP, journeys | Done; isolation suite shown to fail when both walls are removed |
| CI: Postgres service, migrations applied, integration tests run | Done (first run pending on GitHub) |
| Pages: `/login`, `/app`, `/o/{org}`, `/invite`; tokens.css (light and dark); strict CSP | Done; checked at phone and desktop width |

To finish the gate:

1. Create a Google OAuth client (redirect URI `APP_BASE_URL/auth/google/callback`)
   and set `GOOGLE_OAUTH_CLIENT_ID` / `GOOGLE_OAUTH_CLIENT_SECRET`.
2. Apply 007 on a staging copy of Supabase and confirm ADR 3's role check.
3. Run the gate on staging: Google sign-in, email link, create org, invite.

## M2 Events and sharing: built, gate not yet shown on staging

Gate: the events home shows My events, Shared with me, Recent, Starred,
Archived and Trash; sharing with an editor, check-in staff and viewer gives
each exactly the access in the role matrix, verified by generated
authorization tests; every change appears in the audit log.

| Part | State |
| --- | --- |
| Migration 008: events, event_shares (people and links), stars, opens, event invites, append-only audit_log, RLS, composite FKs | Done |
| Events home: My events, All events (org owners and admins), Shared with me, Recent, Starred, Archived, Trash; search (name, venue, tags; `/` shortcut); sort; grid and list | Done |
| Event create (blank or copy), edit, duplicate (keeps people), archive, Trash with 30-day restore, transfer ownership, star | Done |
| Sharing: add by email (existing account at once, others by invite), roles, remove, withdraw invites, guests, view-only link with expiry, owner switches, org switch for outside sharing | Done |
| Audit log: every share, role change, invite, revocation, event change, archive and delete; org and event activity pages | Done |
| Tests: spec role table pinned cell by cell; matrix generated from `eventRoutes` (39 routes × 10 callers) incl. audit entries; cross-org event IDs; RLS and wall 3 on new tables; guests; links; views | Done |

Deferred (ADR 6): move event to another org, purge job after 30 days in
Trash, template starting points with content.

## M3 Register and pass: built, gate not yet shown on staging

Gate: a participant registers on a phone and receives a pass email built
from a gallery template within 60 seconds; the event migrated from the
current database keeps its old QR codes working.

| Part | State |
| --- | --- |
| Migrations 009 (publishing, form fields, event_participants) and 010 (outbox) | Done |
| Publish / unpublish; registration settings: open or approval, opens/closes, waitlist, pass ID prefix | Done |
| Form builder: 11 answer types, required, help, options, who sees the answer, reorder | Done (list editor; drag and drop later) |
| Public page `/e/{org}/{event}`: form with per-question errors, capacity, waitlist, closed states; already-registered re-sends the pass | Done |
| Participant list: status filters and counts, search, approve, cancel, give a place, pass preview and download | Done |
| Pass IDs (PREFIX-XXXX-XXXX), Ed25519 tokens with key rotation, pass check endpoint | Done |
| Pass rendering: Classic band, PNG 1080 x 1920 and A6 PDF, Sinhala and Tamil shaped | Done |
| Outbox and worker: pass issued, received, waitlist, cancelled; retries; Reply-To the organizer | Done; pass email measured at ~4 s locally |
| My passes portal `/me`: passes across orgs, pass page, PNG/PDF | Done |
| `cmd/legacy-import`: current delegates into an event, old QR codes valid | Done; checked on local data (NSAC-1092) |
| Tests: registration journey, capacity with 10 concurrent registrations, RLS for participants, pass emails, portal, import and old QR codes; matrix covers the new routes | Done |

## M4 Check-in and workshops: built, gate not yet shown on staging

Gate: three phones check in 60 people and record attendance at two
workshops; duplicates show amber with time and staff name; the dashboard
updates live. The nightly load test meets p95 under 300 ms.

| Part | State |
| --- | --- |
| Migration 011: workshops, sign-ups, checkpoints with rules, append-only check-ins; default Confirm, Check in, Kit | Done |
| Scan API: pass check, window, workshop sign-up, prerequisites, once/repeatable, roles; idempotent; duplicates with time and staff | Done |
| Undo (editors any, check-in staff own within 30 s), lookup, record from lookup, check-only mode | Done |
| Workshops: everyone / opt-in / invite, capacity, waitlist with promotion, time clashes, add at the door | Done |
| Scanner page: camera, paste box (also the e2e hook), status band, Record / Undo / Add, lookup, recent scans live, retry queue | Done |
| Check-in tab (checkpoints and rules), Workshops tab, Dashboard tab (tiles, bars, check-ins per 10 minutes, workshops, recent; live) | Done |
| Portal: workshop sign-up and cancel on the pass page | Done |
| Live feed: polled, Server-Sent Events | Done |
| Tests: the gate itself (3 phones x 60 people, 2 workshops), rules/undo/lookup, live stream, portal sign-ups, once under 20 concurrent phones, RLS and append-only; matrix covers 70 routes | Done |
| Load test: `scripts/loadtest` + nightly workflow | Done; local p95 165 ms |

## M5 Designer and integrations: built, gate not yet shown on staging

Gate: an admin builds a custom pass in the designer; snapshot tests confirm
the downloaded PNG and PDF match the preview. Sheets sync keeps a connected
spreadsheet up to date within about one minute; Google Form import creates
no duplicates.

| Part | State |
| --- | --- |
| Pass design format (JSON, absolute elements, `{{variables}}`), one renderer for preview, PNG and PDF | Done |
| Scan checks: one QR, dark on light, contrast 7:1, quiet zone 4, 30 mm printed, 40% of a phone, nothing over the code, sample names fit | Done |
| Gallery: Classic (the current pass), Conference badge (4 x 3 in), Minimal; snapshot tests per sample | Done |
| Migration 012: templates with append-only versions; events pin a version; Classic when none | Done |
| Designer page: drag, resize, snap, layers, properties, undo/redo, live checks and preview, save as a new version | Done |
| Event Pass tab: choose the design, preview with a real participant, re-send passes | Done |
| Migration 013: integrations, refresh tokens sealed with AES-256-GCM (`INTEGRATIONS_KEY`), unreadable by the tenant role | Done |
| Google Sheets: connect, the platform creates the sheet (drive.file scope), Participants / Check-ins / Workshops tabs rewritten within ~40 s of a change | Done |
| Google Form import: link, match questions, poll every minute, each response once (`external_ref`), existing emails skipped, passes emailed | Done |
| Revoked Google access shows "Needs reconnecting"; failures retried every minute | Done |
| Tests: designer gate (preview = PNG = PDF), integrations gate against a fake Google, forged callback; matrix covers the new routes | Done |

## M6 Billing and launch: in progress

Gate: Free and Pro plans with limits enforced; Stripe checkout and customer
portal work; onboarding checklist, help docs and status page are live; an
external security review is done. A real pilot event runs on production
with no lost check-ins.

| Part | State |
| --- | --- |
| Setup checklist on the events home (owners and admins): six steps ticked off from what exists, hide and show again | Done |
| Help pages at `/help` (10 topics, public), linked from the top bar, checklist, scanner, Check-in and Integrations tabs | Done |
| Health check `/api/v1/health` (database, late emails) for an uptime monitor; `STATUS_PAGE_URL` linked from help | Done |
| Status page itself (hosted service, see DEPLOY.md step 9) | To set up, outside the code |
| Birth date question and under-18 check: per-event "age is counted on" date (default the start date), Under 18 filter and badge for organizers and door staff, dashboard tile, Sheet column; legacy import brings birth dates (migration 016) | Done |
| Participant profiles (photo, headline, LinkedIn/GitHub/website, private or public to the event) and the participant map beta: per-event switch, opt-in live location (expires after 3 min, deleted on stop, going private or map off), feedback to organizers (migration 015) | Demo, for feedback |
| Own artwork as a pass template: upload PNG/JPEG/WebP, re-encoded and stored per org (migration 014); name, QR and pass ID placed on top; `+ Image` in the designer for logos | Done |
| Plans, usage and limits | Next |
| Stripe checkout, customer portal, webhooks | Not started |
| Gaps against the current admin dashboard; pilot rehearsal on staging | Not started |
| Security self-review; external review booked | Not started |

## Open questions (need a decision)

- **Hosting.** The spec says Google Cloud (Cloud Run, Cloud SQL); production
  is DigitalOcean App Platform with Supabase. M1 runs on either.
- **Product name.** `PRODUCT_NAME` defaults to "Event Pass" as a neutral
  placeholder; the spec's working title uses NASA's marks.
- **Maps and camera libraries.** The spec lists Leaflet and qr-scanner; the
  current build uses Google Maps and html5-qrcode.
- **Email provider.** Platform emails use the current SMTP settings until
  SES or Postmark is chosen.

## Spec corrections (current build as it is)

| Spec says | Current build |
| --- | --- |
| Shared staff key | Per-person staff accounts: bcrypt, JWT, roles superadmin/admin/scanner |
| Fixed actions (confirm, check in, kit...) | Configurable `scan_scenarios` with per-staff grants |
| Scanner offline queue "later" | Exists: Dexie manifest, queued scans, claims sync, heartbeats |
| One Gmail account over SMTP | Queued email worker with daily limit, pause and interval |
| Registration form | `custom_attributes` JSONB + `participant_field_definitions` |
| QR token signing | HMAC-SHA256 over `membership_id:email`; the legacy verifier needs that key |
