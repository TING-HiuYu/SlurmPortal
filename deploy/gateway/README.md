# Web-access gateway

One gateway runs on each cluster's login node. It serves the browser terminal, the file browser
and rsync transfers between clusters, all as the signed-in user over SSH. The source is
[`cmd/gateway`](../../cmd/gateway) and [`internal/gateway`](../../internal/gateway).

| File | Purpose |
| --- | --- |
| `gateway.yaml.example` | Annotated config template |
| `web-access-gateway.service` | systemd unit; checks the config before starting (`-check-config`) |
| `nginx.conf.example`, `web-access-nginx.service` | Public HTTPS in front of the gateway's loopback listener |
| [acme/](acme/README.md) | Renews the public HTTPS certificate (Certbot, Cloudflare DNS-01) |
| [container/](container/README.md) | The same gateway and nginx as one container |

## Install (systemd)

1. Install the binary as `/usr/local/libexec/go-ldap-admin/web-access-gateway`, and create a
   `web-access-gateway` system user.
2. Write `/etc/go-ldap-admin/web-access-gateway.yaml` (mode 0600, readable by that user) from
   `gateway.yaml.example`.
3. Install nginx's site from `nginx.conf.example`. Put the portal origin in its CORS map,
   otherwise browsers report "Failed to fetch".
4. Enable `web-access-gateway.service` and `web-access-nginx.service`.
5. Add the gateway's client-certificate fingerprint to `web-access.gateways` in master's
   `PLUGINS_CONFIG`, then deploy master. The cluster's web features turn on once the gateway
   registers.

Reload the config without dropping sessions with `systemctl reload web-access-gateway` (SIGHUP).
