# Cryptography & signing

This page catalogs every cryptographic key, signature, encryption scheme, and hash function in Slurm Portal: what it protects, who verifies it, its lifetime, and how to rotate it safely. Review this reference carefully before modifying any cryptographic parameters—several domain-separation strings and AEAD additional data formats are bound to persisted ciphertexts.

## At a glance

| Mechanism | Algorithm | Signed / Sealed By | Verified / Opened By | Lifetime |
| --- | --- | --- | --- | --- |
| User session token | JWT `HS256` | Master (`JWT_KEYS`) | Master | 12 h default (max 168 h) |
| Web Access Gateway ticket | JWT `EdDSA` (Ed25519) | Master (`PLUGIN_WEB_ACCESS_TICKET_KEY`) | Web Access Gateway | ≤ 5 minutes |
| Standard SSH user certificate | OpenSSH cert, Ed25519 CA | Master (SSH CA) | Cluster `sshd` | 30 days |
| Ephemeral web-session SSH cert | OpenSSH cert, Ed25519 CA | Master (SSH CA) | Web Access Gateway & `sshd` | ≤ 5 minutes |
| SSH Key Revocation List (KRL) | OpenSSH KRL binary format | Slave (`ssh-keygen -k`) | Cluster `sshd` (`RevokedKeys`) | Rebuilt on each sync |
| User X.509 client certificate | ECDSA P-256 | Master (imported X.509 CA) | Cluster services | 1 year |
| X.509 Certificate Revocation List | X.509 v2 CRL | Master | Cluster services | 48 h (re-signed when < 12 h remain) |
| Machine peer identity | mTLS, ECDSA P-256 (`issue.sh`) | Private Sync CA or Cloudflare CA | Master (by SHA-256 fingerprint) & agents | 825 days default |
| CA private keys at rest | AES-256-GCM | `PKI_MASTER_KEY` | Master | Until CA deletion |
| Stored provider secrets | AES-256-GCM + HKDF-SHA256 | `PKI_MASTER_KEY` (`internal/secretbox`) | Master | Until secret update |
| Replication snapshot | SHA-256 | Master | Slave | Per sync cycle |
| Audit log integrity | SHA-256 hash chain | Master | `GET /api/v2/audit/verify` | Retained audit rows |
| Portal login password | Argon2id | Master | Master | Until password change |
| OpenLDAP user password | `{CRYPT}$6$` (SHA-512 crypt) | Master | OpenLDAP / SSSD | Until password change |
| Password reset code | HMAC-SHA256 | Master (derived from active JWT key) | Master | 10 minutes |

## Keys you provide

| Secret | Format | Purpose | Rotation Procedure |
| --- | --- | --- | --- |
| `JWT_KEYS`, `JWT_ACTIVE_KID` | `kid1:base64,kid2:base64` (≥ 32 bytes each) | Signs user session JWTs and keys password-reset HMACs | Append a new `kid:key` pair to `JWT_KEYS`, update `JWT_ACTIVE_KID`, deploy, and remove the old key after the maximum token TTL expires |
| `PKI_MASTER_KEY` | Base64-encoded 32 random bytes | Encrypts CA private keys and stored provider secrets | **Do not rotate without re-sealing all database rows.** Keep an offline backup |
| `PLUGIN_WEB_ACCESS_TICKET_KEY` | Base64-encoded 32-byte Ed25519 seed | Signs short-lived Web Access Gateway tickets | Generate a new seed under a new `ticket-key-id`, add the new public key to every gateway's `ticket_public_keys`, then switch the master |
| mTLS Sync CA & client certs | PEM (`deploy/certs/issue.sh`) | Authenticates slave agents and gateways to the master | Issue a new client certificate, add its SHA-256 fingerprint to the master, deploy, and then swap the certificate on the agent |

:::warning
Losing `PKI_MASTER_KEY` permanently renders all sealed CA private keys and stored mail/OAuth secrets unreadable. Previously issued user certificates will continue working on clusters until they expire, but no new certificates can be signed until replacement CAs are generated and synced.
:::

## Session tokens

- Strictly enforces `HS256`; tokens declaring any other `alg` header are rejected before signature verification.
- Standard claims: `iss`, `aud`, `sub`, `jti`, `iat`, `nbf`, `exp`, and `src`. The JWT header's `kid` selects the verification key from `JWT_KEYS`, enabling zero-downtime key rotation.
- Revoked token IDs (`jti`) are recorded in `v2_revoked_tokens` on logout or refresh and retained until the token's natural expiration.
- Every authenticated request queries the user's current status and assigned roles from the database, ensuring role changes and account deactivations take effect immediately.

