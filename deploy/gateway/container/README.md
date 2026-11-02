# Gateway container

This directory packages the web-access gateway and Nginx into a single container managed by `supervisord`. Use it when the cluster login node cannot run systemd units directly (for example, when the login node itself is a container).

| File | Purpose |
| --- | --- |
| `Dockerfile` | Ubuntu image containing Nginx, `supervisord`, OpenSSH client, `screen`, `rsync`, and the compiled gateway binary |
| `prepare.py` | Stages a minimal Docker build context (binaries and scripts only, excluding private keys) and renders an unprivileged Nginx config |
| `supervisord.conf`, `entrypoint.sh`, `exit-on-fatal.py` | Starts Nginx and the gateway, and terminates the container if either process repeatedly crashes |
| `healthcheck.sh` | Container healthcheck script |
| `compose.yaml.example` | Hardened Docker Compose service definition |

## How it runs

The service in `compose.yaml.example` runs with strict container hardening:

- shares the host network namespace (`network_mode: host`);
- runs as a fixed non-root UID (`19033` by default);
- uses a read-only root filesystem with all Linux capabilities dropped and `no-new-privileges` enabled;
- never mounts the host Docker socket.

It bind-mounts three host directories read-only:

| Host path | Container mount | Contents |
| --- | --- | --- |
| Config directory | `/etc/go-ldap-admin` | `web-access-gateway.yaml` and `web-access-gateway/nginx.conf` |
| TLS directory | `/etc/go-ldap-admin-web-access-tls` | The login node's public HTTPS certificate and its mTLS client key pair (never share keys from other sites) |
| Trust directory | `/etc/go-ldap-admin-trust` | The slave agent's published SSH trust directory so CA and KRL updates take effect immediately |

Gateway runtime state is stored in a dedicated local directory owned by the gateway UID. User home directories are never mounted into the container; all terminal, file, and transfer operations connect over local SSH as the authenticated POSIX user.

Store each deployed release in its own versioned runtime directory on the host so you can roll back at any time by running `docker compose up -d` from the previous release directory.

Run the preparation script unit tests from this directory:

```sh
python3 -m unittest test_prepare
```
