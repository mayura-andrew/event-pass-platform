# Deploying to DigitalOcean App Platform (with Supabase and Gmail SMTP)

The API runs as one $5/month App Platform container in Bangalore (`blr`), the
database is a Supabase project in Mumbai, and staff credential emails go out
through a Gmail account. App Platform gives the app a free
`https://<name>.ondigitalocean.app` address, and HTTPS is required: phone
browsers only allow the camera on secure pages.

You need: a GitHub account, a DigitalOcean account, a Supabase account, and a
Gmail (or Google Workspace) account with 2-Step Verification on. Allow about
an hour the first time.

---

## 1. Put the code on GitHub

Create an **empty private** repository named `nsac-qr` on GitHub (no README,
no .gitignore), then from this folder:

```sh
git remote add origin git@github.com:<your-user>/nsac-qr.git
git push -u origin main
```

`.env` is in `.gitignore` and never leaves your machine.

## 2. Create the Supabase database

1. supabase.com → **New project**. Region: **South Asia (Mumbai)**. Save the
   database password somewhere safe.
2. **SQL Editor** → run each file's contents, in this order:
   `migrations/001_schema.sql`, `migrations/002_dashboard_schema.sql`,
   `migrations/003_rbac_and_emailing.sql`, `migrations/004_auto_membership_id.sql`,
   `migrations/005_jsonb_dynamic_schema.sql`, `migrations/006_email_queue_schema.sql`.
   Each is safe to run twice.
3. **Project Settings → Database → Connection string → Session pooler**
   (port 5432). Copy it and put your password in it. If the password has
   symbols such as `@`, `#` or `/`, URL-encode them (`@` → `%40`).

   Use the **pooler** address (`aws-0-ap-south-1.pooler.supabase.com`), not
   the direct `db.<ref>.supabase.co` one: the direct host is IPv6-only and App
   Platform connects over IPv4.

> Supabase's free plan pauses a project after 7 days without activity. Open
> the project (or upgrade) in the week before the event.

## 3. Load venues, delegates and the first superadmin (from your laptop)

```sh
export DATABASE_URL='postgres://postgres.<ref>:<password>@aws-0-ap-south-1.pooler.supabase.com:5432/postgres?sslmode=require'

# A NEW secret for production. Store it in a password manager: it must stay
# the same until the event ends, or every pass already sent stops verifying.
export HMAC_SECRET_KEY=$(openssl rand -hex 32)

go run ./scripts/seed -venues venues.csv -participants participants.csv -dry-run   # check first
go run ./scripts/seed -venues venues.csv -participants participants.csv
```

Passes from your local test database won't work in production: they were
signed with a different key.

### The first superadmin

Every other account is created later from the dashboard's Staff page, which
emails credentials. Only this first one is made by hand. Two ways:

**A. From your laptop (recommended).** Nothing is written down anywhere:

```sh
go run ./scripts/create-admin -email you@example.org -name "Your Name" -role superadmin
```

It prints the database it is about to change (check it says the Supabase
pooler host, not `127.0.0.1`), asks you to confirm, then asks for the password
twice at a hidden prompt (12+ characters for a superadmin). Add `-temporary`
when you create it for someone else: they must choose their own password at
first sign-in. Check the result with:

```sh
go run ./scripts/create-admin -list
```

**B. From the Supabase SQL editor (no Go needed).** Open
`scripts/sql/bootstrap_superadmin.sql`, replace the three values marked
`CHANGE ME`, paste it into **SQL Editor** and run it. It returns one row when
the account is saved; no row means a value was left unedited or the password
is shorter than 12 characters. Because the editor keeps a query history, the
password is always temporary: sign in at `/admin/` and choose your own
straight away, and the one in the history stops working.

Your local test accounts (`superadmin@test.local` and the others) stay in
your local database; production starts with only this account.

## 4. Create a Gmail app password

Gmail refuses your normal password over SMTP; it needs an app password.

1. Google Account → **Security** → turn on **2-Step Verification** (required).
2. Open <https://myaccount.google.com/apppasswords>, name it `NSAC QR`, and
   copy the 16-character password. Remove the spaces.
