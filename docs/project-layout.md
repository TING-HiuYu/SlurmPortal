# Project layout

This is a map of the repository: what each directory holds and where to start reading. The Go
module is `github.com/TING-HiuYu/SlurmPortal`, and every package has a package comment saying what
it owns.

```text
.
├── cmd/                  one directory per program (package main)
├── internal/             all Go libraries; nothing outside this module imports them
├── migrations/           SQL schema changes, embedded and applied at start
├── web/                  the React web app
├── deploy/               everything installed outside the Go binaries
├── docs/                 this documentation
└── .github/              CI, issue and pull request templates
```

## `cmd/`: programs

| Directory | Builds | Notes |
| --- | --- | --- |
| `worker` | `main.wasm` for the Cloudflare Worker | `GOOS=js GOARCH=wasm`; exports `goV2Fetch`, `goV2Tick` and friends to `deploy/worker/worker.js` |
| `server` | Native master (the container deployment) | Same `internal/app`, database/sql + SQLite; also serves the web app and a mutual-TLS sync listener |
| `slave` | Cluster agent | `-config`, `-once`, and `-plan` for an offline dry run |
| `gateway` | Web-access gateway | Terminal, SFTP, rsync on a login node |
| `slurm-lifecycle-hook` | Slurm prolog / epilog hook | Fail-open; reports to the slave's lifecycle runtime |
| `slurm-notify-observer` | Read-only slurmrestd proxy | Runs on the controller so the slave never holds the Slurm JWT key |
| `host-bridge` | Trigger bridge for the containerised slave | Runs `slurm_trigger.py` on the host over SSH |
| `migration-check` | Operator tool | Migrate a database copy, prove the CA keys open, export a snapshot |
| `convert-passwords` | Operator tool | Convert v1 reversible passwords into one-way hashes |

## `internal/`: libraries

**Request handling.** Start reading at `app`.

| Package | Role |
| --- | --- |
| `app` | Wires everything: migrations, plugins, middleware, core modules |
| `httpx` | Router (mandatory permissions), middleware, JSON responses |
| `authn` | Who is calling: session tokens (JWT), machine peers (client certificates) |
| `authz` | Is it allowed: route permissions, role policies (casbin), legacy policy import |
| `apperr` | The single error type returned to clients |
| `config` | YAML plus environment configuration and its validation |
| `audit` | Hash-chained audit log and its middleware |
| `telemetry` | Tracing and metrics interfaces, buffered provider, traced store |

**Business modules.**

| Package | Role |
| --- | --- |
| `domain/identity` | Users, groups, roles, UID/GID allocation, anti-escalation checks |
| `domain/auth` | Sign-in, tokens, password change and e-mailed reset codes |
| `domain/pki` | Certificate authorities and user certificates |
| `mail` | Mail hub: provider kinds (SMTP, REST), templates, delivery counts |
| `password` | argon2id and crypt hashing |
| `secretbox` | Sealing secrets at rest with keys derived from `PKI_MASTER_KEY` |
| `legacyimport` | One-time import of go-ldap-admin v1 data |

**Storage.**

| Package | Role |
| --- | --- |
| `store` | `Store` interface, database/sql implementation, migration runner |
| `store/d1` | Cloudflare D1 implementation over the JavaScript bridge |
| `store/storetest` | Conformance tests for both implementations |
| `objstore` | Minimal S3 (SigV4) client for R2 |
| `statusreport` | Latest report per cluster and kind |
| `testutil` | Migrated in-memory databases for tests |

**Plugins.**

| Package | Role |
| --- | --- |
| `plugin` | Registry, `Host`, roles, the shared `Services` registry |
| `plugins/all` | Links every built-in plugin behind a `no_<name>` build tag |
| `plugins/*` | One package per plugin; see [plugins.md](plugins.md) |

**Cluster side.**

| Package | Role |
| --- | --- |
| `replication` | The snapshot format, its hash, the DNS publisher and the snapshot endpoint |
| `slave` | Replicator, LDAP writer, trust publisher, trigger queue, reporters, `-plan` |
| `gateway` | Gateway server: tickets, SSH sessions, SFTP, transfers |
| `lifecycle/runtime` | Lifecycle runtime inside the slave; the hook's configuration |
| `lifecycle/eventbus` | Bounded in-process event bus |
| `lifecycle/jobnotify` | Job event types, slurmrestd reads, the observer |
| `lifecycle/jobdoneemail` | Listener that sends finished jobs to master |
| `lifecycle/resources` | GPU sampling and per-job usage summaries |

## `migrations/`

The files `0000_legacy_base.sql` … `0016_*.sql` hold the schema. `0000` is the go-ldap-admin v1
schema, so a v1 database opens as is. Files are applied once each, in name order, and are
additive only.

## `web/`

The React 19 app with Ant Design 6, built by Vite.

| Path | Holds |
| --- | --- |
| `src/routes.jsx` | Every page, its menu entry and the plugin or permission it needs |
| `src/session.js` | Sign-in state, the access map from `/api/v2/me/access` |
| `src/api/client.js` | API calls, token refresh |
| `src/views/admin/` | Admin pages (users, groups, roles, PKI, mail, notifications, diagnostics, audit…) |
| `src/views/status/` | Cluster status, resources and queue, usage |
| `src/views/workspace/` | Web terminal and file browser |
| `src/components/` | Shared components: layout, charts, template editor, terminal |
| `src/i18n.js`, `src/locales/en.json` | Chinese is the source language; English translations by key |
| `public/branding/` | Logo shown on the sign-in page and in the sidebar |

## `deploy/`

| Path | Holds |
| --- | --- |
| `compose/master/` | Container master: compose file (Caddy, master, RustFS), `init.sh`, `add-cluster.sh` |
| `compose/slave/` | Slave for any master: compose file, `init.sh`, `slave.yml.example`, demo OpenLDAP |
| `docker/` | `master.Dockerfile`, `slave.Dockerfile` |
| `worker/` | SaaS master: Worker shell (`worker.js`, D1 and SMTP bridges), `build.sh`, `wrangler.example.toml`, smoke test |
| `certs/` | `issue.sh`: private CA and certificates for the container master's sync port |
| `slave/` | systemd unit for a slave without containers |
| `triggers/` | `slurm_trigger.py`, host bridge dispatcher, trigger examples |
| `storage/` | HOME ACL reconciler, trust mounts, compute-node SSH trust installer |
| `gateway/` | Gateway config example, systemd units, nginx example |
| `gateway/acme/` | Public HTTPS certificate renewal (Certbot, Cloudflare DNS-01) |
| `gateway/container/` | Container image for a gateway with nginx |
| `lifecycle/` | Slurm prolog / epilog scripts, observer unit |

See [deployment.md](deployment.md) for how these fit together.

## Names you will still see

Some installed paths keep the project's former `go-ldap-admin` prefix, such as
`/etc/go-ldap-admin/` and `/usr/local/libexec/go-ldap-admin/`, so clusters installed before the
rename keep working. The project itself is Slurm Portal; new installs use `slurm-portal` names
wherever nothing depends on the old ones. The central service is the **master** (once called
"main"; configurations that still say `main` keep working).
