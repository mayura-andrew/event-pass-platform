#!/usr/bin/env bash
# One-time EC2 host setup. Run on the instance as your normal user (not root):
#   curl -fsSL <raw url> | bash -s -- <git-repo-url>     or
#   bash setup.sh https://github.com/OWNER/REPO.git
# Supports Amazon Linux 2023 and Ubuntu. Safe to re-run.
set -euo pipefail

REPO_URL="${1:?usage: setup.sh <git-repo-url>}"
DEST="$HOME/event-pass-platform"

. /etc/os-release
case "$ID" in
  amzn)
    sudo dnf install -y docker git
    ;;
  ubuntu|debian)
    sudo apt-get update -y
    sudo apt-get install -y docker.io git curl
    ;;
  *) echo "Unsupported OS: $ID" >&2; exit 1 ;;
esac

sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"

# Compose v2 plugin (not packaged on Amazon Linux).
if ! sudo docker compose version >/dev/null 2>&1; then
  sudo mkdir -p /usr/local/lib/docker/cli-plugins
  arch=$(uname -m)
  sudo curl -fsSL \
    "https://github.com/docker/compose/releases/download/v2.29.7/docker-compose-linux-${arch}" \
    -o /usr/local/lib/docker/cli-plugins/docker-compose
  sudo chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
fi

# 2 GB swap so image builds don't run out of memory on small instances.
if [ ! -f /swapfile ]; then
  sudo fallocate -l 2G /swapfile
  sudo chmod 600 /swapfile
  sudo mkswap /swapfile
  sudo swapon /swapfile
  echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab >/dev/null
fi

if [ ! -d "$DEST/.git" ]; then
  git clone "$REPO_URL" "$DEST"
fi

# Build the .env from the example, pre-filling the domain from the public IP
# via sslip.io (no domain needed; Caddy gets a real certificate for it).
ENV_FILE="$DEST/deploy/ec2/.env"
if [ ! -f "$ENV_FILE" ]; then
  token=$(curl -fsS -X PUT http://169.254.169.254/latest/api/token \
    -H 'X-aws-ec2-metadata-token-ttl-seconds: 60' || true)
  ip=$(curl -fsS -H "X-aws-ec2-metadata-token: $token" \
    http://169.254.169.254/latest/meta-data/public-ipv4 || true)
  cp "$DEST/deploy/ec2/.env.example" "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  if [ -n "$ip" ]; then
    domain="${ip//./-}.sslip.io"
    sed -i "s|^APP_DOMAIN=.*|APP_DOMAIN=$domain|; s|^APP_BASE_URL=.*|APP_BASE_URL=https://$domain|" "$ENV_FILE"
    echo "Domain set to $domain (use an Elastic IP so it stays stable)."
  fi
  sed -i "s|^JWT_SECRET=.*|JWT_SECRET=$(openssl rand -hex 32)|; s|^PASS_SIGNING_KEYS=.*|PASS_SIGNING_KEYS=1:$(openssl rand -base64 32)|; s|^INTEGRATIONS_KEY=.*|INTEGRATIONS_KEY=$(openssl rand -base64 32)|" "$ENV_FILE"
fi

cat <<EOF

Setup done. Next:
  1. Log out and back in (docker group), then:
  2. Edit $ENV_FILE: RDS URLs, SMTP settings. Secrets are already generated.
  3. Run:  bash $DEST/deploy/ec2/deploy.sh
  4. Create the event_pass_app / event_pass_jobs RDS logins (see .env.example),
     then deploy.sh again.
Security group: allow 80 and 443 from anywhere, 22 from your IP only.
EOF