3. Google Workspace accounts: if App passwords is missing, the Workspace
   admin has to allow it (Admin console → Security → Less secure apps /
   App passwords).

Use the same Gmail address for `SMTP_USERNAME` and in `SMTP_FROM`; Gmail
replaces any other From address with the signed-in account. Gmail allows about
500 messages a day (2,000 on Workspace), which is far more than staff
provisioning needs.

## 5. Deploy with GitHub Actions

`.github/workflows/deploy.yml` runs `go vet` and `go test` on every push and
pull request. On `main`, once they pass, it applies `.do/app.yaml` to App
Platform: it creates the app the first time and updates it after that. App
Platform then builds the Dockerfile and deploys.

1. **Connect GitHub to DigitalOcean (once).** Control panel → **Apps →
   Create App → GitHub** and authorize DigitalOcean to read the repository
   (for an organization repo, grant access to that organization). You can
   leave the wizard once the repository shows up in the list; the workflow
   creates the app itself.
2. **Create a DigitalOcean API token.** **API → Tokens → Generate New Token**,
   with read and write access to **App Platform** (custom scopes: `app`
   create/read/update), and an expiry later than the event.
3. **Add the repository secrets.** GitHub → the repository → **Settings →
   Secrets and variables → Actions → New repository secret**:

   | Secret | Value |
   | --- | --- |
   | `DIGITALOCEAN_ACCESS_TOKEN` | The token from step 2 |
   | `DATABASE_URL` | The Session pooler string from step 2 of this guide |
   | `JWT_SECRET` | `openssl rand -hex 32` (a new one, not your local value) |
   | `HMAC_SECRET_KEY` | The production key from step 3, the one the seed script used. It signs passes for delegates added on **Add delegates** |
   | `SMTP_USERNAME` | `yourname@gmail.com` |
   | `SMTP_PASSWORD` | The 16-character app password |
   | `SMTP_FROM` | `NASA Space Apps Colombo <yourname@gmail.com>` |
   | `GOOGLE_MAPS_API_KEY` | Optional; restrict it to the app's URL |

   The workflow passes them into `.do/app.yaml`, where they are stored
   encrypted (`type: SECRET`). It refuses to deploy if a required one is
   missing, because a blank secret would replace the value in production.
   Values can't contain a single quote (`'`).

   The rest (`SMTP_HOST=smtp.gmail.com`, `SMTP_PORT=587`,
   `SMTP_SECURITY=starttls`, `CLIENT_IP_HEADER=DO-Connecting-IP`,
   `APP_BASE_URL=${APP_URL}`, …) is in the spec itself.
4. **Run it.** Push to `main`, or GitHub → **Actions → Test and deploy → Run
   workflow**. The first run creates the app `nsac-qr` and takes a few
   minutes; the build and deploy logs are printed in the job, and the last
   line gives the app's URL.

> **`.do/app.yaml` is the source of truth.** Every deploy replaces the app's
> settings with that file, so a change made in the control panel (an
> environment variable, the instance size, a domain) is undone by the next
> push. Make the change in `.do/app.yaml`, or for a secret in the GitHub
> secret, and push.

## 6. Check the deployment

1. `https://<app>.ondigitalocean.app/healthz` returns an empty 200.
2. **Runtime Logs** (Apps → nsac-qr → Runtime Logs) within ~30 s of start:

   | Log line | Meaning | Fix |
   | --- | --- | --- |
   | `SMTP check passed: connected and signed in` | Email works | Nothing |
   | `SMTP CHECK FAILED` … `i/o timeout` / `connection refused` | DigitalOcean is blocking outbound SMTP for your account | Open a DigitalOcean support ticket asking to allow outbound SMTP on port 587 for this app, or switch to an HTTP email API (Resend, Brevo, Postmark); the code change is small |
   | `SMTP CHECK FAILED` … `smtp auth` / `535` | Wrong username or app password | Re-create the app password; check there are no spaces |
   | `DATABASE_URL` / `ping database` errors | Wrong connection string | Use the Session pooler string, password URL-encoded |

