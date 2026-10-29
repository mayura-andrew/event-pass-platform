#!/usr/bin/env bash
# Creates or updates the staging app on DigitalOcean from .do/staging.yaml,
# filling in its '${NAME}' secrets from a local env file that never goes in
# the repository.
#
#   scripts/staging-spec.sh [env-file]     # default ~/.config/nsac-qr/staging.env
#
# The env file holds DATABASE_URL, SMTP_USERNAME, SMTP_PASSWORD and
# optionally SMTP_FROM (defaults to SMTP_USERNAME). JWT_SECRET,
# PASS_SIGNING_KEYS and INTEGRATIONS_KEY are generated into it on the first
# run and reused after that: changing them signs everyone out, breaks every
# pass and loses Google connections. Needs doctl, signed in.
set -euo pipefail

env_file=${1:-$HOME/.config/nsac-qr/staging.env}
spec=$(dirname "$0")/../.do/staging.yaml
[ -f "$env_file" ] || { echo "missing $env_file (see the comment at the top of this script)" >&2; exit 1; }
chmod 600 "$env_file"

# Generate the app's own secrets once.
grep -q '^JWT_SECRET=' "$env_file" || echo "JWT_SECRET=$(openssl rand -hex 32)" >>"$env_file"
grep -q '^PASS_SIGNING_KEYS=' "$env_file" || echo "PASS_SIGNING_KEYS=1:$(openssl rand -base64 32)" >>"$env_file"
grep -q '^INTEGRATIONS_KEY=' "$env_file" || echo "INTEGRATIONS_KEY=$(openssl rand -base64 32)" >>"$env_file"

set -a
# shellcheck disable=SC1090
. "$env_file"
set +a
SMTP_FROM=${SMTP_FROM:-${SMTP_USERNAME:-}}
for name in DATABASE_URL SMTP_USERNAME SMTP_PASSWORD SMTP_FROM JWT_SECRET PASS_SIGNING_KEYS INTEGRATIONS_KEY; do
  value=${!name:-}
  [ -n "$value" ] || { echo "$name is empty in $env_file" >&2; exit 1; }
  case "$value" in *"'"*) echo "$name contains a single quote; remove it" >&2; exit 1 ;; esac
done
export SMTP_FROM

# Fill the placeholders into a private temporary copy; ${APP_URL} stays.
filled=$(mktemp)
trap 'rm -f "$filled"' EXIT
python3 - "$spec" "$filled" <<'PY'
import os, re, sys
src, dst = sys.argv[1], sys.argv[2]
text = open(src).read()
def fill(m):
    name = m.group(1)
    return "'" + os.environ[name] + "'" if name in os.environ and name != "APP_URL" else m.group(0)
open(dst, "w").write(re.sub(r"'\$\{([A-Z_]+)\}'", fill, text))
PY
doctl apps spec validate "$filled" >/dev/null

id=$(doctl apps list --format ID,Spec.Name --no-header | awk '$2 == "nsac-qr-staging" {print $1}')
if [ -z "$id" ]; then
  doctl apps create --spec "$filled" --format ID,DefaultIngress --wait=false
else
  doctl apps update "$id" --spec "$filled" --format ID,DefaultIngress --wait=false
fi
