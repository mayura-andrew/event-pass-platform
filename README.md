# NASA Space Apps: offline-first QR delegate management

Delegates get a signed QR pass. Volunteers scan it on a phone that keeps working with no signal. Scans are uploaded when the connection returns, and the first scan to reach the server wins.

```
Google Form CSV ──> scripts/seed ──> Supabase (participants + HMAC signature)
Organizer ──> /admin/ingest.html ──┘  (one at a time or CSV upload; IDs NSAC-001000… generated;
                                        extra CSV columns -> custom fields, managed on /admin/fields.html)
                                          │
Participant ──> /pass.html ──> QR {"id","sig"} + venue map
                                          │
Volunteer phone ──> /scanner.html
   ├─ online:  downloads delegate list, scenarios and existing claims into IndexedDB
   ├─ scan:    lookup in IndexedDB, verify signature, check duplicates, queue
   ├─ sync:    POST /api/v1/scans/sync ──> one transaction, one savepoint per scan
   └─ report:  POST /api/v1/devices/heartbeat (queue counts, every minute online)
                                          │
Organizer ──> /admin/dashboard.html
   ├─ metrics, delegate filters (age, claim status, venue), manual override
   ├─ live scan stream (server-sent events)
   └─ checkpoint on/off, scanner phone health
Organizer ──> /admin/emails.html
   └─ queue pass emails (QR inline + pass link) ──> worker sends 1 every 2 s via Gmail SMTP
Superadmin ──> /admin/staff.html
   └─ provision staff ──> temporary password emailed ──> set own password at first sign-in
```

## Roles

| Role | Scanner app | Checkpoints | Dashboard | Staff page |
| --- | --- | --- | --- | --- |
| `superadmin` | yes | all | yes | view + create, edit, reset, deactivate |
| `admin` | yes | all | yes | view only |
| `scanner` | yes | only those granted in `staff_scenario_grants` | no | no |

Grants are enforced by the API, not only hidden on the phone: `GET /scenarios/active` returns only granted
checkpoints, and `POST /scans/sync` marks scans for any other checkpoint `INVALID`. Every request rechecks the
account (active, current role, temporary password) with a 30-second cache, so deactivating someone,
changing their role or resending their credentials reaches signed-in devices within 30 seconds rather
than when their JWT expires.

## Layout

```
cmd/api/main.go                     entry point, wiring, graceful shutdown
internal/config                     environment configuration
internal/domain                     shared types and sync status codes
internal/auth                       JWT issue/verify (HS256, algorithm pinned), QR pass signature
internal/repository/postgres        SQL: participants, scenarios, admins, scans
internal/service                    validation and business rules
internal/handler/http               routes, middleware, rate limiting, static files
migrations/001_schema.sql           tables, indexes, RLS, starter scenarios
migrations/002_dashboard_schema.sql date of birth, override audit, scanner heartbeats
migrations/003_rbac_and_emailing.sql roles (volunteer -> scanner), checkpoint grants, temporary passwords
migrations/004_auto_membership_id.sql membership ID sequence (NSAC-001000, ...)
migrations/005_jsonb_dynamic_schema.sql custom fields: participants.custom_attributes (JSONB) + field definitions
migrations/006_email_queue_schema.sql  pass email queue (email_jobs) and its pause switch
migrations/007_tenancy.sql          SaaS: accounts, sessions, organizations, members, invites, Row Level Security
migrations/008_events_sharing.sql   SaaS: events, sharing (people and links), stars, recent, audit log
scripts/seed                        CSV import + HMAC signing
scripts/create-admin                create scanner / dashboard logins
scripts/sample                      example participants.csv and venues.csv
public/scanner.html, js/scanner.js  volunteer scanner PWA
public/js/db.js                     Dexie schema, session, hydration
public/pass.html, js/pass.js        participant pass portal
public/js/pass-card.js              downloadable 70 x 110 mm pass (PNG + print-ready PDF) on the Colombo template
public/assets                       qr-template.pdf (source), pass-template.jpg (600 dpi render), colombo-mark.png
public/sw.js                        service worker (offline app shell)
public/admin/                       organizer dashboard (dashboard.html, app.js), staff page (staff.html,
                                    staff.js), add delegates (ingest.html, ingest.js), custom fields
                                    (fields.html, fields.js), pass emails (emails.html, emails.js),
                                    shared helpers (common.js), compiled CSS
scripts/tailwind                    Tailwind config + source CSS for the dashboard
public/vendor                       pinned Dexie 4.0.11, html5-qrcode 2.3.8, qrcodejs 1.0.0,
                                    node-qrcode 1.5.4 encoder core (pass card QR)
```

