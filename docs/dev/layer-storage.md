# Layer: storage

All persistent state on the master is accessed through a compact SQL storage abstraction backed by Cloudflare D1 in serverless deployments and SQLite in container deployments. High-frequency live cluster telemetry is stored separately in S3/R2 object storage.

**Packages:** `internal/store`, `internal/store/d1`, `internal/store/storetest`, `migrations`, `internal/objstore`, and `internal/statusreport`.

## The interface

```go
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	Exec(ctx context.Context, query string, args ...any) (Result, error)
}

type Store interface {
	Querier
	// Batch executes all statements atomically: all commit or none do.
	Batch(ctx context.Context, stmts []Statement) ([]Result, error)
	Close() error
}
```

- `store.QueryRow(ctx, q, query, args, &dest…)` scans a single row and returns `store.ErrNoRows` when the result set is empty.
- Interactive multi-roundtrip SQL transactions (`BEGIN` … `COMMIT` across Go function calls) are intentionally omitted because Cloudflare D1 does not support them across the Worker bridge. Instead, related mutations are grouped into a single `store.Batch`, which executes atomically on both D1 and SQLite.
- All queries are written in standard SQLite dialect, as D1 is built on SQLite.

## Implementations

| Aspect | `store.SQL` (Native Container) | `d1.Store` (Cloudflare Worker) |
| --- | --- | --- |
| Underlying driver | `database/sql` with `modernc.org/sqlite` (pure Go, no CGo) | JavaScript bridge `goD1Bridge` (`deploy/worker/d1-bridge.js`) wrapping the D1 `DB` binding |
| `Batch` execution | Single `database/sql` transaction (`BeginTx` / `Commit`) | Single `DB.batch(...)` call (up to 100 statements per batch) |
| Connection model | Single writer (`SetMaxOpenConns(1)`) with SQLite WAL mode enabled | Managed by Cloudflare's D1 edge proxy |
| Binary `BLOB` columns | Native `[]byte` | Base64-encoded `{"blob": "…"}` across the Wasm boundary |

Both implementations are verified against `store/storetest.Run(t, store)`, which tests parameter binding, `NULL` handling, binary blobs, atomic batch rollback on constraint failure, and `ErrNoRows`. The JavaScript D1 bridge is additionally covered by `deploy/worker/d1-bridge.test.mjs`.

## Migrations

Schema migrations live in `migrations/*.sql`, are embedded into the binary via `migrations.FS`, and run automatically on startup via `store.Migrate`:

- Each file executes once in lexicographical order and records its completion in `v2_schema_migrations`.
- Individual SQL statements within a migration file are separated by a line containing only `--;`.
- **Additive-only policy:** Applied migration files are never edited or deleted; new schema changes are added as `00NN_<description>.sql`. Because migrations only add tables, columns, or indexes, the previous release binary can safely run against a newly migrated database during a rollback.
- **Concurrent cold-start safety:** On Cloudflare Workers, multiple isolates may cold-start simultaneously after a deploy. Both `store.Migrate` and `legacyimport.Run` use idempotent statements so concurrent execution is safe.

See [Data Model](data-model.md) for the complete table reference and entity relationship diagram.

## Object storage

Live cluster status snapshots are stored in S3-compatible object storage rather than the SQL database. `internal/objstore` is a lightweight, zero-dependency S3 client (AWS Signature Version 4) scoped to a single bucket:

- Connects to **Cloudflare R2** in serverless deployments or **RustFS** (or any S3-compatible service) via `endpoint` in container deployments.
- Supports conditional writes (`Put` with `If-Match` / `If-None-Match`) so an older status report that arrives out of order can never overwrite a newer snapshot.
- Generates short-lived presigned URLs (`PresignGET`, 15-minute default TTL) so browsers fetch live cluster status directly from R2/S3 without hitting the master API or database.

`objstore/objstoretest` provides an in-memory S3 server for unit tests, while `TestLiveR2` (`internal/plugins/currentstatus/live_test.go`) validates conditional writes and presigned reads against a live R2 bucket when credentials are supplied.

## Cost-aware queries

Cloudflare D1 bills per row read and written, and Worker isolates may cold-start frequently. Database code across this repository adheres to the following practices:

- Use `EXISTS` or `LIMIT 1` rather than `COUNT(*)` when checking for row existence.
- Ensure every recurring query is backed by a covering or partial index; avoid full-table scans on request paths or cold starts.
- Mark periodic machine telemetry routes with `Routine: true` so successful reports do not insert rows into `v2_audit_log`.
- Serve live cluster status from R2/S3 and static assets via Workers Static Assets (`run_worker_first` routes only `/api/*`, `/sync/*`, and `/healthz` to the Worker).
- Update the `_sync` DNS TXT record only when a write occurs or during the 5-minute cron tick—never on read requests.

Use `wrangler d1 insights <database>` and Cloudflare Worker analytics to verify row read/write counts before and after schema or query changes.

## Next steps

- [Data Model](data-model.md): table schemas and relationships.
- [Sync Protocol](sync-protocol.md): how database state is compiled into a cluster snapshot.
- [Development Guide](development.md): running the storage conformance test suite.
