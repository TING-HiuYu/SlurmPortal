# deploy/

This directory contains all deployment configurations, container definitions, systemd units, and host helper scripts that live outside the Go binaries. See [docs/deploy](../docs/deploy/README.md) for an overview of the architecture and step-by-step installation guides.

| Directory | Used by | Contents |
| --- | --- | --- |
| [compose/master](compose/master) | Container master | Docker Compose stack (Caddy, master, RustFS), `init.sh`, `add-cluster.sh`, and `Caddyfile` |
| [compose/slave](compose/slave) | Every cluster | Docker Compose stack for the slave agent, `init.sh`, `slave.yml.example`, and a demo OpenLDAP profile |
| [docker](docker) | Both Compose stacks | `master.Dockerfile` (server + web bundle) and `slave.Dockerfile` (slave + host bridge) |
| [worker](worker) | SaaS master | Cloudflare Worker runtime bridges, `build.sh`, `wrangler.example.toml`, smoke test, and GitHub OAuth connector script |
| [certs](certs) | Container master | `issue.sh` for managing the private mTLS CA, master sync certificate, and cluster client certificates |
| [slave](slave) | Non-container slaves | `slurm-portal-slave.service` systemd unit |
| [triggers](triggers/README.md) | Slaves | `slurm_trigger.py` (manages Slurm associations and storage hooks on user events), SSH host bridge dispatcher, and config examples |
| [storage](storage/README.md) | Login and compute nodes | Home directory ACL reconciler, NFS trust-directory mount helper, and compute-node SSH lockdown script |
| [gateway](gateway/README.md) | Login nodes | Web access gateway config template, systemd units, Nginx config, [ACME DNS-01 renewal](gateway/acme/README.md), and [single-container packaging](gateway/container/README.md) |
| [lifecycle](lifecycle/README.md) | Slurm controller | `PrologSlurmctld` / `EpilogSlurmctld` hook scripts and `slurm-notify-observer` systemd unit |

Host configuration and binary paths retain the historical `go-ldap-admin` prefix (`/etc/go-ldap-admin/` and `/usr/local/libexec/go-ldap-admin/`) to preserve compatibility with existing cluster installations.

Every Python helper script with non-trivial logic has a companion `test_*.py` unit test suite that runs in CI.
