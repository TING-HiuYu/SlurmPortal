#!/bin/sh
# Private CA for a container (non-Cloudflare) master's sync listener: the master's server certificate
# and one client certificate per slave or gateway. The fingerprint printed for each client goes into
# the master's CLUSTERS (slaves) or web-access gateways (gateways) setting.
set -eu
umask 077

usage() {
  cat >&2 <<EOF
Usage: $0 init-ca DIR
       $0 master DIR NAME [NAME...]     server certificate; each NAME is a DNS name or an IP address
       $0 client DIR ID                  client certificate for a slave or gateway (also: slave DIR ID)
EOF
  exit 2
}

DAYS=${DAYS:-825}
[ "$#" -ge 2 ] || usage
action=$1
directory=$2
mkdir -p "$directory"
directory=$(cd "$directory" && pwd)

need_ca() {
  [ -f "$directory/ca.key" ] && [ -f "$directory/ca.crt" ] || { echo "run init-ca first" >&2; exit 1; }
}

fingerprint() {
  openssl x509 -in "$1" -outform DER | openssl dgst -sha256 -r | cut -d' ' -f1
}

case "$action" in
  init-ca)
    [ "$#" -eq 2 ] || usage
    [ ! -e "$directory/ca.key" ] && [ ! -e "$directory/ca.crt" ] || { echo "CA already exists; refusing to replace it" >&2; exit 1; }
    temp=$(mktemp)
    trap 'rm -f "$temp"' EXIT HUP INT TERM
    cat > "$temp" <<'EOF'
[req]
prompt = no
distinguished_name = dn
x509_extensions = ca_ext
[dn]
CN = Slurm Portal sync CA
[ca_ext]
basicConstraints = critical,CA:TRUE,pathlen:0
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
EOF
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$directory/ca.key"
    openssl req -new -x509 -sha256 -days 3650 -key "$directory/ca.key" -config "$temp" -out "$directory/ca.crt"
    echo "CA created in $directory; keep ca.key offline once every certificate is issued"
    ;;
  master|main)
    [ "$#" -ge 3 ] || usage
    need_ca
    shift 2
    [ ! -e "$directory/master.key" ] && [ ! -e "$directory/master.crt" ] || { echo "master certificate already exists" >&2; exit 1; }
    san=""
    for name in "$@"; do
      case "$name" in
        *[!A-Za-z0-9.:-]*|''|.*|*.) echo "invalid name: $name" >&2; exit 2 ;;
        *[!0-9.]*) case "$name" in *:*) entry="IP:$name" ;; *) entry="DNS:$name" ;; esac ;;
        *) entry="IP:$name" ;;
      esac
      san="${san:+$san,}$entry"
    done
    temp=$(mktemp)
    trap 'rm -f "$temp" "$directory/master.csr"' EXIT HUP INT TERM
    cat > "$temp" <<EOF
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = serverAuth
subjectAltName = $san
EOF
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$directory/master.key"
    openssl req -new -sha256 -key "$directory/master.key" -subj "/CN=$1" -out "$directory/master.csr"
    openssl x509 -req -sha256 -days "$DAYS" -in "$directory/master.csr" -CA "$directory/ca.crt" -CAkey "$directory/ca.key" -CAserial "$directory/ca.srl" -CAcreateserial -extfile "$temp" -out "$directory/master.crt"
    openssl verify -purpose sslserver -CAfile "$directory/ca.crt" "$directory/master.crt"
    chmod 0644 "$directory/master.crt" "$directory/ca.crt"
    echo "master certificate created: master.crt, master.key ($san)"
    ;;
  client|slave)
    [ "$#" -eq 3 ] || usage
    need_ca
    name=$3
    case "$name" in *[!A-Za-z0-9_-]*|'') echo "invalid ID" >&2; exit 2 ;; esac
    mkdir -p "$directory/clients"
    [ ! -e "$directory/clients/$name.key" ] && [ ! -e "$directory/clients/$name.crt" ] || { echo "certificate for $name already exists" >&2; exit 1; }
    temp=$(mktemp)
    trap 'rm -f "$temp" "$directory/clients/$name.csr"' EXIT HUP INT TERM
    cat > "$temp" <<EOF
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
subjectAltName = URI:spiffe://slurm-portal/client/$name
EOF
    openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$directory/clients/$name.key"
    openssl req -new -sha256 -key "$directory/clients/$name.key" -subj "/CN=$name" -out "$directory/clients/$name.csr"
    openssl x509 -req -sha256 -days "$DAYS" -in "$directory/clients/$name.csr" -CA "$directory/ca.crt" -CAkey "$directory/ca.key" -CAserial "$directory/ca.srl" -CAcreateserial -extfile "$temp" -out "$directory/clients/$name.crt"
    openssl verify -purpose sslclient -CAfile "$directory/ca.crt" "$directory/clients/$name.crt"
    echo "client certificate created: clients/$name.crt, clients/$name.key"
    echo "fingerprint: $(fingerprint "$directory/clients/$name.crt")"
    ;;
  *) usage ;;
esac
