# Security model

This page describes who can do what in Slurm Portal, and what protects each boundary. To report a
vulnerability, see [SECURITY.md](../SECURITY.md).

## Trust boundaries

| Boundary | Who crosses it | Protected by |
| --- | --- | --- |
| Browser → portal | Users and admins | Cloudflare Access in front of the hostname, then a portal session token |
| Machine → portal | Slaves and gateways | A Cloudflare-verified client certificate on the sync hostname only; a WAF rule drops anything without one |
| Browser → gateway | Users | An Ed25519-signed ticket from master, valid 5 minutes, scoped to user, cluster and action |
| Gateway → login node | The gateway, as the user | An SSH certificate valid 5 minutes, for a key made in the browser |
| User → cluster | Users | SSH with the user's own key and a certificate from the portal's CA, checked against the KRL |

The browser hostname and the sync hostname are kept apart by `deploy/worker/edge.js`:

- `/sync/*` is refused everywhere except the sync hostname;
- the sync hostname serves nothing else (apart from `/healthz`);
- a client certificate is honoured only there.

## Users

### Signing in

- **Password.** The portal checks an argon2id hash. Unknown usernames take the same time as
  wrong passwords (`password.VerifyDummy`). After 5 failures in 15 minutes, that key is blocked
  for the rest of the window.
- **GitHub (OAuth).** A connector's ordered rules decide who may sign in: anyone, an e-mail or
  username glob, or a GitHub organisation. They also decide which groups a new account joins.
  The account's e-mail is its verified primary GitHub address.
- **Cloudflare Access.** The Access JWT is verified against the team's published keys and the
  application's AUD tag.

### Sessions

Every sign-in yields an HS256 session token with a configured lifetime. Logout and refresh
revoke the old token ID. Keys rotate through `JWT_KEYS` / `JWT_ACTIVE_KID`.

### Passwords

- Only one-way hashes are stored: argon2id for the portal and crypt SHA-512 for LDAP.
- Plaintext exists only inside the request that sets it.
- v1's reversible passwords were removed (migration 0015).

### Reset codes

A reset code is 6 digits, e-mailed, and stored only as an HMAC. It expires after 10 minutes, a
new code can be sent after 1 minute, and 5 wrong tries discard it.

## Permissions

- **Every route declares a permission.** The router will not start with a route that has none.
- **Roles grant `(object, action)` pairs.** The server checks them on every call. The menu the
  browser shows is a convenience, not a control.
- **Ranks stop escalation.** Each role has a rank (`sort`, lower is higher). Nobody can create,
  edit or delete a user or role ranked at or above their own. Nobody can grant a permission they
  do not hold. Rank 1 is the super-administrator.
- **Disabling a user revokes their certificates** in the same operation.

## Machines

- **Allow-listing.** Each slave and gateway certificate is listed by SHA-256 fingerprint and
  bound to one cluster or gateway ID. Identities named in a request body are never trusted; the
  certificate decides.
- **Slave guards.** A slave refuses a sync that would delete more than
  `limits.max-user-deletions` users or drop a trusted CA. A wrong or tampered snapshot therefore
  cannot lock everyone out. Entries master does not manage are left alone.
- **Trust generations.** Trust files are written as immutable generations, and `current` is
  switched atomically. A bad generation is rolled back by repointing `current`.
- **Observer.** The Slurm JWT key stays on the controller inside the observer, which allows only
  authenticated, read-only job and node requests.

## Keys and secrets

| Secret | Where | Protects |
| --- | --- | --- |
| `PKI_MASTER_KEY` | Worker secret | Seals CA private keys, SMTP and OAuth secrets in D1 (`internal/secretbox`, purpose-bound keys) |
| `JWT_KEYS` | Worker secret | Session tokens |
| `PLUGIN_WEB_ACCESS_TICKET_KEY` | Worker secret | Gateway tickets; gateways hold only the public key |
| Slave / gateway client keys | The machine, mode 0600 | Machine identity; master stores only fingerprints |
| CA private keys | D1, sealed | Never leave master; slaves and gateways see public keys only |

Configuration files that hold a secret must be mode 0600 or stricter. The slave, gateway and
lifecycle hook refuse to start otherwise.

## Audit

- **Every mutation is recorded.** That includes denied and failed ones, with actor, action,
  target, outcome and request ID.
- **The log is hash-chained.** `GET /api/v2/audit/verify` walks the chain and reports the first
  record that does not link or hash.
- **The log is bounded** by `AUDIT_MAX_RECORDS`. Past it, each new record replaces the oldest,
  and verification starts from the oldest record kept.
- **Routine machine reports** are recorded only when they fail.

## HTTP hardening

- **Headers.** API responses send `nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy:
  no-referrer`, `Cache-Control: no-store` and a deny-all CSP.
- **Rate limits.** Requests are limited per client IP, read from the connection or from a
  trusted proxy header.
- **CORS** allows only the configured origins.
- **Request bodies** are size-limited and decoded strictly: unknown JSON fields are rejected.

## Gateway

The gateway never reads user files itself. The terminal, SFTP and rsync all run over SSH as the
signed-in user, so normal Unix permissions apply. Tickets name the user, cluster, scopes and
destination clusters, and every request is checked against them.
