# Current system implementation

This document describes the application as it is implemented in this
repository. It is an architecture and operations overview, not an AWS
deployment guide.

## What the system does

Event Pass Platform is a Go web application for organizations to create and
manage events. Organizers can publish registration pages, collect participant
registrations, issue passes, and manage check-in. Participants can register and
view their passes through the public pages and pass portal.

The application is a single Go API process backed by PostgreSQL. It serves
server-rendered HTML, static files, and JSON endpoints. Browser JavaScript
provides interactive features such as dashboards, event editing, pass design,
and check-in scanning.

## High-level architecture

```text
Browser
  ├── HTML pages and JSON requests ──> Go API (cmd/api)
  ├── static PWA files ──────────────> Go API / public directory
  └── embedded page assets ─────────> Go API / web directory

Go API
  ├── HTTP handlers and middleware
  ├── services (application and business logic)
  ├── PostgreSQL repositories
  ├── PostgreSQL database
  └── optional SMTP and Google APIs

cmd/migrate ─────────────────────────> PostgreSQL migrations
```

In the deployed container, the API listens on port `8080` by default. It
expects PostgreSQL and external services to be reachable from its runtime
environment.

## Main code areas

- `cmd/api`: loads configuration, opens PostgreSQL, constructs services and
  repositories, starts the HTTP server and background workers.
- `cmd/migrate`: applies or reports database migrations.
- `internal/handler/http`: routes, request handling, authentication/session
  behavior, middleware, and HTML page rendering.
- `internal/service`: business workflows such as accounts, organizations,
  events, registration, passes, check-in, profiles, and integrations.
- `internal/repository/postgres`: PostgreSQL queries and transaction handling.
- `internal/domain`: shared models, roles, validation concepts, and errors.
- `internal/passes`: pass token support and pass image rendering.
- `migrations`: versioned PostgreSQL schema and tenant security policies.
- `web/templates` and `web/static`: embedded HTML templates, CSS, and browser
  JavaScript.
- `public`: files served as the public portal/PWA, including the web manifest
  and offline scanner dependencies.

## Application workflows

### Sign-in and organizations

Accounts sign in using Google OAuth when configured, or email-based sign-in
links through the configured mail service. Users can belong to organizations,
where organization membership and event sharing determine what they can
manage. Organization creation is restricted to the addresses in
`PLATFORM_ADMIN_EMAILS`; if that setting is empty, the app does not allow
organization creation through the application.

### Event registration and passes

Organizers create events and configure dates, capacity, visibility, and
registration form fields. Public event pages accept registrations according to
the event's configured rules, including approval and waitlist behavior.

Successful registrations are stored in PostgreSQL. The application creates
signed pass tokens and renders pass images. When SMTP is configured,
participant emails are queued and processed asynchronously by the API process.
The participant portal lets a pass holder access their passes.

### Check-in and live updates

Event staff use the scanner and check-in pages to validate participants,
record attendance, and manage workshops and checkpoints. Check-in data is
persisted in PostgreSQL. A live-feed service runs in the API process to support
updates in the application.

### Templates, integrations, and participant profiles

Pass templates support pass design and previews. Google Forms import and
Google Sheets synchronization are optional integrations; they require Google
OAuth configuration and encrypted-token key configuration. Participant
profiles and event map features are also part of the current application.

## Database and tenant isolation

PostgreSQL is the system of record. The API connects using `DATABASE_URL`, and
the database connection pool is capped by `DB_MAX_CONNS` (default `10`).

Schema changes are SQL files embedded into the migration binary. Run
`/app/event-pass-migrate up` to apply pending migrations. The migration history
starts with the existing schema and continues through migrations `007`–`016`,
which add multi-tenancy, events, registration, check-in, templates,
integrations, pass assets, participant profiles, and birth dates.

Tenant isolation has a database enforcement layer. Tenant repository
operations run in transactions that switch to the `event_pass_tenant` role and
set the current organization and account context. PostgreSQL Row Level
Security (RLS) policies use that context to constrain tenant-scoped records.
Consequently, database setup must allow the migrations to create the role and
grant it to the migration connection's database user. The API database user
also needs to be able to switch to that role.

## Runtime configuration

Configuration is read from environment variables at startup. The deployment
must provide `DATABASE_URL` and a `JWT_SECRET` of at least 32 bytes. In
production, also configure:

- `APP_BASE_URL` as the public HTTPS address when sign-in links, Google OAuth,
  or email are enabled.
- `PASS_SIGNING_KEYS` as the pass-token signing key ring.
- `INTEGRATIONS_KEY` when using Google integrations.
- SMTP settings when the app needs to send sign-in and participant emails.
- `PLATFORM_ADMIN_EMAILS` for the accounts permitted to create organizations.

Google OAuth, Google Maps, map tile settings, and status-page links are
optional. The complete variable list and development defaults are in
`.env.example`. Production secrets should be stored in a secrets manager or
protected runtime configuration, not committed to the repository or baked
into the container image.

## HTTP server and background work

`cmd/api` constructs one HTTP server and starts it alongside the live event
feed and Google integration worker. When SMTP is configured, it also starts
the participant-email outbox worker. These workers run inside the same API
process; there is no separate worker service in the current container
definition.

The router serves `/healthz` as a basic liveness endpoint. `GET
/api/v1/health` is the deeper health check: it reports database availability
and email queue status, and returns an unavailable response when the database
cannot be reached. The server handles termination signals and attempts a
graceful shutdown.

## Container and deployment files

The root `Dockerfile` builds two binaries in a Go build stage:

- `event-pass-api`, the web application.
- `event-pass-migrate`, the schema migration command.

The final image contains these binaries and the `public` directory, runs as a
non-root user, and exposes port `8080`. HTML templates and browser assets in
`web` are embedded in the Go application.

`event-pass.compose.yaml` is for local development: it starts PostgreSQL,
the API, and Mailpit for capturing test emails. `render.yaml` and `.do/`
describe existing Render and DigitalOcean deployment configurations. The
GitHub Actions workflow tests the project and has a DigitalOcean deployment
job. There is not currently an AWS-specific deployment configuration in this
repository.

For an EC2 + RDS deployment, EC2 would run the existing container and RDS
would provide PostgreSQL. The app itself does not provision AWS resources;
network access, TLS termination, secrets delivery, database backups, and
deployment automation must be configured outside the application.

## Current scope and operational considerations

- PostgreSQL is required; the application does not include an embedded or
  in-memory production database.
- SMTP and Google integrations are external dependencies and are disabled or
  unavailable when their settings are omitted.
- Email delivery and integration work currently run in the API process.
- The app is designed to run as one API instance or as multiple instances
  sharing PostgreSQL, but the included deployment files do not configure AWS
  scaling or high availability.
- The application has no AWS-specific code requirement. Its AWS deployment
  depends on supplying the same runtime configuration and connectivity it
  expects in other environments.
