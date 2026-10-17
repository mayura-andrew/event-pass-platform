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
   `migrations/003_rbac_and_emailing.sql`. Each is safe to run twice.
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

ADMIN_PASSWORD='<a long password>' go run ./scripts/create-admin \
  -email you@example.org -name "Your Name" -role superadmin
```

Passes from your local test database won't work in production: they were
signed with a different key.

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

## 5. Create the App Platform app

1. In `.do/app.yaml`, replace `YOUR_GITHUB_USER/nsac-qr` with your repository
   and commit/push the change.
2. DigitalOcean control panel → **Apps → Create App → GitHub**. The first time,
   authorize DigitalOcean to read the repository.
3. Create the app from the spec. Either:
   - Command line: `doctl auth init` (with a token from the DigitalOcean
     account you want to bill), then `doctl apps create --spec .do/app.yaml`;
   - or the control panel: pick the repo and branch `main`, and set the values
     from `.do/app.yaml` by hand (Dockerfile build, HTTP port 8080, health
     check `/healthz`, region Bangalore, plan Basic $5).
4. **Settings → api → Environment Variables**: fill in the secrets and tick
   **Encrypt** on each:

   | Variable | Value |
   | --- | --- |
   | `DATABASE_URL` | The Session pooler string from step 2 |
   | `JWT_SECRET` | `openssl rand -hex 32` (a new one, not your local value) |
   | `SMTP_USERNAME` | `yourname@gmail.com` |
   | `SMTP_PASSWORD` | The 16-character app password |
   | `SMTP_FROM` | `NASA Space Apps Colombo <yourname@gmail.com>` |
   | `GOOGLE_MAPS_API_KEY` | Optional; restrict it to the app's URL |

   The rest (`SMTP_HOST=smtp.gmail.com`, `SMTP_PORT=587`,
   `SMTP_SECURITY=starttls`, `CLIENT_IP_HEADER=DO-Connecting-IP`,
   `APP_BASE_URL=${APP_URL}`, …) comes from the spec.
5. Save. The app rebuilds and deploys (a few minutes).

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

## 7. Your own domain (optional)

Apps → Settings → **Domains → Add domain**, then create the DNS record it
shows. When it's active, set `APP_BASE_URL` to `https://your.domain` so emailed
sign-in links use it.

## Updating

- `git push` to `main` deploys automatically (`deploy_on_push`).
- Changed anything under `public/`? Bump `CACHE_VERSION` in `public/sw.js` in
  the same commit, or phones keep the previous version until the background
  refresh lands.
- New migration? Run it in the Supabase SQL editor **before** pushing the code
  that needs it.
- Avoid deploying while scanning is in progress. A deploy restarts the
  container: phones keep scanning offline and upload afterwards, but
  dashboards reconnect and in-memory rate limits reset.

## Settings worth knowing

| Variable | Default | When to change |
| --- | --- | --- |
| `LOGIN_RATE_PER_MIN` | 30 | Sign-ins per client IP per minute. Venue Wi-Fi puts every phone behind one IP; raise it if many volunteers sign in at the same moment |
| `PASS_RATE_PER_MIN` | 120 | Public pass lookups per IP per minute; raise if a crowd on venue Wi-Fi opens passes at once |
| `JWT_TTL` | 14h | Must cover a full event day offline |
| `CHECKIN_SCENARIO_CODE` | CHECK_IN | If your gate scenario has another code |

Keep `instance_count` at 1: rate limits, the account cache and the live scan
feed live in the container's memory.
