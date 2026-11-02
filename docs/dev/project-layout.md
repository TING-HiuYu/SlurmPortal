# Project layout

This page maps the directory structure of the `github.com/TING-HiuYu/SlurmPortal` repository and explains where each subsystem lives. Every Go package includes a package-level doc comment summarizing its responsibility and invariants.

```text
.
├── cmd/                  Executable entrypoints (one subdirectory per binary, package main)
├── internal/             Private Go packages organized by architectural layer
├── migrations/           Embedded SQL schema migrations applied in order at startup
├── web/                  React 19 + Ant Design 6 single-page application
├── deploy/               Docker Compose stacks, Worker host scripts, systemd units, and cluster scripts
├── docs/                 Markdown source for the Developer and Deployment Wikis
├── site/                 Docusaurus documentation site and Simplified Chinese localization
└── .github/              GitHub Actions CI/Pages workflows and issue/PR templates
```

## `cmd/`: programs

| Directory | Binary / Artifact | Description |
| --- | --- | --- |
| `worker` | `main.wasm` (Cloudflare Worker) | Compiled with `GOOS=js GOARCH=wasm`; exports `goV2Fetch`, `goV2Tick`, and `goV2Publish` to `deploy/worker/worker.js` |
| `server` | Native master server | Runs `internal/app` over `database/sql` + SQLite; serves static web assets and the dedicated mTLS `/sync/*` listener |
| `slave` | Cluster synchronization agent | Supports continuous daemon mode (`-config`), single-pass mode (`-once`), and offline rehearsal (`-plan`) |
| `gateway` | Web Access Gateway | Serves browser terminal WebSockets, SFTP file browsing, and `rsync` transfers on a login node |
| `slurm-lifecycle-hook` | Slurm prolog / epilog notifier | Fail-open helper invoked by `slurmctld` to report job start and end events to the slave |
| `slurm-notify-observer` | Read-only `slurmrestd` proxy | Runs as `slurm` on the controller so the slave can query job details without holding the Slurm JWT key |
| `host-bridge` | Container-to-host trigger bridge | Pipes trigger event JSON over SSH stdin to `slurm_trigger.py` on the cluster host |
| `migration-check` | Migration rehearsal CLI | Upgrades a copy of a v1 database, verifies `PKI_MASTER_KEY` opens every CA, and exports a test snapshot |
| `convert-passwords` | Legacy password converter | Converts reversible v1 RSA passwords into one-way Argon2id and SHA-512 crypt hashes |

## `internal/`: libraries

**HTTP & Request Pipeline** (start reading at `internal/app`):

| Package | Responsibility |
| --- | --- |
| `app` | Application bootstrap: runs migrations, initializes plugins, wires middleware and core domain modules |
| `httpx` | HTTP router with mandatory permissions, middleware stack, strict JSON decoder, and response helpers |
| `authn` | Authentication: HS256 session tokens (`JWT`) and mTLS machine peer verifiers (`TLSPeer`, `BridgePeer`) |
| `authz` | Authorization: route permission types, Casbin RBAC evaluator, and legacy v1 policy importer |
| `apperr` | Unified client error type mapping internal errors to stable machine codes and HTTP statuses |
| `config` | YAML and environment variable loader with strict validation |
| `audit` | Append-only SHA-256 hash-chained audit log and HTTP middleware |
| `telemetry` | OpenTelemetry tracing/metrics interfaces, buffered span exporter, and traced store wrapper |

**Core Domain Modules:**

| Package | Responsibility |
| --- | --- |
| `domain/identity` | Users, POSIX and organizational groups, roles, monotonic UID/GID allocation, and rank checks |
| `domain/auth` | Password sign-in, session refresh/logout, password updates, and 6-digit email reset codes |
| `domain/pki` | SSH and X.509 Certificate Authorities, user certificate issuance/rotation/revocation, and CRL signing |
| `mail` | Mail hub: named providers (`smtp`, `rest`), HTML template engine, and daily delivery counters |
| `password` | Argon2id and `{CRYPT}$6$` SHA-512 password hashing and constant-time verification |
| `secretbox` | AES-256-GCM + HKDF-SHA256 encryption for database-stored secrets |
| `legacyimport` | One-time idempotent importer for `go-ldap-admin` v1 configuration and GPU history |

**Storage Layer:**

| Package | Responsibility |
| --- | --- |
| `store` | `Store` interface, `database/sql` SQLite implementation, and embedded migration runner |
| `store/d1` | Cloudflare D1 `Store` implementation communicating over the WebAssembly JS bridge |
| `store/storetest` | Shared conformance test suite executed against both `store.SQL` and `d1.Store` |
| `objstore` | Minimal AWS SigV4 S3 client for Cloudflare R2 and RustFS |
| `statusreport` | Helper for storing and querying the latest per-cluster status reports |
| `testutil` | In-memory migrated SQLite databases and test fixtures |