See [Identity & Access Layer](layer-access.md#users-session-tokens) for additional details.

## Gateway tickets and web-session certificates

Browser terminal and file manager sessions never expose long-lived SSH keys to the browser:

1. The browser generates a fresh, non-exportable **Ed25519 key pair** in memory via WebCrypto.
2. `POST /api/v2/web-access/ssh-certificate` signs an **OpenSSH user certificate** for that public key valid for at most 5 minutes (`principal = username`, extension `permit-pty` only, key ID `web-access/<cluster>/access/user:<id>/<serial>`).
3. `POST /api/v2/web-access/ticket` issues an **Ed25519-signed JWT ticket** (`aud = gatewayId`, `clusterId`, permitted `scopes`, 300-second TTL).
4. The Web Access Gateway verifies the ticket against `ticket_public_keys`, validates the SSH certificate against its trusted CAs, and confirms that both credentials name the exact same user and cluster.

Because these ephemeral certificates expire within 5 minutes (`pki.MaxTemporarySSH`), they are not persisted in `user_certificates` or added to KRLs.

## SSH certificate authority

- SSH CAs use **Ed25519** keys, either generated inside the portal or imported by an administrator.
- **Standard user certificates** are valid for 30 days. The master generates an Ed25519 key pair, signs the public key, and returns the private key once in the HTTP response without storing it.
- Certificate principals and Key ID are set to the user's POSIX username, with standard interactive extensions (`permit-pty`, `permit-X11-forwarding`, `permit-agent-forwarding`, `permit-port-forwarding`).
- **Revocation (KRL):** Revoking a certificate inserts its serial number into `revoked_certificates`. On the next sync cycle, each slave compiles a binary OpenSSH Key Revocation List (`ssh/revoked.krl`) via `ssh-keygen -k`, which `sshd` enforces via `RevokedKeys <trust>/current/ssh/revoked.krl`.
- **CA removal guard:** `ssh/trusted-user-ca-keys.pub` contains all enabled SSH CAs. Slaves refuse any snapshot that drops a currently trusted SSH CA unless `limits.allow-ca-removal: true` is explicitly configured.

## X.509 authority

- X.509 CAs are imported as a PEM certificate and private key pair.
- User certificates use **ECDSA P-256** (`Subject CN = username`, Extended Key Usage `clientAuth`, 1-year TTL clamped to the CA's expiration) and are written to OpenLDAP as `userCertificate;binary`.
- **CRLs** have a 48-hour validity window and are automatically re-signed by the periodic background tick whenever less than 12 hours remain (as well as immediately upon revocation). Slaves publish them to `crl/ca-<id>.pem`.

## Machine identity (mutual TLS)

`deploy/certs/issue.sh` provisions a 10-year ECDSA P-256 private root CA along with server and client certificates (default 825-day validity, configurable via `DAYS=`):

- **Master verification:** Cloudflare mTLS (or `SYNC_CLIENT_CA_FILE` on `cmd/server`) verifies the X.509 certificate chain. The master then checks the leaf certificate's SHA-256 fingerprint against `SYNC_CLIENT_FINGERPRINTS`, `CLUSTERS`, and `web-access.gateways`.
- **Agent verification:** Slaves and gateways verify the master's server certificate against `tls.ca-file` and `tls.server-name`.
- **Filesystem permissions:** All private key files must have mode `0600`; agents refuse to start if permissions are overly permissive.

Store `ca.key` offline once your cluster certificates have been issued.

## Sealing secrets at rest

Two envelope schemes encrypt secrets stored in the database, both rooted in `PKI_MASTER_KEY`:

| Property | CA Private Keys (`internal/domain/pki`) | Provider Secrets (`internal/secretbox`) |
| --- | --- | --- |
| Cipher | AES-256-GCM | AES-256-GCM |
| Encryption key | Raw 32-byte `PKI_MASTER_KEY` | `HKDF-SHA256(PKI_MASTER_KEY, info = "go-ldap-admin/" + purpose)` |
| AEAD additional data (AAD) | SHA-256 of the CA's identity metadata | Purpose string (e.g., `mail-provider`, `oauth-connector`) |
| Protected data | SSH and X.509 CA private keys | SMTP passwords, REST mail API tokens, OAuth client secrets |

Binding the identity or purpose as AEAD additional data prevents cross-row or cross-purpose ciphertext substitution attacks.

:::danger
The HKDF info prefix `"go-ldap-admin/"` and the purpose strings are cryptographically bound into every stored ciphertext. Changing them will break decryption of existing database records; they intentionally retain the project's historical name for compatibility.
:::

## Hashes

- **Snapshot digest:** SHA-256 of the canonical snapshot JSON, published as `v2:<hex>`. Slaves recompute the SHA-256 over the downloaded body and verify it against the `X-Data-Hash` header.
- **Audit hash chain:** Each record's `hash` is the SHA-256 of its canonical JSON representation (with `hash` empty), which embeds the preceding record's `prevHash` (64 zeroes for the genesis record). Sensitive fields (`password`, `token`, `secret`, `key`) are redacted before hashing.
- **OAuth CSRF state:** A random token set in an HTTP-only cookie; only its SHA-256 digest is stored in `oauth_states` (10-minute TTL).

## Passwords

| Target | Algorithm & Parameters | Verified By | Database Column |
| --- | --- | --- | --- |
| Portal UI / API | Argon2id (2 passes, 19 MiB RAM, 1 thread, 16-byte random salt, PHC string) | Master (`internal/password`, constant time) | `v2_user_credentials.app_hash` |
| Cluster OpenLDAP | `{CRYPT}$6$` (SHA-512 crypt, 100,000 rounds) | OpenLDAP / SSSD on cluster nodes | `v2_user_credentials.ldap_hash` → LDAP `userPassword` |

`cmd/convert-passwords` migrates legacy reversible RSA passwords from `go-ldap-admin` v1 into these one-way hashes.

## Next steps

- [Security Model](security.md): how these cryptographic primitives enforce system trust boundaries.
- [Day-2 Operations](../deploy/operations.md): step-by-step runbooks for rotating keys and certificates in production.