## Setup

Deploying for real (DigitalOcean App Platform + Supabase + Gmail)? Follow [DEPLOY.md](DEPLOY.md).

Requires Go 1.22 or newer. Run `go mod tidy` once to fetch dependencies and create `go.sum`.

1. **Database.** Run `migrations/001_schema.sql`, `002_dashboard_schema.sql`, `003_rbac_and_emailing.sql`, `004_auto_membership_id.sql`, `005_jsonb_dynamic_schema.sql` and `006_email_queue_schema.sql`, in that order, in the Supabase SQL editor. All are safe to re-run. 003 renames `volunteer` accounts to `scanner` and grants them every existing checkpoint, so nobody loses access on deploy.
2. **Environment.** Copy `.env.example` to `.env`, fill it in, then `set -a; source .env; set +a`.
3. **Venues and delegates.**
   ```sh
   go run ./scripts/seed -venues scripts/sample/venues.csv -participants scripts/sample/participants.csv -dry-run
   go run ./scripts/seed -venues venues.csv -participants participants.csv
   ```
   Re-running is safe: rows are upserted. Lines with problems are reported and skipped.

   The seed script imports delegates who **already have** membership IDs (from the registration form).
   To add delegates without IDs, use `/admin/ingest.html` instead (one at a time or a CSV upload): the
   server assigns `NSAC-001000`, `NSAC-001001`, … and signs each pass. That needs `HMAC_SECRET_KEY` set
   for the API too, with the same value the seed script uses.
4. **Logins.**
   ```sh
   go run ./scripts/create-admin -email lead@example.org -name "Event lead" -role superadmin
   ```
   Create the first superadmin this way (it confirms the target database and asks for the password at a
   hidden prompt; `-list` shows existing accounts). In production without Go, use
   `scripts/sql/bootstrap_superadmin.sql` in the Supabase SQL editor instead. After that, add staff from `/admin/staff.html`, which emails each
   person a temporary password. (`create-admin` still works for any role, but a `scanner` made there has no
   checkpoints until you assign some on the Staff page.)
5. **Run.** `go run ./cmd/api`, then open `/scanner.html`, `/pass.html` and `/admin/` (organizer dashboard; needs an `admin` or `superadmin` login).

The camera only works over HTTPS (localhost is exempt), so put the API behind TLS for real devices.

### Run locally with Docker

Docker Compose starts a local PostgreSQL database, applies the schema on first
startup, and runs the API on port 8080. The database is also available locally
on port 5433 for the seed and admin scripts. Set `JWT_SECRET` in `.env` first:

```sh
docker compose -f nsac-qr.compose.yaml up --build
```

A database volume created before a migration existed won't run it
automatically (init scripts only run on an empty volume). Apply pending ones
with the migration command, which records what it applied (goose):

```sh
DATABASE_URL='postgres://nsac:nsac-local-password@127.0.0.1:5433/nsac_qr?sslmode=disable' go run ./cmd/migrate up
```

