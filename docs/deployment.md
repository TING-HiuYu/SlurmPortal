# Deployment

A Slurm Portal installation is one **master** and one **slave** per cluster. Optional extras: a
**gateway** per login node, for the browser terminal and files, and the **lifecycle hook**, for
job e-mail and GPU usage. The master can run in one of two ways:

| | SaaS master | Container master |
| --- | --- | --- |
| Runs on | Cloudflare: Worker, D1, R2, Access | Your host: Docker Compose with the master (SQLite), an S3 store (RustFS) and Caddy |
| You look after | Cloudflare settings | A host, its backups and its updates |
| Sign-in protection | Cloudflare Access in front, plus the portal's own sign-in | The portal's own sign-in (password, GitHub) |
| Slaves learn of changes | DNS TXT record, so a quiet cluster makes no requests | A small HTTPS request to the master every minute |
| Guide | [deploy-saas-master.md](deploy-saas-master.md) | [deploy-container-master.md](deploy-container-master.md) |

Slaves are the same for both: [deploy-slaves.md](deploy-slaves.md).

## Order

1. Deploy the master and sign in as the first administrator.
2. Register each cluster on the master:
   - SaaS: a Cloudflare client certificate, and an entry in `CLUSTERS`;
   - containers: `./add-cluster.sh`.
3. On each cluster, deploy the slave and point sshd at its trust files.
4. Optionally add a gateway per login node, and the lifecycle hook on the Slurm controller.

## Gateway (optional, per login node)

The gateway carries the browser terminal, the file browser and transfers between clusters, running
everything as the signed-in user over SSH. See [deploy/gateway](../deploy/gateway/README.md):

- a systemd install with nginx in front;
- or a single container ([deploy/gateway/container](../deploy/gateway/container/README.md)).

The gateway needs:

- a client certificate the master allows, as for a slave. Add its fingerprint to `web-access.gateways` in
  `PLUGINS_CONFIG`, and to `SYNC_CLIENT_FINGERPRINTS`;
- the public half of `PLUGIN_WEB_ACCESS_TICKET_KEY`, under the same key ID;
- the slave's `trusted-user-ca-keys.pub`.

## Lifecycle hook (optional, on the Slurm controller)

The hook reports job start and end without ever delaying scheduling. The observer gives the slave
read-only job details from slurmrestd. See [deploy/lifecycle](../deploy/lifecycle/README.md). Then
set `lifecycle-file` in the slave's configuration and turn on 作业通知 (job e-mail) in the portal.

## Machine identity

Slaves and gateways prove who they are with client certificates; the master knows each one by its
SHA-256 fingerprint:

- **SaaS master.** Certificates come from Cloudflare's client-certificate CA.
- **Container master.** `deploy/certs/issue.sh` runs a private CA, through `init.sh` and
  `add-cluster.sh`.

When a certificate is renewed its fingerprint changes. Update the master's lists in the same change.

## Moving from go-ldap-admin v1

A master started on a go-ldap-admin v1 database:

- applies its migrations;
- imports the v1 web-access settings, notification settings and GPU job history once;
- reads users, groups, roles, CAs and certificates from the v1 tables directly.

`cmd/migration-check` rehearses this on a copy and proves `PKI_MASTER_KEY` opens every CA. Run
`slave -plan` against each cluster's directory before its first sync.
