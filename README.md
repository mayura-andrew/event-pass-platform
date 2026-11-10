# Event Pass Platform

General-purpose event registration and pass management for organizations.

## Features

- Multi-organization event management and sharing
- Public registration at `/e/{organization}/{event}`
- Open or approval-based registration, dates, capacity, and waitlists
- Custom questions with required fields and selectable options
- Participant pass emails, `/me` pass portal, PNG/PDF downloads
- Workshop registration and staff check-in
- Pass design templates and previews
- Google Forms import and Google Sheets synchronization
- Participant profiles, event maps, and age-based registration rules

## Local development

Requires Go 1.22 or newer and Docker Compose.

```sh
cp .env.example .env
openssl rand -hex 32
# Put the generated value in JWT_SECRET in .env.
docker compose -f event-pass.compose.yaml up --build
```

The API reads SMTP settings from `.env`. The example values route email to the
local Mailpit service; view captured messages at http://localhost:8025. To use
a real SMTP provider instead, set its host, port, security mode, username, and
password in `.env`.

Open:

- http://localhost:8080/login
- http://localhost:8025 for captured development email when using Mailpit

The local Compose database runs on `localhost:5433`. For an existing database,
apply migrations with:

```sh
DATABASE_URL='postgres://event_pass:event-pass-local-password@127.0.0.1:5433/event_pass?sslmode=disable' \
  go run ./cmd/migrate up
```

## Project layout

- `cmd/api`: HTTP server and dependency wiring
- `internal/domain`: platform models and business types
- `internal/handler/http`: API and HTML routes
- `internal/service`: registration, pass, event, integration, and check-in logic
- `internal/repository/postgres`: PostgreSQL persistence and tenant isolation
- `internal/passes`: pass token signing and rendering
- `migrations`: versioned PostgreSQL schema
- `web/templates`: organizer and participant pages

## Configuration

Copy [.env.example](./.env.example) and set `DATABASE_URL`, `JWT_SECRET`, and
`APP_BASE_URL`. Configure SMTP and Google OAuth only when those integrations
are needed. `PRODUCT_NAME` controls user-facing platform branding.

## Render deployment with Brevo SMTP

This repository includes [render.yaml](./render.yaml), which runs migrations
before each deploy and starts the API with the production Docker image. The
Blueprint expects an external PostgreSQL connection, such as Aiven.

1. In Render, choose **New > Blueprint** and connect this repository.
2. Set `DATABASE_URL` to the Aiven PostgreSQL connection string. Use the
   **Service URI** from Aiven and keep its TLS settings, typically including
   `sslmode=require`. Do not commit this value to the repository.
3. Set `APP_BASE_URL` to the final Render URL, for example
   `https://event-pass-platform.onrender.com`.
4. In Brevo, verify the sender address or domain and create an SMTP key.
5. Set these Render environment variables:
   - `SMTP_USERNAME`: the Brevo SMTP login shown in Brevo
   - `SMTP_PASSWORD`: the Brevo SMTP key, not the Brevo API key
   - `SMTP_FROM`: `Event Pass Platform <verified-sender@example.com>`
   - `PASS_SIGNING_KEYS`: `1:<base64-encoded-32-byte-secret>`
   - `INTEGRATIONS_KEY`: a base64-encoded 32-byte secret
6. In Gmail, ensure the `SMTP_FROM` address is the authenticated account or a
   verified “Send mail as” alias.

Render runs `/app/event-pass-migrate up` before starting the web service, so
the Aiven database must be reachable from Render and the Aiven database user
must have permission to create tables, indexes, roles, and RLS policies.
Migrations are embedded in the image and are applied automatically on deploy.

The outbox worker runs inside the web service and sends participant emails
through Brevo. The SMTP port is set to `2525` to avoid common cloud-provider
restrictions on ports 465 and 587. Brevo's sending limits and sender
verification rules still apply. The configured
`EMAIL_DAILY_LIMIT=450`; use a transactional email provider for larger events.

## Tests

```sh
go test ./...
```

Integration tests run when `EVENT_PASS_TEST_DATABASE_URL` points to a migrated
PostgreSQL database.
