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

for name in APP_DOMAIN DATABASE_URL JWT_SECRET PASS_SIGNING_KEYS INTEGRATIONS_KEY APP_BASE_URL; do
  if [ -z "${!name:-}" ]; then
    echo "$name is required in deploy/ec2/.env" >&2
    exit 1
  fi
done

case "$APP_BASE_URL" in
  https://*) ;;
  *) echo "APP_BASE_URL must use https:// when SMTP is configured" >&2; exit 1 ;;
esac

docker compose build api
docker compose run --rm --entrypoint /app/event-pass-migrate api up
docker compose up -d api caddy
docker compose ps
