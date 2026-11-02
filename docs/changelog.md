# Changelog

## Slurm Portal 2.0

First major release under the **Slurm Portal** name—a complete architectural redesign of `go-ldap-admin` purpose-built for multi-cluster Slurm HPC environments.

**Control Plane (Master)**

- **Dual Runtime Support:** Deploy serverless on Cloudflare (Workers, D1, R2, and Cloudflare Access) or self-host in containers (Docker Compose with SQLite, RustFS S3 storage, and Caddy). Both runtimes compile from the same Go codebase.
- **Identity & Ranked RBAC:** Unified management of POSIX users, primary/supplementary POSIX groups, hierarchical organizational units, and roles with rank-enforced anti-escalation rules.
- **Authentication & Credentials:** Sign in via local password, GitHub OAuth (with ordered rule-based group/role assignment), or Cloudflare Access; self-service email verification codes for password resets; one-way password hashing using Argon2id (portal) and SHA-512 crypt (OpenLDAP).
- **Built-in PKI:** Integrated SSH and X.509 Certificate Authorities, issuing 30-day OpenSSH user certificates and publishing revocation lists (KRLs and CRLs) across all clusters.
- **Mail Hub:** Pluggable email delivery subsystem supporting SMTP, custom HTTP REST APIs, and multi-provider load-balanced pools with daily quotas, weighted routing, automatic failover, live template previews, and per-provider delivery metrics.
- **Tamper-Evident Audit Log:** Append-only, size-bounded SHA-256 hash-chained audit trail with online integrity verification.
- **Modular Plugin System:** Built-in plugins for real-time cluster and queue telemetry, 7-day GPU utilization analytics, job completion emails, browser terminal and file management, cross-cluster file transfers, dependency diagnostics, and OpenTelemetry tracing.

**Cluster Agents**

- **Slave Agent (`cmd/slave`):** Synchronizes master snapshots into local OpenLDAP behind user-deletion and CA-removal safety guards, publishes immutable SSH trust generations, and executes ordered account/storage triggers (`sacctmgr`, NFSv4 home provisioning). Includes an offline `-plan` rehearsal mode.
- **Web Access Gateway (`cmd/gateway`):** Provides in-browser terminal sessions, SFTP file browsing, and cross-cluster `rsync` transfers executed as the authenticated POSIX user via ephemeral 5-minute WebCrypto SSH certificates.
- **Job Lifecycle Hook & Observer (`cmd/slurm-lifecycle-hook`, `cmd/slurm-notify-observer`):** Fail-open Slurm prolog/epilog notifier and read-only `slurmrestd` proxy that capture job completion events and NVML GPU metrics without delaying job scheduling or exposing the Slurm JWT signing key.

**Migration from go-ldap-admin v1**

- Opens existing v1 databases directly and performs a one-time import of legacy web-access, notification, and GPU accounting records.
- Includes `cmd/migration-check` for offline migration validation and `cmd/convert-passwords` to replace legacy reversible passwords with one-way hashes.
