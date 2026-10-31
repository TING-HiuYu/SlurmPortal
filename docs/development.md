# Development

This page covers local setup, running each part, tests, and the conventions code in this
repository follows.

## Prerequisites

| Tool | Version | For |
| --- | --- | --- |
| Go | 1.26 (see `go.mod`) | Everything in `cmd/` and `internal/` |
| Node.js | 22 | The web app and the Worker bridge tests |
| Python | 3.12 with PyYAML | Cluster scripts in `deploy/` and their tests |
| OpenLDAP `slapd` | optional | `make ldap-it`, the slave's integration test |
| Wrangler | 4 (via `npx`) | Deploying to Cloudflare |

## Run the portal locally

```sh
mkdir -p data
JWT_KEYS="dev:$(openssl rand -base64 32)" JWT_ACTIVE_KID=dev \
PKI_MASTER_KEY="$(openssl rand -base64 32)" \
BOOTSTRAP_ADMIN_USERNAME=admin BOOTSTRAP_ADMIN_PASSWORD='change-me-now-123' \
SYSTEM_ROLE=standalone HTTP_LISTEN=127.0.0.1:8080 \
go run ./cmd/server
```

- Settings come from environment variables. `-config file.yml` adds a YAML file underneath them.
- The database is `data/slurm-portal.db` (SQLite). Change it with `DB_DSN`.
- The bootstrap admin is created once. Remove `BOOTSTRAP_ADMIN_PASSWORD` afterwards.
- Add `PLUGINS_ENABLED` to try plugins, for example `PLUGINS_ENABLED=diagnostics,job-notification`.

Then run the web app against it:

```sh
cd web
npm ci
API_ORIGIN=http://127.0.0.1:8080 npm run dev    # http://localhost:5173
```

`API_ORIGIN` defaults to `http://127.0.0.1:8080`. Point it at any running master to work on the UI
against real data.