3. Sign in at `/admin/` as the superadmin from step 3.
4. **Checkpoints** → create or confirm your scenarios (the starter four come
   from migration 001).
5. **Staff → Provision staff** → create a scanner account for yourself with a
   real address. The email should arrive within a minute; sign in on your
   phone at `/scanner.html` with it.

## 7. Email delegates their passes

Once delegates are loaded and confirmed: **Dashboard → Pass emails → Bulk
enqueue confirmation passes**. The server sends one every 2 seconds (about 10
minutes for 300) and the page shows progress; you can pause at any time.

- Send yourself one first: enter your own test delegate's membership ID under
  **Send or resend one pass** and check it arrives (and isn't in spam).
- A personal Gmail account sends about 500 emails a day, including staff
  credential emails. The queue stops at 450 (`EMAIL_DAILY_LIMIT`) and
  continues as the 24-hour window moves; with more delegates than that, use a
  Google Workspace account (about 2,000 a day) and raise the limit in
  `.do/app.yaml`.
- If the page says sending stopped by itself, read the reason: a wrong app
  password or Gmail's daily cap. Fix it and press **Resume sending**.

## 8. Your own domain (optional)

Add the domain to `.do/app.yaml` and push (a domain added only in the control
panel is removed by the next deploy):

```yaml
domains:
  - domain: pass.example.org
    type: PRIMARY
```

Then create the DNS record shown under Apps → Settings → **Domains**. When
it's active, set `APP_BASE_URL` in the spec to `https://pass.example.org` so
emailed sign-in links use it.

## Updating

- `git push` to `main` deploys automatically once the tests pass
  (**Actions → Test and deploy**). A failing test stops the deploy.
- To change a secret, update the GitHub secret, then re-run the workflow
  (**Actions → Test and deploy → Run workflow**).
- Changed anything under `public/`? Bump `CACHE_VERSION` in `public/sw.js` in
  the same commit, or phones keep the previous version until the background
  refresh lands.
- New migration? Apply it **before** pushing the code that needs it, either
  in the Supabase SQL editor or with `DATABASE_URL=... go run ./cmd/migrate up`
  (goose; it records applied versions, and 001-006 are safe to re-run, so
  the first run on an existing database is harmless). `007_tenancy.sql` adds
  the SaaS tables and creates the `nsac_tenant` role; the current build
  doesn't use them, so it can go in any time before the SaaS pages are used.
  `008_events_sharing.sql` (events, sharing, audit log) likewise. (`004_auto_membership_id.sql` must be in place before the
  **Add delegates** page is used, and `005_jsonb_dynamic_schema.sql` before
  this version starts: the API reads `custom_attributes` everywhere.)
- Avoid deploying while scanning is in progress. A deploy restarts the
  container: phones keep scanning offline and upload afterwards, but
  dashboards reconnect and in-memory rate limits reset.

## Settings worth knowing

| Variable | Default | When to change |
| --- | --- | --- |
| `LOGIN_RATE_PER_MIN` | 30 | Sign-ins per client IP per minute. Venue Wi-Fi puts every phone behind one IP; raise it if many volunteers sign in at the same moment |
| `PASS_RATE_PER_MIN` | 120 | Public pass lookups per IP per minute; raise if a crowd on venue Wi-Fi opens passes at once |
| `JWT_TTL` | 14h | Must cover a full event day offline |
| `DB_MAX_CONNS` | 10 | Database connections the API may open. In Supabase's session pooler each one holds a connection from the project's pool (Database Settings → Connection pooling → Pool Size): keep it below that, leaving room for the seed script and the SQL editor |
| `CHECKIN_SCENARIO_CODE` | CHECK_IN | If your gate scenario has another code |
| `EMAIL_SEND_INTERVAL` | 2s | Gap between pass emails. Longer if Gmail throttles (421 "Try again later" in the failure list) |
| `EMAIL_DAILY_LIMIT` | 450 | Pass emails per rolling 24 hours. Raise to ~1900 on Google Workspace; 0 turns the cap off |

Keep `instance_count` at 1: rate limits, the account cache and the live scan
feed live in the container's memory.
