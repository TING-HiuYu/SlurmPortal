#!/bin/sh
# Registers one cluster: issues its slave's client certificate, adds the cluster to CLUSTERS in .env,
# and writes a bundle to copy to the cluster host (deploy/compose/slave on that host).
#   ./add-cluster.sh cluster1 "Cluster 1"
# Restart the master afterwards: docker compose up -d
set -eu
cd "$(dirname "$0")"
[ "$#" -eq 2 ] || { echo "usage: $0 CLUSTER_ID \"Display name\"" >&2; exit 2; }
id=$1
name=$2
case "$id" in *[!a-z0-9-]*|''|-*) echo "cluster ID: lowercase letters, digits and -" >&2; exit 2 ;; esac
case "$name" in *[\"\\]*) echo "the display name may not contain quotes or backslashes" >&2; exit 2 ;; esac
[ -f .env ] && [ -f secrets/certs/ca.key ] || { echo "run ./init.sh first" >&2; exit 1; }
grep -q "^CLUSTERS=.*\"id\":\"$id\"" .env && { echo "cluster $id is already in .env" >&2; exit 1; }

umask 077
sh ../../certs/issue.sh client secrets/certs "$id" >/dev/null 2>&1
fp=$(openssl x509 -in "secrets/certs/clients/$id.crt" -outform DER | openssl dgst -sha256 -r | cut -d' ' -f1)
entry="{\"id\":\"$id\",\"name\":\"$name\",\"fingerprint\":\"$fp\"}"
if grep -q '^CLUSTERS=\[\]$' .env; then
  sed -i "s#^CLUSTERS=\[\]\$#CLUSTERS=[$entry]#" .env
else
  sed -i "s#^\(CLUSTERS=\[.*\)\]\$#\1,$entry]#" .env
fi

if grep -q '^SYNC_CLIENT_FINGERPRINTS=$' .env; then
  sed -i "s#^SYNC_CLIENT_FINGERPRINTS=\$#SYNC_CLIENT_FINGERPRINTS=$fp#" .env
else
  sed -i "s#^\(SYNC_CLIENT_FINGERPRINTS=.*\)\$#\1,$fp#" .env
fi

bundle=secrets/clusters/$id
mkdir -p "$bundle"
cp "secrets/certs/clients/$id.crt" "$bundle/client.crt"
cp "secrets/certs/clients/$id.key" "$bundle/client.key"
cp secrets/certs/ca.crt "$bundle/master-ca.crt"
host=$(sed -n 's/^PORTAL_HOST=//p' .env)
port=$(sed -n 's/^SYNC_PORT=//p' .env)
base=$(sed -n 's/^LDAP_BASE_DN=//p' .env)
cat > "$bundle/README.txt" <<TXT
Bundle for cluster $id. Copy this directory to the cluster host, into deploy/compose/slave/secrets/,
then follow docs/deploy-slaves.md. In slave.yml set:
  master-url: https://$host:${port:-8443}
  dns-name: ""                  # this master publishes its hash over HTTPS, not DNS
  cluster-id: $id
  tls.server-name: $host
  tls.ca-file: /etc/slurm-portal/secrets/master-ca.crt
  master's base DN: $base
TXT
echo "cluster $id added (fingerprint $fp)"
echo "bundle: $(pwd)/$bundle  -> copy it to the cluster host; then: docker compose up -d"