To try the whole container deployment on one machine (master, S3 store, a slave and a demo
OpenLDAP), follow [deploy-container-master.md](deploy-container-master.md) with `localhost` as the
host name, then [deploy-slaves.md](deploy-slaves.md#try-it-without-a-cluster).

### Settings

`internal/config/config.go` lists every setting. These are the ones you are most likely to need:

| Variable | Meaning |
| --- | --- |
| `SYSTEM_ROLE` | `standalone`, `master`, `slave` or `gateway` |
| `HTTP_LISTEN`, `CORS_ORIGINS`, `TRUSTED_IP_HEADER` | Listener, allowed browser origins, proxy client-IP header |
| `STATIC_DIR` | `cmd/server` only: serve the built web app (`web/dist`) from this directory |
| `BEHIND_TLS_PROXY` | `cmd/server` only: trust `X-Forwarded-Proto: https` from a TLS proxy. Only when the listener is reachable through that proxy alone |
| `SYNC_LISTEN`, `SYNC_CERT_FILE`, `SYNC_KEY_FILE`, `SYNC_CLIENT_CA_FILE` | `cmd/server` only: the mutual-TLS listener for slaves and gateways |
| `DB_DSN` | Native database DSN |
| `JWT_KEYS`, `JWT_ACTIVE_KID` | Session token keys as `kid:base64,kid2:base64`, and the one that signs |
| `PKI_MASTER_KEY` | Base64 32 bytes; seals CA keys and stored secrets. Losing it loses the CAs |
| `AUTH_ISSUER`, `AUTH_AUDIENCE` | Session token claims |
| `LDAP_BASE_DN`, `LDAP_USER_DN` | Directory layout of the snapshot |
| `AUDIT_ENABLED`, `AUDIT_MAX_RECORDS` | Audit log on/off and its bound |
| `PLUGINS_ENABLED`, `PLUGINS_CONFIG` | See [plugins.md](plugins.md) |
| `CLUSTERS` | JSON list of `{id, name, fingerprint}` for the clusters |
| `SYNC_CLIENT_FINGERPRINTS` | Client certificates allowed on `/sync/*` |
| `SYNC_DNS_NAME`, `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_RECORD_ID`, `CLOUDFLARE_API_TOKEN` | Where the snapshot hash is published |
| `TELEMETRY_SAMPLE_RATIO` | Share of new traces recorded by telemetry plugins |

## Tests

```sh
make check      # everything below, plus govulncheck and npm audit
make vet        # gofmt and go vet
make test       # Go tests with the race detector
make js         # Worker bridge tests (node --test)
make web        # web tests (vitest) and a production build
make wasm       # the Worker build compiles
make ldap-it    # slave against a real slapd (set SLAPD and SLAPD_SCHEMA)
```

The cluster scripts have Python unit tests next to them:

```sh
for t in $(git ls-files 'deploy/*test_*.py'); do
  (cd "$(dirname "$t")" && python3 -m unittest -q "$(basename "$t" .py)")
done
```

Run them with `umask 022`. The TLS deploy script correctly refuses group-writable files, and its
test fixtures inherit your umask.

CI (`.github/workflows/ci.yml`) runs the Go, web and script tests on every push to `main` and
on every pull request.

### Writing tests

- Use `internal/testutil` for a migrated in-memory database with fixtures.
- Both store implementations must pass `store/storetest`. Add a case there when you rely on new
  SQL behaviour.
- Use `objstore/objstoretest` for a fake R2.
- Keep tests offline. Live tests (R2, Cloudflare Access) are opt-in through environment
  variables and skip otherwise.

## Conventions

- **Every route declares its permission.** Use `authz.Policy(object, action)` for anything
  admin. The router panics at start on a missing one.
- **Errors are `apperr`.** Return `apperr.BadRequest(...)`, `apperr.New(status, code, msg)` or a
  predefined error. Wrap internal causes with `.Wrap(err)`; they are logged and never sent to
  the client.
- **Schema changes are new migrations.** Add `migrations/00NN_name.sql`, separate statements
  with `--;` lines, and never edit an applied file. SQL must run on both SQLite and D1.
- **Mind the bill.** D1 charges per row read and written, and the Worker per request and CPU
  time. Prefer `EXISTS`/`LIMIT 1` to `COUNT(*)`, hit an index, avoid full-table reads per request
  or per cold start, and mark periodic machine reports `Routine`.
- **Secrets never sit in plain text in the database.** Seal them with `Host.Secrets`, or take
  them from `env:NAME`.
- **Comments.** Every package has a package comment. Exported types and functions have a doc
  comment that says what they do and anything a caller must know (limits, side effects, nil
  safety). Write comments in English.
- **Formatting.** `gofmt` for Go; the existing style for JS (no build-time formatter).
- **UI text.** Write UI strings in Chinese, wrapped in `t("…")`, and add the English translation
  to `web/src/locales/en.json`. Do not translate e-mail templates.

## Working on…

### The Worker

`deploy/worker/build.sh` builds `dist/main.wasm` and `web/dist`. The JavaScript in
`deploy/worker/` only bridges platform bindings: keep business logic in Go. Bridge behaviour is
covered by `*.test.mjs`.

### The slave

```sh
go run ./cmd/slave -config slave.yml -once      # one check and exit
go run ./cmd/slave -plan -snapshot snap.json -ldif cluster.ldif -trust-dir trust …   # offline dry run
```

`cmd/migration-check -snapshot-out` exports a snapshot from a database copy. The config file
must be mode 0600.

### The gateway

The gateway needs a login node with sshd that trusts the portal's CA. For local work, use the
unit tests in `internal/gateway`; they run a real SSH server in process.

## Commits and pull requests

- Branch from `main`. Use one topic per pull request.
- The first line of a commit message names the area and says what changed for the user or
  operator, for example `slave: refuse a plan that drops a trusted CA`.
- `make check` must pass. Say in the pull request how you tested anything that tests cannot
  cover, such as a cluster or Cloudflare.
