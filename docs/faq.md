# FAQ & Troubleshooting

## General

### Does Slurm Portal replace Slurm's native CLI tools?

No. Slurm continues to schedule workloads, and users still submit and inspect jobs with `sbatch`, `srun`, and `squeue`. Slurm Portal manages **identity, access, and day-to-day operations** across your clusters: POSIX accounts, groups, RBAC roles, SSH certificates, and the Slurm accounting associations derived from them. It also provides real-time cluster telemetry, automated job completion emails, and an in-browser terminal and file manager.

### Do users need to install any custom client software?

No. A standard OpenSSH client is all that is required. Users either download a 30-day SSH certificate and key pair from the portal once a month to connect from their local terminal, or open an interactive shell directly in the browser.

### Does the master need inbound network access to the clusters?

No. The master never initiates connections to a cluster. Slave agents and Web Access Gateways always dial outbound to the master over mutual TLS (mTLS). Clusters can operate behind strict NAT or firewalls that only permit outbound HTTPS.

### Should I choose the serverless or container master?

Choose the **serverless master** if you already use Cloudflare and want zero host maintenance with near-zero idle cost. Choose the **container master** if your control plane must run on on-premises hardware or inside an isolated institutional network. Both run the exact same Go application and expose the same features (aside from Cloudflare Access integration). See [Deployment Overview](deploy/README.md) for a detailed comparison.

### What happens if the master goes offline?

Clusters continue operating normally. Existing users in local OpenLDAP can still log in and submit Slurm jobs, and issued SSH certificates remain valid until their 30-day expiration. Only administrative changes (such as creating new users or revoking certificates) wait until the master is reachable again.

### Can I adopt Slurm Portal on a cluster with existing LDAP accounts?

Yes. The slave agent never modifies or deletes LDAP entries that are not managed by the master, and an empty LDAP password hash in the portal preserves any existing `userPassword` attribute in OpenLDAP. Always run [the slave's offline plan mode](deploy/slaves.md#5-rehearse-then-start) before the first sync to inspect the exact LDAP operations that would be performed.

### How do I migrate from go-ldap-admin v1?

Point the v2 master at a copy of your go-ldap-admin v1 database. On startup, it automatically runs additive schema migrations and imports legacy settings once. See [Migrating from go-ldap-admin v1](deploy/README.md#moving-from-go-ldap-admin-v1).

## Accounts and SSH Logins

### A newly created user cannot log in yet

Changes typically propagate to a cluster within 1–2 minutes: the slave agent polls every 60 seconds, and SSSD caches lookups on each node for `entry_cache_timeout` (60 seconds in our recommended configuration). Run `sss_cache -E` on the login node to flush the SSSD cache immediately, and inspect `journalctl -u slurm-portal-slave` on the controller for sync errors.

### SSH returns `Permission denied (publickey)` despite a valid certificate

Check `journalctl -u ssh` on the login node to identify the exact cause:

| `sshd` Log Message | Cause & Resolution |
| --- | --- |
| `revoked by file …/revoked.krl` | The certificate serial was revoked. Issue a new certificate in the portal. |
| `Error checking … in revoked keys file …: Permission denied` | `sshd` cannot read `revoked.krl`. Upgrade the slave binary (older builds wrote files under a restrictive `umask`) or verify permissions on the NFS trust mount. |
| `Certificate invalid: expired` | The 30-day validity window has passed. Issue a replacement under **Certificates & credentials**. |
| `Certificate … not signed by a trusted CA` | `TrustedUserCAKeys` in `sshd_config` does not point to `current/ssh/trusted-user-ca-keys.pub`, or the signing CA is disabled in the portal. |

### Nobody can log in with SSH keys after updating `sshd_config`

If `RevokedKeys` references a file path that does not exist, OpenSSH rejects **all** public-key and certificate logins. Verify that `/srv/slurm-portal-trust/current/ssh/revoked.krl` exists and that the NFS trust share is mounted.

### Job submission fails with `Invalid account or account/partition combination`

The user does not have a Slurm accounting association yet. Associations are created by the slave's trigger script (`slurm_trigger.py`). Check the slave logs for failed trigger executions. Note that the very first trigger run for a brand-new user may fail once if SSSD has not resolved the new POSIX UID yet; the slave automatically retries on the next cycle.

## Cluster Status and Email Notifications

### Cluster status pages are empty or stale

- Verify that `current-status.enabled: true` is set in `slave.yml` and that the `slurm-portal` user can run `scontrol` and `squeue`.
- Browsers fetch live status JSON directly from object storage via presigned URLs. On Cloudflare, a `"Failed to fetch"` error in the browser console indicates that your R2 bucket's CORS policy does not allow the portal origin (`deploy/worker/r2-cors.json`). On a container master using a non-standard HTTPS port, ensure `PORTAL_URL` includes the port.

### Job completion emails are not being sent

Job completion emails require:
1. Slurm 26.05+ with `slurmrestd` enabled;
2. `slurm-lifecycle-hook`, `slurm-notify-observer`, and the slave's `lifecycle-file` configured ([Job Notifications & GPU Monitoring](deploy/optional.md#job-e-mail-and-gpu-usage));
3. A working mail provider under **Mail service** and **Job notifications** enabled.

Note that only jobs with a wall-clock runtime longer than 60 seconds trigger notification emails.

## Web Terminal and File Manager

### Opening the web terminal fails with `"Failed to fetch"`

The browser connects directly to the login node's Web Access Gateway over HTTPS. Ensure the Nginx reverse proxy in front of the gateway includes the portal's origin in its CORS allowlist and presents a TLS certificate trusted by the user's browser.

### Cluster web access status shows "Not ready"

The gateway has not registered with the master within the last 3 minutes. On the login node, run `curl -sk https://127.0.0.1:19934/readyz` to inspect the failure reason—most commonly an unreachable `master_origin` or an unrecognized client certificate fingerprint.

## Cloudflare Deployment

### CLI scripts fail with HTTP 403 / Error 1010

Cloudflare's default bot protection blocks requests without a standard `User-Agent` header. Include an explicit `User-Agent` along with your Cloudflare Access service token headers (`cf-access-client-id` and `cf-access-client-secret`).

### The `_sync` DNS TXT record does not update after an edit

The Worker updates the DNS TXT record asynchronously after each write and re-verifies it on every 5-minute cron tick. Check `dig +short TXT _sync.portal.example.org` and inspect the **Diagnostics** page in the portal to verify your `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ZONE_ID`, and `CLOUDFLARE_RECORD_ID`.

## Slurm

### `slurmctld` exits with `CLUSTER ID MISMATCH`

The cluster was manually added via `sacctmgr add cluster` after `slurmctld` had already registered itself with a different cluster ID. Run `sacctmgr -i delete cluster <name>` and restart `slurmctld` so it can re-register automatically. See [Accounting](deploy/accounting.md).

### Which Slurm versions are supported?

Account provisioning, SSH certificates, live queue/node status, and the web terminal work with any modern Slurm release (including Ubuntu 24.04's packaged Slurm 23.11). Job completion emails and per-job GPU usage statistics require **Slurm 26.05 or newer** because they query the `v0.0.45` OpenAPI schema in `slurmrestd`. See [Slurm](deploy/slurm.md).
