#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

if [ ! -f .env ]; then
  echo "Missing deploy/ec2/.env. Copy .env.example, fill it, and retry." >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
. ./.env
set +a

for name in APP_DOMAIN MIGRATE_DATABASE_URL DATABASE_URL WORKER_DATABASE_URL JWT_SECRET PASS_SIGNING_KEYS INTEGRATIONS_KEY APP_BASE_URL; do
  if [ -z "${!name:-}" ]; then
    echo "$name is required in deploy/ec2/.env" >&2
    exit 1
  fi
done

case "$APP_BASE_URL" in
  https://*) ;;
  *) echo "APP_BASE_URL must use https://" >&2; exit 1 ;;
esac

if [ "$PASS_SIGNING_KEYS" = "1:" ]; then
  echo "PASS_SIGNING_KEYS has no key yet (1:<openssl rand -base64 32>)" >&2
  exit 1
fi
if [ "$DATABASE_URL" = "$MIGRATE_DATABASE_URL" ]; then
  echo "DATABASE_URL must be the event_pass_api login, not the owner used for migrations" >&2
  exit 1
fi

docker compose build api worker
# Migrations run as the table owner; the API and worker never do.
docker compose run --rm -e DATABASE_URL="$MIGRATE_DATABASE_URL" --entrypoint /app/event-pass-migrate api up
docker compose up -d api worker caddy
docker compose ps
