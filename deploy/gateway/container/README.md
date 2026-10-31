# Gateway container

This image runs the gateway and nginx together, under supervisord, in one container. Use it when
the login node has no systemd (for example when the login node is itself a container).

| File | Purpose |
| --- | --- |
| `Dockerfile` | Ubuntu with nginx, supervisor, the SSH client, screen and rsync; the gateway binary is copied in |
| `prepare.py` | Builds a minimal build context (binary and scripts only, never keys) and renders the non-root nginx config |
| `supervisord.conf`, `entrypoint.sh`, `exit-on-fatal.py` | Run nginx and the gateway; stop the container if either keeps failing |
| `healthcheck.sh` | Container health check |
| `compose.yaml.example` | How it runs |

## How it runs

The `compose.yaml.example` service is locked down:

- host network;
- a fixed non-root UID (19033 by default);
- read-only root filesystem, no capabilities, `no-new-privileges`;
- no Docker socket.

It mounts these, read-only:

| Host path | Mounted at | Holds |
| --- | --- | --- |
| Config directory | `/etc/go-ldap-admin` | `web-access-gateway.yaml` and `web-access-gateway/nginx.conf` |
| TLS directory | `/etc/go-ldap-admin-web-access-tls` | This site's HTTPS certificate and its own mTLS client certificate; never another site's keys |
| Trust directory | `/etc/go-ldap-admin-trust` | The slave's published trust directory, so CA changes are seen at once |

The gateway keeps its state in its own directory on local disk, owned by the gateway UID. It never
mounts user HOMEs: file access goes through SSH as the user.

Keep each release in its own runtime directory; to roll back, run `docker compose up -d` in the
previous one.

Run the tests with `python3 -m unittest test_prepare` in this directory.
