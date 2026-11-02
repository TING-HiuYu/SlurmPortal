# Deployment

This wiki walks you step by step from fresh **Ubuntu 24.04 LTS** servers to a production Slurm cluster managed by Slurm Portal: accounts and groups provisioned in the portal automatically sync to the cluster's OpenLDAP directory, users log in with short-lived SSH certificates signed by the portal CA, jobs run under each user's Slurm accounting association, and researchers can access an interactive terminal and file manager directly in the browser.

Every command in this guide was verified on clean Ubuntu 24.04 hosts. Any step that requires site-specific hardware or external DNS (such as physical NVIDIA GPUs or public ACME certificates) is explicitly noted.

## The pieces

A Slurm Portal deployment consists of one central **master** control plane and one **slave** agent per cluster:

| Component | Role | Where It Runs |
| --- | --- | --- |
| **Master** | Control plane: web UI, REST API, identity database, SSH/X.509 CAs, and audit log | Cloudflare Workers (serverless) or a Linux host (Docker Compose) |
| **Slave** | Reconciles master snapshots into local OpenLDAP, publishes SSH trust files, runs account triggers, and reports cluster status | One host per cluster (`ctl`); requires outbound HTTPS only |
| **Web Access Gateway** *(optional)* | Browser terminal, SFTP file browser, and cross-cluster `rsync` transfers | Each cluster's login node (`login`) |
| **Lifecycle Hook & Observer** *(optional)* | Job completion emails and per-job GPU utilization tracking | Each cluster's Slurm controller (`ctl`) |

You can deploy the master in either **serverless** or **container** mode; the cluster-side setup is identical for both.

| Dimension | [Serverless Master](master-saas.md) | [Container Master](master-container.md) |
| --- | --- | --- |
| Infrastructure | Cloudflare Workers, D1, R2, and Cloudflare Access | Docker Compose on your own host (`cmd/server` + SQLite, RustFS S3, and Caddy) |
| Operational overhead | Zero host maintenance; managed Cloudflare services | You manage the Linux host, OS updates, and local backups |
| Authentication perimeter | Cloudflare Access at the edge + portal sign-in | Portal sign-in (local password and GitHub OAuth) behind Caddy HTTPS |
| How slaves detect changes | Polls a `_sync` DNS TXT record (zero Worker requests when idle) | Polls `GET /sync/v2/snapshot-hash` over mTLS once per minute |

## The path

| Step | Guide | Estimated Time |
| --- | --- | --- |
| 1 | [Plan Your Site](plan.md): host topology, DNS names, firewall ports, and UID/GID ranges | 15 min |
| 2 | [Munge](munge.md): shared cluster authentication key | 5 min |
| 3 | [Slurm](slurm.md): building Slurm 26.05 `.deb` packages, `slurmctld`, and `slurmd` | 30 min |
| 4 | [Accounting](accounting.md): MariaDB and `slurmdbd` | 10 min |
| 5 | [slurmrestd](slurmrestd.md): JWT-authenticated Slurm REST API | 10 min |
| 6 | [OpenLDAP](openldap.md): directory service with StartTLS / LDAPS | 15 min |
| 7 | [SSSD & SSH](sssd-ssh.md): node identity resolution and compute-node access lockdown | 10 min |
| 8 | [Shared Home & Trust](shared-home.md): NFSv4 exports for `/home` and SSH trust files | 15 min |
| 9 | Deploy the Master: [Serverless (Cloudflare)](master-saas.md) or [Containers (Docker Compose)](master-container.md) | 25 min |
| 10 | [Initial Portal Setup](portal-setup.md): SSH CA, POSIX group presets, mail providers, and OAuth | 10 min |
| 11 | [Cluster Slave Agent](slaves.md): OpenLDAP sync, SSH trust publication, and Slurm triggers | 20 min |
| 12 | [Job Notifications & GPU Monitoring](optional.md) and [Web Access Gateway](gateway.md) *(optional)* | 45 min |
| 13 | [End-to-End Verification](verify.md): test user provisioning, SSH certificate login, and revocation | 10 min |

Already running a working Slurm cluster? Jump straight to **Step 6** ([OpenLDAP](openldap.md)), or to **Step 9** if your cluster already uses OpenLDAP and SSSD.

## Machine identity

Cluster slaves and Web Access Gateways authenticate to the master using X.509 client certificates, and the master binds each agent to its cluster ID by its SHA-256 certificate fingerprint:

- **Serverless master:** Client certificates are issued from Cloudflare's Client Certificate CA in the dashboard.
- **Container master:** Client certificates are issued from a local ECDSA P-256 CA managed by `deploy/certs/issue.sh` (invoked automatically by `init.sh` and `add-cluster.sh`).

Whenever you rotate an agent's client certificate, update its SHA-256 fingerprint on the master at the same time.

## Moving from go-ldap-admin v1

When pointed at an existing `go-ldap-admin` v1 database, the v2 master automatically:

- Applies additive v2 schema migrations (`0001`–`0016`);
- Performs a one-time import of v1 web-access settings, job notification settings, and GPU job history;
- Reads existing users, groups, roles, CAs, and certificates directly from the v1 base tables.

Before cutting over a production database, run `cmd/migration-check` against a backup copy to verify that `PKI_MASTER_KEY` decrypts all CA keys, and run [the slave's offline plan mode](slaves.md#5-rehearse-then-start) against each cluster's `slapcat` export to confirm `"safe": true`.

## After that

See [Day-2 Operations](operations.md) for routine administration, backups, key rotation, and troubleshooting runbooks.
