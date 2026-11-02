# Development

This guide covers setting up a local development environment, running the master and web UI, executing the test suites, and following the repository's engineering conventions.

## Prerequisites

| Tool | Version | Purpose |
| --- | --- | --- |
| Go | 1.26 (see `go.mod`) | Building and testing all packages in `cmd/` and `internal/` |
| Node.js | 22 | Building the React web app and running Cloudflare Worker bridge tests |
| Python | 3.12 + `PyYAML` | Running cluster helper scripts in `deploy/` and their unit tests |
| OpenLDAP (`slapd`) | Optional | Running `make ldap-it` (the slave integration test against a live `slapd` instance) |
| Wrangler | 4 (via `npx`) | Building and deploying the Cloudflare Worker |

## Run the portal locally

Start a local standalone master server with an SQLite database in `./data`:

```sh
mkdir -p data
JWT_KEYS="dev:$(openssl rand -base64 32)" JWT_ACTIVE_KID=dev \
PKI_MASTER_KEY="$(openssl rand -base64 32)" \
BOOTSTRAP_ADMIN_USERNAME=admin BOOTSTRAP_ADMIN_PASSWORD='change-me-now-123' \
SYSTEM_ROLE=standalone HTTP_LISTEN=127.0.0.1:8080 \
go run ./cmd/server
```

- Configuration is loaded from environment variables, optionally layered over a YAML file passed via `-config file.yml`.
- By default, the SQLite database is created at `data/slurm-portal.db` (override with `DB_DSN`).
- The bootstrap administrator account is created on first boot; you can omit `BOOTSTRAP_ADMIN_PASSWORD` on subsequent runs.
- Set `PLUGINS_ENABLED` to activate optional plugins (for example, `PLUGINS_ENABLED=diagnostics,job-notification`).

In a second terminal, start the Vite development server for the React web app:

```sh
cd web
npm ci
API_ORIGIN=http://127.0.0.1:8080 npm run dev    # opens http://localhost:5173
```

`API_ORIGIN` defaults to `http://127.0.0.1:8080` and proxies `/api/*` requests to your local Go server.

