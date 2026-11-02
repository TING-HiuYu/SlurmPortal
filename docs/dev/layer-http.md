# Layer: HTTP & edge

The HTTP and edge layer spans everything between an incoming network connection and a domain handler:

- The edge runtime that terminates TLS and enforces host separation;
- The HTTP router with mandatory route-level permissions;
- The request middleware pipeline;
- Strict JSON decoding and the unified error model.

**Packages:** `internal/httpx`, `internal/apperr`, `deploy/worker` (Cloudflare Worker edge), and `cmd/server` (native container edge).

## The edge

| Responsibility | Cloudflare Worker (`deploy/worker`) | Container (`cmd/server` + Caddy) |
| --- | --- | --- |
| Browser TLS termination | Cloudflare Edge (with optional Cloudflare Access gate) | Caddy reverse proxy; `BEHIND_TLS_PROXY=true` tells `cmd/server` to trust `X-Forwarded-Proto` |
| Static web assets | Cloudflare Workers Static Assets (not billed as Worker invocations) | Served from `STATIC_DIR` with SPA fallback and immutable caching for `/assets/*` |
| Machine sync endpoints | Dedicated hostname `SYNC_HOSTNAME` with Cloudflare mTLS verification | Dedicated mTLS listener `SYNC_LISTEN` (`:8443`) verified against `SYNC_CLIENT_CA_FILE` |
| Request body cap | 1 MiB enforced in `worker.js` before entering Wasm | 1 MiB per handler (`httpx.DefaultMaxBody`) |
| Client IP resolution | `CF-Connecting-IP` | `TRUSTED_IP_HEADER` (`X-Forwarded-For` behind Caddy) |
| Post-write hash publication | `ctx.waitUntil(goV2Publish())` | 5-second background ticker in `cmd/server` |

`deploy/worker/edge.js` implements two security invariants at the Worker boundary (tested by `deploy/worker/edge.test.mjs`):

- `allowed(url, env)`: ensures `SYNC_HOSTNAME` serves only `/sync/*` and `/healthz`, while browser hostnames never serve `/sync/*`.
- `verifiedPeer(request, env)`: extracts the client-certificate SHA-256 fingerprint only when the request arrived on `SYNC_HOSTNAME` and Cloudflare verified the certificate chain.

## Router

`httpx.Router` wraps Go's standard `http.ServeMux` (using Go 1.22+ method and wildcard patterns such as `GET /api/v2/users/{id}`).

```go
rt.Handle(httpx.Route{
	Method:     http.MethodGet,
	Path:       "/api/v2/hello",
	Permission: authz.Authenticated(),
	Handler: func(w http.ResponseWriter, r *http.Request) error {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"hello": "world"})
		return nil
	},
})
```

| Route Field | Purpose |
| --- | --- |
| `Permission` | **Mandatory.** Must be one of `authz.Public()`, `authz.Authenticated()`, `authz.Peer()`, or `authz.Policy(object, action)`. `Handle` panics at startup if omitted. |
| `Handler` | Returns an `error`; any non-nil error is formatted and written by `httpx.WriteError`. |
| `Routine` | Marks periodic machine telemetry endpoints so they are written to the audit log only on failure. |

`Router.Routes()` returns metadata for every registered route, which `tools/apidoc` inspects to generate the [API Reference](api-reference.md).

## Middleware

Applied in `internal/app/app.go` from outermost to innermost:

| Middleware | Behavior |
| --- | --- |
| `WithScope` | Preserves a well-formed incoming `X-Request-Id` header or generates a new UUID; attaches a mutable request scope (matched route, status code, audit target, authenticated principal) to `r.Context()`. |
| `telemetry.HTTPMiddleware` | Creates an OpenTelemetry span named after the route pattern when a telemetry plugin is enabled. |
| `Recover` | Catches panics, logs the failure with the request ID, and returns a safe `500 internal` response. |
| `SecurityHeaders` | Sets `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, and `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`. |
| `RateLimit` | Enforces a per-client-IP token bucket (`HTTP.RatePerSecond`, default `20`; `HTTP.RateBurst`, default `200`). Excess requests receive HTTP `429` (`rate_limited`). |
| `CORS` | Permits only exact origins listed in `CORS_ORIGINS` with headers `Authorization, Content-Type, X-Request-Id, Traceparent` and methods `GET, POST, PUT, PATCH, DELETE` (preflight cached for 600 s). Same-origin deployments require no CORS configuration. |
| Write tracking | Records when a non-`GET` request outside `/sync/` succeeds so the master republishes the snapshot hash. |
| `audit.Middleware` | Appends an audit log record after the handler finishes; see [Domain Modules: Audit](layer-domain.md#audit). |

## Request bodies

Handlers read JSON request payloads exclusively through `httpx.DecodeJSONLimit(w, r, &dst, limit)`:

- Requires `Content-Type: application/json` (otherwise returns HTTP `415 unsupported_media_type`);
- Enforces a strict byte limit (`1 MiB` by default; otherwise returns HTTP `413 too_large`);
- Calls `DisallowUnknownFields()`, turning mistyped field names into immediate `400 bad_request` errors instead of silently ignoring them;
- Verifies that the body contains exactly one JSON value with no trailing tokens.

Request DTOs implement `Validate() error`, which handlers invoke before executing any business logic.

## Error model

Every error returned to a client is normalized to an `apperr.Error` comprising an HTTP status code, a stable machine-readable `code`, and a human-readable `message`. `apperr.From(err)` maps any unexpected internal error to HTTP `500` (`internal`); the underlying cause is logged server-side with the request ID and never exposed to the caller.

| Error Code | HTTP Status | Meaning |
| --- | --- | --- |
| `bad_request` | `400` | Malformed JSON or failed input validation (`apperr.BadRequest(msg)`) |
| `unauthenticated` | `401` | Missing, invalid, expired, or revoked token, or disabled user account |
| `forbidden` | `403` | Caller's roles lack the required `object:action` policy, or target outranks caller |
| `not_found` | `404` | Requested resource does not exist |
| `conflict` | `409` | Uniqueness or state conflict (e.g., duplicate username or group name) |
| `too_large` | `413` | Request payload exceeds size limit |
| `unsupported_media_type` | `415` | `Content-Type` is not `application/json` |
| `rate_limited` | `429` | Client IP exceeded the token-bucket rate limit |
| `internal` | `500` | Unhandled server error (logged with `requestId`) |
| `unavailable` | `503` | Upstream or storage dependency unavailable |

Domain modules also define domain-specific codes—such as `insecure_transport` (when password login is attempted over plain HTTP) or `main_group_required`. API clients should always branch on `error.code` rather than `error.message`.

## Writing responses

- `httpx.WriteJSON(w, status, data)` writes `{"data": data}` (or an empty body for `204 No Content`).
- `httpx.WriteError(w, r, err)` writes `{"error": {"code", "message", "requestId"}}`.
- `httpx.SetTarget(ctx, "user:alice")` records the logical entity acted upon for the audit trail.

## Next steps

- [Identity & Access Layer](layer-access.md): how authentication and RBAC authorization work.
- [Request Lifecycle](request-lifecycle.md): end-to-end sequence diagrams for both runtimes.