**Plugin System:**

| Package | Responsibility |
| --- | --- |
| `plugin` | Plugin registry, `Host` dependency container, process roles, and cross-plugin `Services` registry |
| `plugins/all` | Blank-imports every built-in plugin behind opt-out `no_<name>` build tags |
| `plugins/*` | Individual feature plugins (see [Plugins](plugins.md)) |

**Cluster Replication & Agents:**

| Package | Responsibility |
| --- | --- |
| `replication` | Snapshot builder, double-read consistency check, DNS TXT publisher, and `/sync/v2/snapshot*` handlers |
| `slave` | Replication state machine, OpenLDAP diff reconciler, atomic trust publisher, trigger queue, and `-plan` |
| `gateway` | Web Access Gateway server: ticket/cert verification, SSH session pool, SFTP handlers, and `rsync` runner |
| `lifecycle/runtime` | In-process job lifecycle listener hosted by the slave |
| `lifecycle/eventbus` | Bounded in-memory pub/sub bus for job lifecycle events |
| `lifecycle/jobnotify` | Job event schemas, `slurmrestd` client, and the `slurm-notify-observer` proxy |
| `lifecycle/jobdoneemail` | Event subscriber that resolves finished jobs and forwards completion reports to the master |
| `lifecycle/resources` | Event subscriber that samples NVML GPU usage and submits per-job resource summaries |

## `migrations/`

Numbered SQL files `0000_legacy_base.sql` through `0016_*.sql` define the relational schema. `0000_legacy_base.sql` matches the `go-ldap-admin` v1 schema so v1 databases upgrade seamlessly. Migrations are applied once in order and are strictly additive.

## `web/`

React 19 single-page application using Ant Design 6 and Vite.

| Path | Contents |
| --- | --- |
| `src/routes.jsx` | Declarative route definitions with plugin and permission guards |
| `src/session.js` | Session state and permission helpers populated from `GET /api/v2/me/access` |
| `src/api/client.js` | Fetch wrapper with automatic Bearer token refresh and error translation |
| `src/views/admin/` | Administrative views (users, groups, roles, PKI, mail, notifications, diagnostics, audit) |
| `src/views/status/` | Live cluster node status, resource capacity, job queue, and 7-day GPU usage |
| `src/views/workspace/` | Self-service profile, SSH certificate download, browser terminal, and SFTP file browser |
| `src/components/` | Shared layout, charts, email template editor, and `xterm.js` terminal component |
| `src/i18n.js`, `src/locales/en.json` | Localization runtime and English translation dictionary |
| `public/branding/` | Logos displayed on the sign-in screen and sidebar |

## `deploy/`

| Path | Contents |
| --- | --- |
| `compose/master/` | Self-hosted container master stack (Caddy, `cmd/server`, RustFS), `init.sh`, and `add-cluster.sh` |
| `compose/slave/` | Containerized slave stack, `init.sh`, `slave.yml.example`, and optional demo OpenLDAP profile |
| `docker/` | Multi-stage `master.Dockerfile` and `slave.Dockerfile` |
| `worker/` | Cloudflare Worker shell (`worker.js`, D1/SMTP bridges, `edge.js`), `build.sh`, `wrangler.example.toml`, and smoke tests |
| `certs/` | `issue.sh` helper for generating a private ECDSA P-256 CA and mTLS certificates |
| `slave/` | Native systemd unit (`slurm-portal-slave.service`) |
| `triggers/` | `slurm_trigger.py`, container host-bridge dispatcher, and sample trigger configurations |
| `storage/` | NFSv4 home ACL reconciler, trust mount units, and compute-node SSH trust helper |
| `gateway/` | `gateway.yaml.example`, systemd service units, and `nginx.conf.example` |
| `gateway/acme/` | Certbot + Cloudflare DNS-01 automated certificate renewal scripts |
| `gateway/container/` | Combined container image running `web-access-gateway` behind Nginx |
| `lifecycle/` | Slurm `prolog-slurmctld` / `epilog-slurmctld` wrapper scripts and `slurm-notify-observer.service` |

See the [Deployment Wiki](../deploy/README.md) for step-by-step installation instructions.

## Names you will still see

A few filesystem paths and internal domain-separation constants retain the historical `go-ldap-admin` prefix—such as `/etc/go-ldap-admin/`, `/usr/local/libexec/go-ldap-admin/`, and the `go-ldap-admin/` HKDF info string—so clusters and databases created prior to the v2 rename continue operating without breaking changes. New installations use `slurm-portal` naming wherever backward compatibility is not affected. Similarly, configuration keys that previously used `main` (such as `main-url`) remain supported aliases for `master` (`master-url`).
