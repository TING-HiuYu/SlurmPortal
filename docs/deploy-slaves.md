# Deploy a slave on each cluster

A slave keeps one cluster in step with the master:

- It writes users, groups and memberships into the cluster's OpenLDAP.
- It writes the SSH trust files (trusted CA keys and the revocation list) that sshd reads.
- It runs triggers, such as creating the user's Slurm account.
- Optionally, it reports node and queue status.

The same slave works with either master: [SaaS](deploy-saas-master.md) or
[containers](deploy-container-master.md). Everything is in [deploy/compose/slave](../deploy/compose/slave).

## What you need

- A host on the cluster with Docker Engine and the Compose plugin, which can reach:
  - the cluster's OpenLDAP (the slave writes to it as an administrator);
  - the master's sync address (`https://sync.portal.example.org` for SaaS, or
    `https://portal.example.org:8443` for containers).
- A directory that every login node's sshd can read, for the trust files. On most clusters that
  is a folder on the shared filesystem, for example `/home/.slurm-portal-trust`.
- From the master: this cluster's ID and client certificate (and, for the container master, its
  CA). The container master's `add-cluster.sh` puts them all in one bundle.

## 1. Set up

```sh
cd deploy/compose/slave
./init.sh
```

`init.sh` creates:

- `.env`, so the container runs as your user;
- the `data/` folders;
- a `slave.yml` from the example.

Then add the secrets:

```sh
cp /path/to/bundle/client.crt /path/to/bundle/client.key secrets/
cp /path/to/bundle/master-ca.crt secrets/            # container master only
printf '%s' 'the LDAP admin password' > secrets/ldap-password
chmod 600 secrets/*
```

If the trust directory is not `./data/trust`, set `TRUST_DIR` in `.env` (an absolute host path),
and run `./init.sh` again so it is created.

## 2. Configure slave.yml

| Key | SaaS master | Container master |
| --- | --- | --- |
| `master-url` | `https://sync.portal.example.org` | `https://portal.example.org:8443` |
| `dns-name` | `_sync.portal.example.org` | `""` (the slave asks the master) |
| `tls.server-name` | `sync.portal.example.org` | `portal.example.org` |
| `tls.ca-file` | not set | `/etc/slurm-portal/secrets/master-ca.crt` |
| `cluster-id` | the cluster's ID on the master | the same |

Then set the cluster side:

| Key | Meaning |
| --- | --- |
| `ldap.url`, `ldap.bind-dn`, `ldap.password-file` | How to reach and write the cluster's OpenLDAP. The slave uses the host network, so `ldap://127.0.0.1:389` reaches a local slapd. Other addresses need `ldaps://` or `start-tls: true` |
| `local-base-dn` | This directory's suffix |
| `source-base-dn` | The master's suffix (`LDAP_BASE_DN`), only if it differs. DNs are then rewritten |
| `limits.max-user-deletions` | The slave refuses a sync that would delete more users than this (default 5) |
| `limits.allow-ca-removal` | Leave `false`: a sync that would stop trusting an SSH CA is refused, so a mistake cannot lock everyone out |

## 3. Rehearse, then start

Before the first real sync, see exactly what the slave would change. Export the directory and run
the slave's plan mode against it:

```sh
# ./data/state is the container's /var/lib/slurm-portal
slapcat -n 1 > data/state/cluster.ldif           # on the slapd host, then copy it here
cp /path/to/snapshot.json data/state/
docker compose run --rm --entrypoint /usr/local/bin/slurm-portal-slave slave -plan \
  -snapshot /var/lib/slurm-portal/snapshot.json -ldif /var/lib/slurm-portal/cluster.ldif \
  -trust-dir /trust -local-base-dn dc=example,dc=org -admin-dn cn=admin,dc=example,dc=org
```

Export the snapshot from a copy of the master's database with
`go run ./cmd/migration-check -db file:copy.db -base-dn <LDAP_BASE_DN> -snapshot-out snapshot.json`.
For the SaaS master, take the copy with `npx wrangler@4 d1 export`.
Go ahead when the report says `"safe": true`. A first sync keeps local-only entries and existing
passwords.

```sh
docker compose up -d
docker compose logs -f slave               # "sync check failed" lines say what is wrong
curl -s 127.0.0.1:8081/readyz              # {"ready":true,...} after the first successful sync
```

Changes made on the master reach the cluster within about a minute (`check-interval`).

### Try it without a cluster

The compose file has a throwaway OpenLDAP for trying things out:

```sh
openssl rand -base64 24 > secrets/ldap-password
# in slave.yml: url ldap://127.0.0.1:1389, bind-dn cn=admin,dc=example,dc=org, local-base-dn dc=example,dc=org
docker compose --profile demo up -d
```

## 4. Point sshd at the trust files

On every login node, add to `sshd_config` and reload sshd:

```text
TrustedUserCAKeys /home/.slurm-portal-trust/current/ssh/trusted-user-ca-keys.pub
RevokedKeys       /home/.slurm-portal-trust/current/ssh/revoked.krl
```

`current` always points at a complete, immutable generation. If a shared filesystem caches
attributes for a long time, revocations can be delayed. [`deploy/storage/install-trust-nfs.sh`](../deploy/storage/install-trust-nfs.sh)
mounts just the trust folder with a short cache.

On compute nodes, [`deploy/storage/install-compute-ssh-trust.sh`](../deploy/storage/install-compute-ssh-trust.sh)
sets up the same trust and allows only root SSH.

## 5. Optional parts

| Feature | How |
| --- | --- |
| Slurm accounts and storage on user changes | Triggers: [deploy/triggers](../deploy/triggers/README.md). A container slave uses the host bridge |
| Live node and queue status | Set `current-status.enabled: true`. The slave runs `scontrol` and `squeue`, so build an image that adds your cluster's Slurm client tools and its `slurm.conf` and munge socket (start from [slave.Dockerfile](../deploy/docker/slave.Dockerfile)), or run the slave on the host with [slurm-portal-slave.service](../deploy/slave/slurm-portal-slave.service) |
| Job-end e-mail and GPU usage | The lifecycle hook and observer on the Slurm controller: [deploy/lifecycle](../deploy/lifecycle/README.md), then set `lifecycle-file` in `slave.yml` |
| Browser terminal, files and transfers | A gateway on each login node: [deploy/gateway](../deploy/gateway/README.md) |

When the slave cannot resolve job owners' user names (no LDAP NSS inside the container), it names
them from the accounts the master sent it.

## Running without containers

Build the binary (`CGO_ENABLED=0 go build -o slurm-portal-slave ./cmd/slave`), install it as
`/usr/local/sbin/slurm-portal-slave` with `/etc/slurm-portal/slave.yml`, and use
[slurm-portal-slave.service](../deploy/slave/slurm-portal-slave.service). The configuration is
the same; adjust paths that point into `/etc/slurm-portal/secrets` and `/trust`.

## Updating

```sh
git pull
docker compose up -d --build
```

The slave restarts from its saved state and writes nothing to LDAP unless the snapshot changed.
