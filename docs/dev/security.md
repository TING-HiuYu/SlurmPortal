# Security model

This page outlines the security architecture of Slurm Portal: its trust boundaries, authentication and authorization controls, machine identity verification, secret storage, and audit guarantees. To report a security vulnerability, please follow [SECURITY.md](https://github.com/TING-HiuYu/SlurmPortal/blob/main/SECURITY.md).

## Trust boundaries

| Boundary | Caller | Protection Mechanism |
| --- | --- | --- |
| Browser → Master | Users and administrators | Optional Cloudflare Access gate at the edge, followed by an HS256 portal session token (`authn.JWT`) |
| Machine Agent → Master | Cluster slaves and gateways | Mutual TLS (mTLS) client certificate verified on the dedicated sync endpoint (`SYNC_HOSTNAME` or `:8443`), pinned by SHA-256 fingerprint |
| Browser → Gateway | Signed-in users | Short-lived (5 min) Ed25519-signed gateway ticket bound to the user ID, cluster ID, and permitted scopes |
| Gateway → Login Node `sshd` | Web Access Gateway acting as the user | Ephemeral (5 min) OpenSSH user certificate signed for a browser-generated WebCrypto key pair |
| User SSH Client → Cluster | Researchers and engineers | OpenSSH user certificate (30-day TTL) verified against the portal CA and the cluster's local `revoked.krl` |

On Cloudflare, `deploy/worker/edge.js` enforces strict separation between browser traffic and machine traffic:

- `/sync/*` routes return `404` on every hostname except `SYNC_HOSTNAME`;
- `SYNC_HOSTNAME` rejects all non-`/sync/*` routes (except `/healthz`);
- Client certificate fingerprints are accepted exclusively on `SYNC_HOSTNAME`.

## Users

### Signing in

- **Password authentication:** Verifies Argon2id hashes in constant time. Requests for non-existent usernames execute a dummy Argon2id verification (`password.VerifyDummy`) to prevent username enumeration via timing side channels. Five failed attempts within 15 minutes temporarily lock that username/IP pair. Requires HTTPS.
- **GitHub OAuth:** Evaluates a connector's ordered rules (everyone, email/username glob, or verified GitHub organization membership) to determine whether a user may sign in and which POSIX groups and RBAC roles a newly provisioned account receives. Only the user's primary verified GitHub email address is accepted.
- **Cloudflare Access:** Verifies the `Cf-Access-Jwt-Assertion` signature against the team's JWKS endpoint and application `aud` tag before matching the user by email.

### Sessions

All sign-in flows issue an HS256 JWT session token with a configurable TTL (default 12 hours). Signing out or refreshing a token records its `jti` in `v2_revoked_tokens`. Furthermore, every request reloads the user's active status and current roles from the database, so disabling an account or revoking a role takes effect immediately.

### Passwords

- Passwords are stored exclusively as one-way hashes: Argon2id for portal authentication and `{CRYPT}$6$` (SHA-512 crypt, 100,000 rounds) for OpenLDAP.
- Plaintext passwords exist only in memory during the request that sets them.
- Legacy reversible RSA password ciphertexts from `go-ldap-admin` v1 were removed in migration `0015`.

### Reset codes

Self-service password reset codes are 6-digit numeric strings sent via email and stored only as an HMAC-SHA256 digest keyed from the active JWT signing key. Codes expire after 10 minutes, enforce a 60-second resend cooldown, and are invalidated after 5 failed verification attempts.

## Permissions

- **Mandatory route permissions:** `httpx.Router` refuses to register any route that omits an explicit `authz.Permission`.
- **Role-based policy evaluation:** Roles grant `(object, action)` permissions evaluated on every request. Client-side UI visibility (`GET /api/v2/me/access`) is purely cosmetic and never trusted for access control.
- **Rank-enforced anti-escalation:** Every role has a numeric rank (`sort`, where `1` is the super-administrator and lower numbers outrank higher numbers). Callers cannot create, modify, disable, or delete any user or role ranked at or above their own best rank, nor can they grant any `(object, action)` permission they do not personally hold.
- **Automatic certificate revocation:** Disabling or deleting a user revokes all of their active SSH and X.509 certificates in the same atomic database batch.

## Machines

- **Certificate fingerprint pinning:** Every slave and gateway certificate is registered by its SHA-256 fingerprint and bound to a specific cluster or gateway ID. Self-reported cluster IDs in request bodies are ignored.
- **Slave safety guards:** Before touching OpenLDAP or trust files, `slave.Guard` aborts any sync that would delete more than `limits.max-user-deletions` users (default `5`) or remove a currently trusted SSH CA (unless `limits.allow-ca-removal` is enabled)—preventing a misconfigured or compromised snapshot from wiping a cluster. Unmanaged LDAP entries are never touched.
- **Atomic trust generations:** Slaves write CA bundles, CRLs, and OpenSSH KRLs into immutable content-addressed directories and atomically flip the `current` symlink.
- **Isolated Slurm JWT key:** The privileged Slurm JWT key remains on the controller inside `slurm-notify-observer`, which exposes only authenticated, read-only job and node queries over mTLS.

## Keys and secrets

| Secret | Storage Location | What It Protects |
| --- | --- | --- |
| `PKI_MASTER_KEY` | Worker secret / master `.env` | Encrypts CA private keys, SMTP passwords, REST mail tokens, and OAuth client secrets at rest using AES-256-GCM (`internal/secretbox`) |
| `JWT_KEYS` | Worker secret / master `.env` | Signs and verifies user session tokens and password reset HMACs |
| `PLUGIN_WEB_ACCESS_TICKET_KEY` | Worker secret / master `.env` | Ed25519 private seed signing gateway tickets; gateways hold only the public key |
| Slave & gateway client keys | Cluster host filesystem (`0600`) | Authenticates machine agents to the master; master stores only SHA-256 fingerprints |
| CA private keys | Database (`certificate_authorities.private_key_enc`) | Sealed with `PKI_MASTER_KEY`; never exported in snapshots or API responses |

All agent configuration and key files containing secrets must have permissions `0600` (or stricter); the slave, gateway, and lifecycle hook refuse to start if group or world permissions are set.

## Audit

- **Complete mutation coverage:** Every state-changing request—including denied (`401`/`403`) and failed attempts—is recorded with actor, action, target, outcome, HTTP status, and request/trace IDs.
- **Tamper-evident hash chain:** Each audit record includes the SHA-256 hash of the previous record (`prevHash`) in its own digest (`hash`). Calling `GET /api/v2/audit/verify` walks the chain and pinpoints any modified or deleted row.
- **Bounded retention:** `AUDIT_MAX_RECORDS` caps the table size by pruning the oldest records on insert; chain verification begins from the oldest retained entry.
- **Routine report filtering:** Periodic machine status reports (`Routine: true`) are logged only when they fail.

## HTTP hardening

- **Defensive response headers:** Every API response sets `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, `Cache-Control: no-store`, and `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`.
- **Per-IP rate limiting:** Token-bucket limiter keyed by the verified client IP.
- **Strict CORS:** Only exact origins configured in `CORS_ORIGINS` are permitted.
- **Strict JSON parsing:** Request bodies are capped at 1 MiB and decoded with `DisallowUnknownFields()`.

## Gateway

The Web Access Gateway never reads or writes user files as a privileged service account. Every terminal shell, SFTP operation, and cross-cluster `rsync` transfer runs over a local SSH connection authenticated as the end user with a 5-minute certificate, delegating all filesystem access control to the login node's kernel and POSIX permissions.
