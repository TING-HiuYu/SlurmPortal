ARG BASE_IMAGE=ubuntu:24.04
FROM ${BASE_IMAGE}

ARG GATEWAY_UID=19033
ARG GATEWAY_GID=19033
RUN apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
       ca-certificates nginx supervisor openssh-client screen rsync curl gawk \
    && rm -rf /var/lib/apt/lists/* /etc/nginx/sites-enabled/default \
    && groupadd --gid "${GATEWAY_GID}" web-access-gateway \
    && useradd --system --no-create-home --no-log-init --home-dir /nonexistent \
       --uid "${GATEWAY_UID}" --gid "${GATEWAY_GID}" --shell /usr/sbin/nologin web-access-gateway \
    && install -d -m 0755 /etc/go-ldap-admin/web-access-gateway /etc/go-ldap-admin-web-access-tls \
    && install -d -m 0700 -o web-access-gateway -g web-access-gateway \
       /run/web-access-gateway /var/lib/go-ldap-admin-web-access-gateway

# Use prepare.py context: never send the repository, TLS or mTLS keys as context.
COPY --chmod=0755 web-access-gateway /usr/local/libexec/go-ldap-admin/web-access-gateway
COPY --chmod=0755 entrypoint.sh /usr/local/bin/gateway-entrypoint
COPY --chmod=0755 healthcheck.sh /usr/local/bin/gateway-healthcheck
COPY --chmod=0755 exit-on-fatal.py /usr/local/bin/gateway-exit-on-fatal
COPY --chmod=0644 supervisord.conf /etc/supervisor/web-access-gateway.conf

ENV HOME=/nonexistent PYTHONDONTWRITEBYTECODE=1
WORKDIR /run/web-access-gateway
USER ${GATEWAY_UID}:${GATEWAY_GID}
HEALTHCHECK --interval=30s --timeout=12s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/gateway-healthcheck"]
ENTRYPOINT ["/usr/local/bin/gateway-entrypoint"]
CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/web-access-gateway.conf"]