To test the complete multi-container stack on a single machine (Caddy, master, RustFS S3, slave agent, and a demo OpenLDAP server), follow the [Container Master guide](../deploy/master-container.md) using `localhost` as the hostname, followed by [Running the Slave in a Container](../deploy/slaves.md#in-a-container).

### Settings

`internal/config/config.go` defines every supported configuration parameter. The most commonly used environment variables are listed below:

| Environment Variable | Description |
| --- | --- |
| `SYSTEM_ROLE` | Process role: `standalone`, `master`, `slave`, or `gateway` |
| `HTTP_LISTEN`, `CORS_ORIGINS`, `TRUSTED_IP_HEADER` | HTTP listen address, allowed CORS origins, and trusted reverse-proxy client IP header |
| `STATIC_DIR` | (`cmd/server` only) Directory from which to serve the compiled frontend bundle (`web/dist`) |
| `BEHIND_TLS_PROXY` | (`cmd/server` only) Trust `X-Forwarded-Proto: https` from an upstream TLS proxy (enable only when port 8080 is unreachable directly) |
| `SYNC_LISTEN`, `SYNC_CERT_FILE`, `SYNC_KEY_FILE`, `SYNC_CLIENT_CA_FILE` | (`cmd/server` only) Mutual-TLS listener address, server certificate, and client CA bundle for `/sync/*` |
| `DB_DSN` | SQLite database file path or DSN |
| `JWT_KEYS`, `JWT_ACTIVE_KID` | Session token signing keys (`kid1:base64,kid2:base64`) and the active signing key ID |
| `PKI_MASTER_KEY` | Base64-encoded 32-byte master key used to seal CA private keys and provider secrets |
| `AUTH_ISSUER`, `AUTH_AUDIENCE` | Expected `iss` and `aud` claims in session JWTs |
| `LDAP_BASE_DN`, `LDAP_USER_DN` | Default LDAP directory suffix and user branch emitted in snapshots |
| `AUDIT_ENABLED`, `AUDIT_MAX_RECORDS` | Enables the hash-chained audit log and sets its maximum row count |
| `PLUGINS_ENABLED`, `PLUGINS_CONFIG` | Comma-separated active plugin IDs and JSON/YAML plugin configuration (see [Plugins](plugins.md)) |
| `CLUSTERS` | JSON array of `{id, name, fingerprint}` objects registering cluster slaves |
| `SYNC_CLIENT_FINGERPRINTS` | Comma-separated SHA-256 fingerprints allowed on `/sync/*` |
| `SYNC_DNS_NAME`, `CLOUDFLARE_ZONE_ID`, `CLOUDFLARE_RECORD_ID`, `CLOUDFLARE_API_TOKEN` | Cloudflare DNS TXT publishing settings for serverless snapshot discovery |
| `TELEMETRY_SAMPLE_RATIO` | Sampling ratio (`0.0`–`1.0`) for OpenTelemetry request traces |

## Tests

```sh
make check      # runs vet, test, js, web, wasm, and govulncheck + npm audit
make vet        # verifies gofmt formatting and runs go vet
make test       # runs all Go unit tests with the race detector enabled
make js         # runs Cloudflare Worker bridge unit tests (node --test)
make web        # runs Vitest unit tests and builds the production React bundle
make wasm       # verifies that all internal packages compile for GOOS=js GOARCH=wasm
make ldap-it    # runs the slave integration test against a real slapd binary (set SLAPD and SLAPD_SCHEMA)
```

Python helper scripts under `deploy/` include their own unit test suites:

```sh
for t in $(git ls-files 'deploy/*test_*.py'); do
  (cd "$(dirname "$t")" && python3 -m unittest -q "$(basename "$t" .py)")
done
```

Run the Python tests under `umask 022` (the TLS deployment script enforces strict file mode checks that reject group-writable test fixtures).

GitHub Actions CI (`.github/workflows/ci.yml`) runs Go tests, web tests, Worker bridge tests, Python script tests, Docker Compose validation, `apidoc -check`, and the full Docusaurus site build on every push and pull request.

### Writing tests

- Use `internal/testutil` to spin up a migrated in-memory SQLite database preloaded with test fixtures.
- Any new SQL query pattern relied upon by both runtimes should be covered in `internal/store/storetest`.
- Use `internal/objstore/objstoretest` when testing S3/R2 interactions offline.
- Keep unit tests hermetic and offline. Live integration tests (such as `TestLiveR2`) must be gated behind environment variables and skipped by default.

## Conventions

- **Mandatory route permissions:** Every route registered on `httpx.Router` must declare `authz.Public()`, `Authenticated()`, `Peer()`, or `Policy(object, action)`. Missing permissions cause a startup panic.
- **Structured errors (`apperr`):** Handlers return `apperr.BadRequest(...)`, `apperr.New(status, code, msg)`, or predefined domain errors. Wrap underlying internal errors with `.Wrap(err)` so they are logged server-side without leaking implementation details to clients.
- **Additive schema migrations:** Add new schema changes as `migrations/00NN_<name>.sql` with statements separated by `--;` lines. Never edit an existing migration file. Ensure all SQL runs cleanly on both SQLite and Cloudflare D1.
- **Cost-conscious queries:** Prefer `EXISTS` / `LIMIT 1` over `COUNT(*)`, ensure frequent queries use indexes, avoid full-table scans on cold starts, and set `Routine: true` on periodic machine status routes.
- **No plaintext secrets in the database:** Always seal credentials with `Host.Secrets` (`internal/secretbox`) or load them from environment variables (`env:VAR_NAME`).
- **Documentation comments:** Every Go package has a package comment, and every exported type or function has a doc comment explaining its behavior, preconditions, and error semantics.
- **Frontend localization:** Wrap UI strings in `t("…")` using Chinese as the source key and add the corresponding English translation to `web/src/locales/en.json`.

## Working on…

### The Worker

`deploy/worker/build.sh` compiles `dist/main.wasm` and `web/dist`. Keep business logic in Go and limit `deploy/worker/*.js` to thin platform bridges covered by `deploy/worker/*.test.mjs`.

### The slave

```sh
go run ./cmd/slave -config slave.yml -once      # run a single sync cycle and exit
go run ./cmd/slave -plan -snapshot snap.json -ldif cluster.ldif -trust-dir trust …   # offline dry-run rehearsal
```

Use `cmd/migration-check -snapshot-out` to export a snapshot JSON file from a database backup. Remember that `slave.yml` must have file mode `0600`.

### The gateway

In production, the Web Access Gateway connects to a login node's `sshd` configured to trust the portal's SSH CA. For local development and debugging, see the tests in `internal/gateway`, which spin up an in-process Go SSH server.

## Commits and pull requests

- Branch from `main` and keep each pull request focused on a single cohesive change.
- Write commit subject lines in `<area>: <imperative summary>` format (for example, `slave: refuse sync plans that remove a trusted SSH CA`).
- Ensure `make check`, `go run ./tools/apidoc -check`, and `npm run build` (in `site/`) pass before submitting a pull request.
