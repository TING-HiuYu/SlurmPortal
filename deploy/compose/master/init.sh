#!/bin/sh
# One-time setup for the container master: .env with fresh secrets, the sync CA and the master's
# server certificate, and the data directory. Safe to rerun: it never replaces existing secrets.
#   ./init.sh portal.example.org [extra sync names...]
# Extra names (DNS names or IPs) are added to the certificate slaves connect to on the sync port.
set -eu
cd "$(dirname "$0")"
[ "$#" -ge 1 ] || { echo "usage: $0 PORTAL_HOST [extra sync names...]" >&2; exit 2; }
host=$(printf '%s' "$1" | sed -E 's#^[a-z]+://##; s#[:/].*$##')
shift
[ -n "$host" ] || { echo "give the portal's host name, e.g. portal.example.org" >&2; exit 2; }

rand() { openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-"$1"; }

umask 077
if [ ! -f .env ]; then
  sed -e "s#^PORTAL_HOST=.*#PORTAL_HOST=$host#" \
      -e "s#^PORTAL_UID=.*#PORTAL_UID=$(id -u)#" -e "s#^PORTAL_GID=.*#PORTAL_GID=$(id -g)#" \
      -e "s#^BOOTSTRAP_ADMIN_PASSWORD=.*#BOOTSTRAP_ADMIN_PASSWORD=$(rand 20)#" \
      -e "s#^JWT_KEYS=.*#JWT_KEYS=k1:$(openssl rand -base64 32)#" \
      -e "s#^PKI_MASTER_KEY=.*#PKI_MASTER_KEY=$(openssl rand -base64 32)#" \
      -e "s#^S3_ACCESS_KEY=.*#S3_ACCESS_KEY=portal$(rand 16)#" \
      -e "s#^S3_SECRET_KEY=.*#S3_SECRET_KEY=$(rand 40)#" \
      .env.example > .env
  echo "created .env (first admin: admin / $(sed -n 's/^BOOTSTRAP_ADMIN_PASSWORD=//p' .env))"
else
  echo ".env exists; leaving its secrets alone"
fi

mkdir -p data/master data/s3 data/s3-logs data/caddy secrets/certs
issue=../../certs/issue.sh
[ -f secrets/certs/ca.key ] || sh "$issue" init-ca secrets/certs
[ -f secrets/certs/master.crt ] || sh "$issue" master secrets/certs $(printf '%s\n' "$host" "$@" localhost | awk '!seen[$0]++')
# The master container runs as PORTAL_UID and must read its key.
chmod 0600 secrets/certs/master.key
echo "ready: run ./add-cluster.sh for each cluster, then docker compose up -d"
