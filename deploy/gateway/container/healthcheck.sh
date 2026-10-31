#!/bin/sh
set -eu
case "${GATEWAY_TLS_HOST:-}" in
    ''|*[!A-Za-z0-9.-]*|.*|-*) exit 1 ;;
esac
public_port="${GATEWAY_PUBLIC_PORT-19933}"
case "$public_port" in
    ''|*[!0-9]*|0*) exit 1 ;;
esac
# Check length before numeric comparison to reject oversized values safely.
[ "${#public_port}" -ge 4 ] && [ "${#public_port}" -le 5 ] || exit 1
[ "$public_port" -ge 1024 ] && [ "$public_port" -le 65535 ] || exit 1
case "$public_port" in
    19934|19935) exit 1 ;;
esac
# Verify the real HTTPS certificate/SNI, without bypassing TLS or minting JWTs.
curl --noproxy '*' --fail --silent --show-error --max-time 5 \
    --resolve "${GATEWAY_TLS_HOST}:19934:127.0.0.1" \
    "https://${GATEWAY_TLS_HOST}:19934/readyz" >/dev/null
code=$(curl --noproxy '*' --silent --show-error --max-time 5 --output /dev/null --write-out '%{http_code}' \
    --resolve "${GATEWAY_TLS_HOST}:${public_port}:127.0.0.1" \
    "https://${GATEWAY_TLS_HOST}:${public_port}/v1/auth/session")
[ "$code" = 403 ]
