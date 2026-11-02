# Operations

This guide covers day-to-day administration, backup and recovery procedures, cryptographic key rotation, and troubleshooting. Whenever something looks unhealthy, start at **System → Diagnostics** (`/admin/diagnostics`), which displays the live status of the database, cluster sync agents, Web Access Gateways, and mail providers on a single topology view.

## Everyday tasks

| Administrative Task | Console Location & Notes |
| --- | --- |
| Create a user, assign POSIX groups, and set RBAC roles | **Admin → Users**. New accounts sync to every cluster's OpenLDAP directory within ~60 seconds. |
| Disable or re-enable a user | **Admin → Users** → Status toggle. Disabling a user atomically revokes their active SSH and X.509 certificates. |
| Reset a user's password | **Admin → Users → Reset password** (generates a one-time random password), or the user's self-service **Forgot password?** link on the login page. |
| Issue, rotate, or revoke an SSH certificate | **Admin → Users** → Certificate actions, or self-service under **Workspace → Certificates & credentials**. |
| Generate or import a Certificate Authority | **Admin → Certificate authorities** |
| Configure SMTP, REST, or pool email providers | **Admin → Mail service** (includes test delivery and daily per-provider delivery metrics). |
| Edit job, transfer, or password email templates | **Admin → Job notifications**, **Transfer notifications**, and **Password codes** (with live HTML preview). |
| Configure GitHub OAuth sign-in rules | **Admin → OAuth connectors** (evaluates rules in order to permit login and assign initial groups/roles). |
| Inspect and verify the audit log | **Admin → Audit log** (or run `GET /api/v2/audit/verify` to validate the entire SHA-256 hash chain). |

## How changes reach the clusters

