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

Open:

- http://localhost:8080/login
- http://localhost:8025 for captured development email

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

## Render deployment with Gmail SMTP

This repository includes [render.yaml](./render.yaml), which provisions a
managed PostgreSQL database, runs migrations before each deploy, and starts
the API with the production Docker image.

1. In Render, choose **New > Blueprint** and connect this repository.
2. Set `APP_BASE_URL` to the final Render URL, for example
   `https://event-pass-platform.onrender.com`.
3. Generate a Gmail App Password in the Google account used for sending mail.
   Do not use the normal Google account password. Two-step verification must
   be enabled.
4. Set these Render environment variables:
   - `SMTP_USERNAME`: the Gmail address
   - `SMTP_PASSWORD`: the 16-character Gmail App Password
   - `SMTP_FROM`: `Event Pass Platform <your-gmail-address>`
   - `PASS_SIGNING_KEYS`: `1:<base64-encoded-32-byte-secret>`
   - `INTEGRATIONS_KEY`: a base64-encoded 32-byte secret
5. In Gmail, ensure the `SMTP_FROM` address is the authenticated account or a
   verified “Send mail as” alias.

The outbox worker runs inside the web service and sends participant emails
through Gmail. Gmail's daily sending limit is protected by
`EMAIL_DAILY_LIMIT=450`; use a transactional email provider for larger events.

## Tests

```sh
go test ./...
```

Integration tests run when `EVENT_PASS_TEST_DATABASE_URL` points to a migrated
PostgreSQL database.
