#!/bin/sh
# One-time setup for a cluster's slave: .env (the containers run as you), the data folders, a
# passwd entry for that user (ssh-keygen, which builds the revocation list, needs one), and
# slave.yml from the example. Safe to rerun: existing files are left alone.
#   ./init.sh
set -eu
cd "$(dirname "$0")"
umask 077
[ -f .env ] || sed -e "s#^PORTAL_UID=.*#PORTAL_UID=$(id -u)#" -e "s#^PORTAL_GID=.*#PORTAL_GID=$(id -g)#" .env.example > .env
mkdir -p secrets data/state data/demo-ldap
trust=$(sed -n 's/^TRUST_DIR=//p' .env)
mkdir -p "${trust:-./data/trust}"
uid=$(sed -n 's/^PORTAL_UID=//p' .env)
gid=$(sed -n 's/^PORTAL_GID=//p' .env)
printf 'root:x:0:0:root:/root:/sbin/nologin\nportal:x:%s:%s:Slurm Portal slave:/var/lib/slurm-portal:/sbin/nologin\n' "$uid" "$gid" > data/passwd
chmod 0644 data/passwd
if [ ! -f slave.yml ]; then
  cp slave.yml.example slave.yml
  chmod 0600 slave.yml
  echo "created slave.yml: fill in the master, cluster-id and LDAP settings"
fi
[ -f secrets/ldap-password ] || echo "put the LDAP bind password in secrets/ldap-password (mode 0600)"
for f in client.crt client.key; do [ -f "secrets/$f" ] || echo "put this cluster's $f in secrets/ (from your master)"; done
echo "then: docker compose up -d"
