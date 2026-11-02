# Web-access gateway

One gateway instance runs on each cluster's login node. It powers the browser terminal, SFTP file browser, and cross-cluster `rsync` transfers, executing every session as the signed-in POSIX user over local SSH. The Go implementation lives in [`cmd/gateway`](../../cmd/gateway) and [`internal/gateway`](../../internal/gateway).

| File | Purpose |
| --- | --- |
| `gateway.yaml.example` | Annotated configuration template |
| `web-access-gateway.service` | systemd unit; validates configuration with `-check-config` before starting |
| `nginx.conf.example`, `web-access-nginx.service` | Public HTTPS reverse proxy in front of the gateway's loopback listener |
| [acme/](acme/README.md) | Automated public HTTPS certificate renewal via Certbot and Cloudflare DNS-01 |
| [container/](container/README.md) | Single-container deployment bundling the gateway and Nginx under `supervisord` |

## Install (systemd)

1. Install the gateway binary at `/usr/local/libexec/go-ldap-admin/web-access-gateway` and create the `web-access-gateway` system user.
2. Copy `gateway.yaml.example` to `/etc/go-ldap-admin/web-access-gateway.yaml` (mode `0600`, readable by `web-access-gateway`) and fill in your site settings.
3. Configure Nginx from `nginx.conf.example`. Make sure the CORS origin map includes your portal's exact origin, or browser requests will fail with `"Failed to fetch"`.
4. Enable and start `web-access-gateway.service` and `web-access-nginx.service`.
5. Add the gateway's client-certificate SHA-256 fingerprint to `web-access.gateways` in the master's `PLUGINS_CONFIG` and redeploy the master. Web access features for the cluster activate automatically once the gateway registers.

To reload configuration changes without dropping active terminal or transfer sessions, run `systemctl reload web-access-gateway` (sends `SIGHUP`).