Compose also starts [Mailpit](https://mailpit.axllent.org/), which catches every email the API sends
(staff credentials). Read them at `http://localhost:8025`. The Compose file pins the API to Mailpit on
purpose, so `SMTP_*` variables in your shell can't make local tests send real email.

Open `http://localhost:8080/pass.html`. Stop the containers with
`docker compose -f nsac-qr.compose.yaml down`; add `-v` to also remove the
local database volume.

## Email (staff credentials)

Set these for production (see `.env.example`). Any SMTP relay works: Google Workspace, Amazon SES,
Resend, Mailgun, Postmark.

| Variable | Example | Notes |
| --- | --- | --- |
| `APP_BASE_URL` | `https://pass.example.org` | Sign-in link in emails; required when `SMTP_HOST` is set |
| `SMTP_HOST` | `smtp.gmail.com` | Empty disables staff provisioning (the Staff page shows why) |
| `SMTP_PORT` | `587` | |
| `SMTP_SECURITY` | `starttls` | `starttls` (587), `tls` (465), or `none` (local test servers only; refused with a username) |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | | For Gmail, an app password |
| `SMTP_FROM` | `Space Apps Colombo <no-reply@example.org>` | Must be an address the relay may send as |
| `EVENT_NAME` | `NASA Space Apps Colombo` | Used in subject and body |
| `EMAIL_SEND_INTERVAL` | `2s` | Pass emails: one per interval (2 s = 30 a minute, 300 in about 10 minutes). At least `1s` |
| `EMAIL_DAILY_LIMIT` | `450` | Pass emails per rolling 24 hours; `0` for no cap. Gmail allows ~500 (Workspace ~2,000), shared with staff emails |

### Delegate pass emails

`/admin/emails.html` queues a confirmation email for every confirmed delegate who hasn't had one
(**Bulk enqueue**), or for one delegate by membership ID (also a resend). Each email has the delegate's
QR pass embedded as an image (the same `{"id","sig"}` payload as the pass page, so scanners accept it),
membership ID, team, venue, and a link that opens their pass on `/pass.html`.

The queue lives in `email_jobs`, so it survives restarts and deploys. A worker in the API sends one
message per `EMAIL_SEND_INTERVAL`, claiming jobs with `FOR UPDATE SKIP LOCKED`:

- **Transient failures** (network, 4xx) retry up to 3 times, a minute apart.
- **Rejected addresses** (5xx, e.g. no such mailbox) fail at once and are listed with the error; fix the
  address and press Retry.
- **Problems with the sending account** (wrong app password, Gmail's daily cap) don't count against the
  delegate: the job goes back in line and the queue **pauses itself** with the reason shown. So does
  any run of 5 failures in a row. Resume from the page once it's fixed.
- The worker stops sending at `EMAIL_DAILY_LIMIT` messages per 24 hours and carries on by itself as
  the window moves.
- A message interrupted by a restart mid-send is retried; that delegate might get it twice.

Pause and resume are stored in the database, so a paused queue stays paused through a deploy. A
delegate is never queued twice at the same time, so pressing Bulk enqueue again is harmless.

The email is sent inside the account's database transaction. If it can't be sent, nothing is created
and the superadmin sees why (HTTP 503). Temporary passwords are 12 characters, without look-alike
characters (0/O, 1/l/I). They only work for setting a new password (any other request gets
`403 PASSWORD_CHANGE_REQUIRED`).

## CSV formats

`participants.csv`: `membership_id,email,full_name,team_name,venue_name,hall_section,date_of_birth`
(`venue_name` + `hall_section` must match a row in `venues`.)

`date_of_birth` is optional, `YYYY-MM-DD` by default. For another format pass a Go layout, e.g.
`-dob-layout 1/2/2006` for a US-locale Google Sheets export. Check the export's locale first:
`3/4/2008` is March in one and April in the other. Blank or unreadable dates are reported and stored as
unknown (the delegate is still imported), and a blank cell never erases a date already stored. Ages are
calculated from the database's `CURRENT_DATE` (UTC on Supabase).

`venues.csv`: `name,hall_section,latitude,longitude,description`

Upload on `/admin/ingest.html` (no membership IDs; the server assigns them):
`full_name,email,team_name,date_of_birth,venue_id`. Only `full_name` and `email` are required;
`date_of_birth` is `YYYY-MM-DD`; `venue_id` is a venue's UUID (the page lists them and its sample
template fills one in). Up to 10,000 rows or 5 MB per file. Valid rows are added in one batch; invalid
rows and emails already registered are listed in the report and skipped, so uploading the corrected
file again adds only what's missing. A file with a `membership_id` column is refused, so delegates who
already have IDs aren't given new ones by mistake.

**Extra columns become custom fields.** Any column other than those five (say `food_preference`,
`T-Shirt Size`, `Parent's contact`) is stored per delegate in `participants.custom_attributes`, under a
key made from the header: lower case, accents folded, apostrophes and hyphens dropped, everything else
becomes `_` (`T-Shirt Size` -> `tshirt_size`). A header can also name an existing field by its label.
Blank cells are left out. A key seen for the first time is registered on `/admin/fields.html` as a text
field with **both display toggles off**; organizers then choose, per field:

- **Scanner popup**: shown as a badge after each scan. Only these fields' values are put in the scanner
  manifest, so other columns (a guardian's phone number) never reach a phone.
- **Dashboard filter**: adds a dropdown (common values) or a text box to the delegate table's filter bar.

A field's type (text, number, yes/no, date) can be changed until a delegate has a value; values are then
checked against it on every upload. Deleting a field (superadmin) erases its values too. Columns named
like a delegate column of the seed format (`venue_name`, `hall_section`) are ignored and reported.

**Updating registered delegates.** By default an email that's already registered is skipped. Tick
**Also update delegates who are already registered** (API: `?update_existing=true`) to update them
instead, matched by email: filled cells replace the stored value, blank cells keep it, and custom fields
are merged key by key. `full_name` becomes optional (still required for emails that turn out to be new),
so a file with just `email,tshirt_size` fills in T-shirt sizes for everyone. Email and membership ID never
change, so passes already sent keep working. The report lists each updated delegate with what changed,
and rows that changed nothing as "already up to date".

Header names are matched case-insensitively and spaces become underscores, so a Google Sheets export with "Full Name" works.

## API

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| POST | `/api/v1/auth/login` | public, 10/min/IP | Issue admin JWT |
| GET | `/api/v1/participants/pass?id=&email=` | public, 30/min/IP | Pass data for one delegate |
| GET | `/api/v1/config/public` | public | Maps key and Map ID for the portal |
| GET | `/api/v1/participants/manifest` | any admin | Full confirmed directory (ETag, gzip) |
| GET | `/api/v1/scenarios/active` | any account | Active checkpoints (only granted ones for scanners) |
| POST | `/api/v1/scans/sync` | any admin | Upload up to 500 scans |
| GET | `/api/v1/scans/claims?since=` | any admin | Claims made by other devices |
| GET | `/api/v1/scenarios` | admin, superadmin | All scenarios |
| POST | `/api/v1/scenarios` | admin, superadmin | Create scenario |
| PATCH | `/api/v1/scenarios/{id}` | admin, superadmin | Update scenario (partial) |
| POST | `/api/v1/devices/heartbeat` | any admin | Scanner reports its local queue counts |
| GET | `/api/v1/admin/dashboard/stats` | admin, superadmin | Metric cards, throughput, per-scenario and fleet totals |
| POST | `/api/v1/admin/participants/filter` | admin, superadmin | Delegate table: search, age group, claim status, venue, confirmation, paging, and `custom: [{"key","op","value"}]` (`op`: `eq`, `contains` for text, `missing`) |
| POST | `/api/v1/admin/emails/send-single` | admin, superadmin | `{"participant_id"}` or `{"membership_id"}`: queue (or resend) one pass email; 409 if already queued |
| POST | `/api/v1/admin/emails/send-bulk` | admin, superadmin | Queue every confirmed delegate without a sent or queued pass email |
| GET | `/api/v1/admin/emails/status` | admin, superadmin | Queue counts, daily allowance, pause state, estimate, recent failures |
| POST | `/api/v1/admin/emails/pause-toggle` | admin, superadmin | `{"paused": true}` or `{"paused": false}` |
| POST | `/api/v1/admin/participants/create` | admin, superadmin | `{"full_name","email","team_name","date_of_birth","venue_id"}`; returns the new membership ID and signature. 409 if the email is registered |
| POST | `/api/v1/admin/participants/upload-csv[?update_existing=true]` | admin, superadmin | CSV as multipart field `file` or `text/csv` body; returns `{total_rows, created, updated, unchanged, duplicates, errors, custom_fields, new_fields, ignored_columns}` |
| GET | `/api/v1/admin/fields` | admin, superadmin | Custom fields with usage counts and, for filter fields, common values |
| POST | `/api/v1/admin/fields` | admin, superadmin | `{"field_key","field_label","data_type","show_in_scanner_popup","show_in_dashboard_filter"}` |
| PATCH | `/api/v1/admin/fields/{key}` | admin, superadmin | Any of label, `data_type` (only while unused), the two toggles, `sort_order` |
| DELETE | `/api/v1/admin/fields/{key}` | superadmin | Removes the field and every delegate's value for it |
| GET | `/api/v1/fields/scanner` | any account | Labels of the fields shown after a scan (values come with the manifest) |
| POST | `/api/v1/admin/scenarios/toggle` | admin, superadmin | `{"scenario_id","is_active"}` |
| POST | `/api/v1/admin/scans/manual-override` | admin, superadmin | Record a scan without a QR; rationale required, 409 if already claimed |
| GET | `/api/v1/admin/devices/health` | admin, superadmin | Scanner phones with last reported queue |
| GET | `/api/v1/admin/venues` | admin, superadmin | Venue list for the filter |
| GET | `/api/v1/admin/scans/recent?limit=` | admin, superadmin | Newest scans (feed backlog) |
| GET | `/api/v1/admin/scans/stream` | admin, superadmin | Server-sent events, one `scan` event per new scan |
| POST | `/api/v1/auth/change-password` | signed in (also with a temporary password) | `{"current_password","new_password"}`, returns a new session |
| GET | `/api/v1/admin/staff/list` | admin, superadmin | Accounts with role, status and granted checkpoints |
| POST | `/api/v1/admin/staff/create` | superadmin | `{"full_name","email","role","scenario_ids"}`; emails a temporary password |
| PATCH | `/api/v1/admin/staff/{id}` | superadmin | Any of `full_name`, `role`, `is_active`, `scenario_ids` (replaces grants) |
| POST | `/api/v1/admin/staff/{id}/resend-credentials` | superadmin | New temporary password by email; the old password stops working |

### Filter body

```json
{"search": "orion", "age_group": "UNDER_18", "scenario_id": "<uuid>", "claim_status": "UNCLAIMED",
 "venue_id": "<uuid or UNASSIGNED>", "confirmation": "CONFIRMED", "page": 1, "page_size": 50}
```

Every field is optional. `age_group` is `UNDER_18`, `ABOVE_18` or `UNKNOWN` (no date of birth);
`claim_status` (`CLAIMED`/`UNCLAIMED`) requires `scenario_id`. `page_size` is capped at 200.

### Manual override

The admin is taken from the JWT, not the request. The scan goes through the same "first scan wins" index
as phone syncs: if the delegate already holds the claim the API returns 409 with `claimed_by` and
`claimed_at`, and a phone that later uploads the same claim gets `DUPLICATE_REJECTED`. Override scans
are stored with `entry_method = 'MANUAL_OVERRIDE'` and the reason in `override_reason`; the database
rejects an override with a reason under 10 characters.

### Sync result statuses

| Status | Meaning | Scanner action |
| --- | --- | --- |
| `SUCCESS` | Recorded | Mark synced |
| `ALREADY_SYNCED` | Same idempotency key already stored (retry after a lost response) | Mark synced |
| `DUPLICATE_REJECTED` | Another scan claimed it first; response names who and when | Mark rejected, remember the claim |
| `INVALID` | Bad IDs, unknown scenario, or key not bound to this admin | Stop retrying |
| `FAILED` | Transient database error | Retry later |

Idempotency keys are `{admin_id}_{participant_id}_{scenario_id}` for single-claim scenarios. Repeatable scenarios append `_{uuid}` so each scan is its own row. The server takes the admin from the JWT and rejects keys that don't match it.

## Event-day operations

- **Before doors open:** sign every phone in on venue Wi-Fi, pick the checkpoint, and confirm "Delegates on phone" shows the full count. Add the scanner to the home screen.
- **Changing scenarios:** turning a scenario off hides it from scanners on their next refresh. Scans already taken while it was active still upload.
- **Shared phones:** scans are uploaded by the volunteer who made them. If someone else signs in, the earlier volunteer's scans wait on the phone (the scanner says so) until that person signs in again.
- **After a deploy that changes `public/`:** bump `CACHE_VERSION` in `public/sw.js`, or phones keep the old scanner until the background refresh lands.
- **Changing the pass design:** the card background is `public/assets/pass-template.jpg`, rendered from `qr-template.pdf` with `pdftoppm -r 600 -jpeg -jpegopt quality=90 -singlefile public/assets/qr-template.pdf public/assets/pass-template`. If the boxes move, update the millimetre positions at the top of `public/js/pass-card.js`, then bump `CACHE_VERSION` in `public/sw.js`.
- **Changing the logo:** the official artwork is `public/assets/Colombo L.png` (black, for light
  backgrounds) and `Colombo LW.png` (white, for dark). The pass page and emails use smaller copies made
  from them: `colombo-logo.png` and `colombo-logo-white.png` (960 px wide) and `colombo-logo-email.png`
  (520 px, white, embedded in every email). After replacing the artwork, regenerate the copies (crop to
  the artwork, resize, reduce to a 48-colour palette; Pillow does it in a few lines), bump
  `CACHE_VERSION` in `public/sw.js`, and restart the API so emails pick up the new file.
- **Secrets:** keep `HMAC_SECRET_KEY` fixed for the whole event, and the same for the API and the seed script; rotating it invalidates every pass already sent.
- **Dashboard "Not reporting" phones:** a phone that hasn't reported for 2 minutes is shown with the last queue it reported. If it still holds scans, find the volunteer and get the phone onto Wi-Fi with the scanner open.
- **Dashboard styles:** `public/admin/dashboard.css` is compiled and committed, so the Go build needs no Node. After changing classes in `public/admin/`, rebuild it with the command at the top of `scripts/tailwind/tailwind.config.js`.

## Known limits

- "First scan wins" means first to **reach the server**, not first by device clock. A phone that was offline longer loses ties.
- Every scanner holds the full manifest including signatures. If a phone is lost, deactivate its account on the Staff page: within 30 seconds the phone is refused, and the next time it reaches the server it deletes the delegate list (unsent scans are kept). A phone that never comes back online keeps the list, so phones should have a screen lock.
- Rate limiting is in memory per API instance. For multiple replicas, rate-limit at the edge instead.
- Rate limits key on the socket address unless `CLIENT_IP_HEADER` names the header your load balancer sets to the real client IP (`DO-Connecting-IP` on App Platform). Never point it at a header clients can set themselves.
- The live stream is fed by one poller per API instance that reads new scans every 2 seconds. This works through every Supabase connection mode, including the transaction pooler. If a phone uploads more than 500 scans within one poll, the stream shows the newest 500. The scans are all stored; the dashboard table and counts include them.
- The stream holds one HTTP connection per open dashboard. A proxy in front of the API must allow long-lived responses and must not buffer `text/event-stream` (the API sends `X-Accel-Buffering: no` and a keep-alive every 20 s).
- Removing a checkpoint from a scanner while their phone is offline makes their queued scans for that checkpoint `INVALID` when the phone reconnects (shown in that phone's invalid count on the dashboard). Remove grants after the phone has synced.
- Custom field filters match with JSONB containment (`custom_attributes @> '{"key": value}'`), which the GIN index can serve; `->>`-style comparisons can't use it. At event sizes (a few thousand delegates) PostgreSQL scans the table anyway, in about 2 ms; the index starts to pay off in the hundreds of thousands of rows.
- An update-mode upload can change or add values but not clear one (a blank cell keeps the stored value). To remove a custom field's values, delete the field.
- Membership IDs can have gaps: numbers reserved for a batch that then hit an already-registered email are not reused. Generated IDs stop at `NSAC-999999`.
- A superadmin can't deactivate, demote or reset themselves from the Staff page, so there is always at least one superadmin.
- Phone queue counts are self-reported by each scanner, so the dashboard can only show what a phone last reported while it had signal.
