#!/bin/sh
set -eu
umask 077
if [ "$(id -u)" = 0 ]; then
    echo 'gateway container must run as its dedicated non-root UID' >&2
    exit 1
fi
case "${GATEWAY_TLS_HOST:-}" in
    ''|*[!A-Za-z0-9.-]*|.*|-*) echo 'GATEWAY_TLS_HOST must be an exact DNS hostname' >&2; exit 1 ;;
esac
state=/var/lib/go-ldap-admin-web-access-gateway
if [ ! -d "$state" ] || [ ! -w "$state" ] || [ "$(stat -c %a "$state")" != 700 ] || [ "$(stat -c %u "$state")" != "$(id -u)" ]; then
    echo 'gateway-only state directory must exist with service UID ownership and mode0700' >&2
    exit 1
fi
mkdir -p /run/web-access-gateway/nginx/client-body /run/web-access-gateway/nginx/proxy \
    /run/web-access-gateway/nginx/fastcgi /run/web-access-gateway/nginx/uwsgi /run/web-access-gateway/nginx/scgi
/usr/local/libexec/go-ldap-admin/web-access-gateway -config /etc/go-ldap-admin/web-access-gateway.yaml -check-config
/usr/sbin/nginx -t -c /etc/go-ldap-admin/web-access-gateway/nginx.conf
exec "$@"
