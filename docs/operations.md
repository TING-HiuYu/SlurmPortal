# Operations

This page covers everyday tasks and what to check when something looks wrong. The first place to
look is always **系统 → 运行诊断** (`/admin/diagnostics`). It draws every dependency (database,
each cluster's reports, gateways, mail providers, delivery failures) with its current health.

## Everyday tasks

| Task | Where |
| --- | --- |
| Create a user, set groups and roles | 用户管理. A new user appears in every cluster's LDAP within about a minute |
| Disable a user | 用户管理 → status. Their certificates are revoked at the same time |
| Reset someone's password | 用户管理 → 重置密码 (shown once), or the user's own 忘记密码 link (e-mailed code) |
| Issue, rotate or revoke a user's SSH certificate | 用户管理 → certificate actions, or the user's own 证书与凭据 page |
| Add or import a CA | 证书颁发机构 |
| Configure mail | 邮件服务: providers (SMTP, REST), pools, test send, counts per provider |
| Job, transfer and password e-mails | 作业通知, 传输通知, 密码验证码: templates, previews, test send |
| Allow GitHub sign-in | OAuth 连接器: one connector per GitHub app, with ordered sign-in rules |
| Check the audit trail | 操作日志; verify the chain with `GET /api/v2/audit/verify` |

## How changes reach the clusters

1. A change is saved in D1, and master publishes the new snapshot hash to DNS
   (`_sync.portal.example.org`).
2. Each slave checks the TXT record every 60 seconds and downloads the snapshot when the hash
   changes.
3. The slave updates LDAP and the trust files, then runs triggers (Slurm account, storage).
4. sssd on the login nodes may cache entries for up to its cache timeout (about 90 minutes)
   unless the cache is cleared.

### "A change did not reach a cluster"

1. **Is the hash published?**
   - SaaS master: run `dig +short TXT _sync.portal.example.org`. It should show `v2:<hash>` and
     change after an edit. If it does not, check the 运行诊断 page and the Worker logs. The cron
     tick republishes every 5 minutes.
   - Container master: the slave asks the master directly; check `docker compose logs master`.
2. **Is the slave healthy?** On the slave host:
   - `curl -s 127.0.0.1:8081/readyz` reports the last sync and its hash;
   - check its logs with `docker compose logs slave` (container), or
     `journalctl -u slurm-portal-slave` (systemd).
   Common causes:
   - an expired or changed client certificate (403 from the sync host);
   - a guard refusal (too many deletions, or a CA removal);
   - LDAP refusing writes.
3. **Is the entry in LDAP?** Run `ldapsearch -x -b <base> uid=<user>` on the slapd host. If it is
   there, the login node's sssd cache is the likely cause: `sss_cache -u <user>`.
4. **Did the trigger run?** Pending deliveries stay in the slave's queue directory, in order. A
   failed delivery blocks those after it until it succeeds. Read the slave log for the failing
   command. Fix the cause (permissions, NSS, mounts, Slurm) rather than deleting queue files;
   triggers are safe to repeat.

## Cluster status looks stale

- Each cluster card shows when it was collected. A cluster is marked stale after the
  `stale-seconds` of its plugin.
- Check the slave's `current-status` section and its log for rejected reports.
- The browser reads status straight from R2 through presigned links. A "Failed to fetch" there
  usually means the R2 CORS rules do not list the portal origin
  (`deploy/worker/r2-cors.json`).

## Web terminal or files fail

- **"Failed to fetch" in the browser.** The gateway's nginx CORS map must allow the portal
  origin.
- **Gateway shown offline in 运行诊断.** The gateway re-registers with master periodically. Check
  `systemctl status web-access-gateway` and its log, and its client certificate.
- **SSH refused.** The login node's sshd must trust the CA in
  `<trust dir>/current/ssh/trusted-user-ca-keys.pub`. Check with `sshd -T | grep -i trusteduser`.

## Mail

- 邮件服务 → 测试 sends one message through a provider and shows the provider's answer.
- Every delivery is counted per day, provider and purpose (job, transfer, password, tests). A
  pool's counts appear under the member that delivered.
- A rejected recipient is not retried on another provider; other failures move to the next
  member when failover is on.

## Keys and certificates

| What | How |
| --- | --- |
| Session keys | Add a key to `JWT_KEYS`, set `JWT_ACTIVE_KID` to it, deploy; remove the old key after the token lifetime |
| A slave or gateway client certificate | Issue the new certificate, update its fingerprint on the master (`wrangler.toml`, or `.env` for the container master), redeploy the master, then install the certificate on the machine |
| Gateway ticket key | Add the new public key to every gateway's `ticket_public_keys` first, then change `PLUGIN_WEB_ACCESS_TICKET_KEY` and `web-access.ticket-key-id` |
| User certificates | Renewed automatically before expiry by the cron tick; revoked certificates reach sshd through the KRL on the next sync |
| `PKI_MASTER_KEY` | Do not change it. Every sealed CA key and secret depends on it. Keep an offline copy |

## Backups and recovery

- **Database.** SaaS: `npx wrangler@4 d1 export <database> --remote --output backup.sql` is
  read-only, and D1 also offers Time Travel restore for recent points in time. Containers: back up
  `deploy/compose/master/data/master/`, `.env` and `secrets/`.
- **Trust files.** Each generation is an immutable directory. To roll back, run
  `ln -sfn <previous-generation> <trust dir>/current` on the trust share.
- **LDAP.** Back up with `slapcat -n 0` and `slapcat -n 1` before any risky change. The slave
  can rebuild master-managed entries from the snapshot.
- **Master.** SaaS: `npx wrangler@4 rollback` returns to the previous Worker version. Containers:
  check out the previous release and `docker compose up -d --build`. Migrations are additive, so
  an older master runs on a newer schema.

## Scripted access

API calls to the browser hostname must pass Cloudflare Access: send the service token headers
`cf-access-client-id` and `cf-access-client-secret`. Scripts must also send a normal
`User-Agent` header, or Cloudflare's bot protection answers with error 1010.
