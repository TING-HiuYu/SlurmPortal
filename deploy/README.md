# deploy/

Everything installed somewhere other than as a Go binary. [docs/deployment.md](../docs/deployment.md)
explains how the parts fit together and links to the three guides.

| Directory | Used by | Contents |
| --- | --- | --- |
| [compose/master](compose/master) | Container master | Compose file (Caddy, master, RustFS), `init.sh`, `add-cluster.sh`, `Caddyfile` |
| [compose/slave](compose/slave) | Every cluster | Compose file for the slave, `init.sh`, `slave.yml.example`, a demo OpenLDAP |
| [docker](docker) | Both compose files | `master.Dockerfile` (server + web app) and `slave.Dockerfile` (slave + host bridge) |
| [worker](worker) | SaaS master | Worker shell and bridges, `build.sh`, `wrangler.example.toml`, smoke test, GitHub connector script |
| [certs](certs) | Container master | `issue.sh`: the private CA, the master's sync certificate, client certificates |
| [slave](slave) | Slaves without containers | systemd unit |
| [triggers](triggers/README.md) | Slaves | `slurm_trigger.py` (Slurm accounts and storage on user changes), host bridge dispatcher, examples |
| [storage](storage/README.md) | Login and compute nodes | HOME ACL reconciler, trust-file mounts, compute-node SSH trust |
| [gateway](gateway/README.md) | Login nodes | Gateway config, systemd units, nginx, [ACME renewal](gateway/acme/README.md), [container](gateway/container/README.md) |
| [lifecycle](lifecycle/README.md) | Slurm controller | prolog / epilog hooks and the observer |

Installed host paths keep the project's former `go-ldap-admin` prefix
(`/etc/go-ldap-admin/`, `/usr/local/libexec/go-ldap-admin/`) so existing clusters did not change.

Every script with logic has a `test_*.py` next to it; CI runs them all.
