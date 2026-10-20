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

## Next: M4 Check-in and workshops

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