1. **Snapshot publication:** When an administrator saves a change, the master computes the new snapshot SHA-256 hash and publishes it—either to the `_sync` DNS TXT record (serverless master) or at `GET /sync/v2/snapshot-hash` (container master).
2. **Slave polling:** Each cluster's slave checks the published hash every 60 seconds (`check-interval`) and downloads `GET /sync/v2/snapshot` over mTLS when the hash changes.
3. **Directory & trust reconciliation:** The slave applies minimal LDAP diffs, writes a new immutable SSH trust generation, and executes queued triggers (`slurm_trigger.py`).
4. **Node SSSD caching:** SSSD on cluster nodes caches positive lookups for `entry_cache_timeout` (60 seconds in our recommended setup; SSSD's upstream default is 90 minutes). Run `sss_cache -E` on a node to invalidate its cache immediately.

### "A change did not reach a cluster"

Work through the pipeline in order:

1. **Was the new snapshot hash published?**
   - **Serverless master:** Run `dig +short TXT _sync.portal.example.org` and verify that it returns `"v2:<hash>"` and updates after an edit. If not, check **Diagnostics** and the Cloudflare Worker logs.
   - **Container master:** Inspect `docker compose logs master` for snapshot publication errors.
2. **Is the cluster slave healthy?**
   - On `ctl`, run `curl -s 127.0.0.1:8081/readyz` to inspect `ready`, `hash`, `dirty`, and `lastOk`.
   - Check the slave logs via `journalctl -u slurm-portal-slave` (or `docker compose logs slave`). Common failure causes include:
     - An expired or unrecognized mTLS client certificate (`401`/`403` from the sync endpoint);
     - A safety guard abort (the sync would delete more than `limits.max-user-deletions` users or remove a trusted SSH CA);
     - OpenLDAP bind or connectivity errors.
3. **Is the entry present in OpenLDAP?**
   - Run `ldapsearch -x -ZZ -H ldap://infra.example.org -b dc=example,dc=org uid=<username>`. If the entry is present in OpenLDAP but missing on `login`, flush SSSD on `login` with `sss_cache -u <username>`.
4. **Did the account trigger succeed?**
   - Pending trigger events are stored as files in `/var/lib/slurm-portal/` and processed in strict FIFO order. If an event fails, it blocks subsequent events in the queue until the underlying cause (NSS resolution, NFSv4 mount, `sudoers`, or `slurmdbd`) is resolved. Fix the root cause rather than deleting queue files; triggers are idempotent and will retry automatically.

## Cluster status looks stale

- Each cluster card displays the timestamp of its most recent report; cards are marked stale once that timestamp exceeds the plugin's `stale-seconds` threshold.
- Verify that `current-status.enabled: true` is set in `slave.yml` and inspect the slave log for `scontrol` or `squeue` errors.
- Browsers read status objects directly from R2/S3 via presigned URLs. If the browser console shows `"Failed to fetch"`, verify that your R2 bucket CORS policy (`deploy/worker/r2-cors.json`) includes the portal origin.

## Web terminal or files fail

- **`"Failed to fetch"` in the browser:** Confirm that the Nginx reverse proxy on `login` includes your portal origin in its CORS allowlist and presents a valid TLS certificate.
- **Gateway shown offline in Diagnostics:** The gateway re-registers with the master every few minutes. Check `systemctl status web-access-gateway`, `curl -sk https://127.0.0.1:19934/readyz`, and its mTLS client certificate.
- **SSH connection refused by `sshd`:** Confirm that `sshd` on `login` trusts `<trust-dir>/current/ssh/trusted-user-ca-keys.pub` (`sshd -T | grep -i trustedusercakeys`).

## Mail

- Use **Admin → Mail service → Test** to send a diagnostic message through any provider and inspect the raw upstream response.
- Daily delivery counts are tracked per provider and notification purpose (`v2_mail_usage`). Deliveries routed through a `pool` provider are credited to the specific member provider that sent the message.
- Permanent recipient rejections (`4xx`/`5xx` recipient errors) are not retried across pool members, whereas transport or quota failures automatically fail over to the next available provider in the pool.

## Keys and certificates

| Credential | Rotation Procedure |
| --- | --- |
| **User session signing keys (`JWT_KEYS`)** | Append a new `kid:base64` entry to `JWT_KEYS`, switch `JWT_ACTIVE_KID` to the new `kid`, and redeploy. Remove the old key after `auth.token-ttl` (default 12 h) has elapsed. |
| **Slave or gateway mTLS client certificate** | Issue a new client certificate, append its SHA-256 fingerprint to `SYNC_CLIENT_FINGERPRINTS` and update `CLUSTERS` / `WEB_ACCESS_CONFIG` on the master, redeploy the master, and then replace the `.crt`/`.key` files on the agent. |
| **Gateway ticket signing key (`PLUGIN_WEB_ACCESS_TICKET_KEY`)** | Generate a new Ed25519 seed under a new `ticket-key-id`, add its public key to `ticket_public_keys` on every gateway (`systemctl reload web-access-gateway`), then update the master's `PLUGIN_WEB_ACCESS_TICKET_KEY` and `ticket-key-id`. |
| **SSH user certificates** | Valid for 30 days. Users can issue a fresh certificate anytime under **Certificates & credentials**, or administrators can rotate/revoke them under **Users**. Revocations propagate via `revoked.krl` on the next sync. |
| **Master encryption key (`PKI_MASTER_KEY`)** | **Do not change without re-encrypting all sealed database rows.** Keep a secure offline backup of this key. |

## Backups and recovery

- **Master database:**
  - **Serverless (D1):** Run `npx wrangler@4 d1 export <database> --remote --output backup.sql` for logical exports, or use Cloudflare D1 Time Travel for point-in-time recovery.
  - **Containers (SQLite):** Back up `deploy/compose/master/data/master/` (via `sqlite3 portal.db ".backup copy.db"`), `.env`, and `secrets/`.
- **SSH trust files:** Each slave trust update writes an immutable generation directory. To roll back immediately on a cluster, repoint the symlink: `ln -sfn <previous-generation> <trust-dir>/current`.
- **OpenLDAP directory:** Export backups with `slapcat -n 0` (config) and `slapcat -n 1` (data) before major maintenance. Master-managed entries can always be reconstructed from the latest master snapshot.
- **Master application rollback:** Run `npx wrangler@4 rollback` (serverless) or check out the previous git tag and run `docker compose up -d --build` (containers). Because database migrations are strictly additive, the previous release binary remains compatible with the newer schema.

## Scripted access

When calling the portal API from CLI scripts on a Cloudflare-protected deployment, include your Cloudflare Access service token headers (`cf-access-client-id` and `cf-access-client-secret`) along with a standard `User-Agent` header (requests without a `User-Agent` are blocked by Cloudflare bot protection with error `1010`).
