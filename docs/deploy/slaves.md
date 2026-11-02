# The slave

The **slave** agent (`cmd/slave`) keeps a Slurm cluster synchronized with the master control plane:

- Reconciles users, POSIX groups, organizational units, memberships, and certificates into the cluster's OpenLDAP directory;
- Publishes immutable SSH trust generations (`trusted-user-ca-keys.pub` and `revoked.krl`) consumed by `sshd`;
- Executes local triggers when accounts change (such as creating Slurm accounting associations and NFSv4 home directories);
- Collects and uploads live node, partition, and queue telemetry.

The same slave binary works with both the serverless and container master. Below, we install it as a native systemd service on `ctl` so it can invoke `scontrol`, `squeue`, and `sacctmgr` directly. ([Running the slave in a container](#in-a-container) is covered at the end of this page.)

**Target host:** `ctl`. **Estimated time:** 20 minutes.

## What you need

- The cluster foundation configured as described in [OpenLDAP](openldap.md), [SSSD & SSH](sssd-ssh.md), and [Shared Home & Trust](shared-home.md)—including the `slurm-portal` system account (UID `990`).
- This cluster's mTLS credential bundle from the master (`client.crt`, `client.key`, and `master-ca.crt` for a container master):
  - **Container master:** generated in `secrets/clusters/cluster1/` by `./add-cluster.sh cluster1 "Cluster 1"`.
  - **Serverless master:** the Cloudflare mTLS client certificate and private key issued for `cluster1`.
- Outbound HTTPS connectivity from `ctl` to the master's sync endpoint (`https://portal.example.org:8443` for container master, or `https://sync.portal.example.org` for serverless master).

## 1. Build the programs

Compile the static Go binaries on `ctl` (Ubuntu's `golang-go` automatically downloads the exact Go toolchain version specified in `go.mod`):

```sh
apt install -y golang-go git
git clone https://github.com/TING-HiuYu/SlurmPortal /root/SlurmPortal
cd /root/SlurmPortal
mkdir -p bin
for c in slave host-bridge slurm-lifecycle-hook slurm-notify-observer gateway; do
  CGO_ENABLED=0 go build -trimpath -o bin/$c ./cmd/$c
done
install -m 0755 bin/slave /usr/local/sbin/slurm-portal-slave
```

The remaining binaries in `bin/` are used in [Job Notifications & GPU Monitoring](optional.md) and [Web Access Gateway](gateway.md). Because all binaries are compiled with `CGO_ENABLED=0`, you can also build them on an external workstation and copy them to air-gapped cluster hosts.

## 2. Secrets and configuration

Copy the cluster credential bundle to `/root/cluster1` on `ctl` and retrieve the OpenLDAP admin password from `infra`:

```sh
install -d -m 0750 -o root -g slurm-portal /etc/slurm-portal
install -m 0640 -o root -g slurm-portal /root/cluster1/client.crt /root/cluster1/master-ca.crt /etc/slurm-portal/
install -m 0600 -o slurm-portal -g slurm-portal /root/cluster1/client.key /etc/slurm-portal/
scp root@infra:/root/ldap-admin-password /etc/slurm-portal/ldap-password
chown slurm-portal:slurm-portal /etc/slurm-portal/ldap-password
chmod 0600 /etc/slurm-portal/ldap-password
```

The slave verifies at startup that secret files are not group- or world-readable. Next, create `/etc/slurm-portal/slave.yml`:

```yaml title="/etc/slurm-portal/slave.yml"
# The master (container master shown; see the table below for serverless)
master-url: https://portal.example.org:8443
dns-name: ""
check-interval: 60s
cluster-id: cluster1
tls:
  cert-file: /etc/slurm-portal/client.crt
  key-file: /etc/slurm-portal/client.key
  ca-file: /etc/slurm-portal/master-ca.crt
  server-name: portal.example.org

# This cluster's OpenLDAP
ldap:
  url: ldap://infra.example.org
  start-tls: true
  server-name: infra.example.org     # the name in the LDAP server's certificate
  bind-dn: cn=admin,dc=example,dc=org
  password-file: /etc/slurm-portal/ldap-password
local-base-dn: dc=example,dc=org
local-group-dn: ou=groups,dc=example,dc=org

# What the slave writes
state-dir: /var/lib/slurm-portal
health-listen: 127.0.0.1:8081
trust-dirs:
  - /home/.slurm-portal-trust
trigger-file: /etc/slurm-portal/triggers.yaml
limits:
  max-user-deletions: 5
  allow-ca-removal: false

# Live node and queue status, read with scontrol and squeue
current-status:
  enabled: true
```

```sh
chown slurm-portal:slurm-portal /etc/slurm-portal/slave.yml
chmod 0600 /etc/slurm-portal/slave.yml
```

| `slave.yml` Key | Serverless Master (Cloudflare) | Container Master (Docker Compose) |
| --- | --- | --- |
| `master-url` | `https://sync.portal.example.org` | `https://portal.example.org:8443` |
| `dns-name` | `_sync.portal.example.org` | `""` (polls `/sync/v2/snapshot-hash` over mTLS) |
| `tls.server-name` | `sync.portal.example.org` | `portal.example.org` |
| `tls.ca-file` | Omit (uses system CA bundle) | `/etc/slurm-portal/master-ca.crt` |

See the [Cluster Agents Reference](../dev/agents.md#configuration-reference) for all supported keys. Setting `local-group-dn: ou=groups,dc=example,dc=org` places groups under `ou=groups`, matching the search base configured in `sssd.conf`.

## 3. The trigger: Slurm accounts and home directories

Whenever a user account or group membership changes, `slurm_trigger.py` creates or updates the user's Slurm accounting association (`sacctmgr`) and initializes their NFSv4 home directory (`/home/<group>/<user>`). When a user is deleted in the portal, `deletePolicy: "disable"` sets `MaxSubmitJobs=0` in Slurm while preserving their home directory on disk. The slave invokes the script via a tightly scoped `sudo` rule:

```sh
install -d -m 0755 /usr/local/libexec/go-ldap-admin
install -m 0755 /root/SlurmPortal/deploy/triggers/slurm_trigger.py /usr/local/libexec/go-ldap-admin/
install -d -m 0755 /etc/go-ldap-admin
cat > /etc/go-ldap-admin/slurm-trigger.json <<'EOF'
{
  "cluster": "cluster1",
  "deletePolicy": "disable",
  "homeRoot": "/home",
  "sharedRoot": "/home/shared",
  "storageCommands": [],
  "identityPrefix": ["/usr/bin/getent"],
  "sacctmgrPrefix": ["/usr/bin/sacctmgr"]
}
EOF
chmod 0600 /etc/go-ldap-admin/slurm-trigger.json

cat > /etc/sudoers.d/slurm-portal <<'EOF'
slurm-portal ALL=(root) NOPASSWD: /usr/local/libexec/go-ldap-admin/slurm_trigger.py --config /etc/go-ldap-admin/slurm-trigger.json *
EOF
chmod 0440 /etc/sudoers.d/slurm-portal
visudo -cf /etc/sudoers.d/slurm-portal

cat > /etc/slurm-portal/triggers.yaml <<'EOF'
version: 1
triggers:
  - id: slurm-account
    enabled: true
    events: [user.created, user.updated, user.deleted, membership.added, membership.removed]
    command:
      - /usr/bin/sudo
      - -n
      - /usr/local/libexec/go-ldap-admin/slurm_trigger.py
      - --config
      - /etc/go-ldap-admin/slurm-trigger.json
      - "%DATA%"
    timeoutSeconds: 120
EOF
chown slurm-portal:slurm-portal /etc/slurm-portal/triggers.yaml
chmod 0600 /etc/slurm-portal/triggers.yaml
```

For safety, `slurm_trigger.py` verifies that `/home` is mounted as an `nfs4` filesystem before creating home directories and ignores any UID below `10000`.

## 4. The service

Install the systemd service unit and add a drop-in override so the slave can invoke `sudo` and write to `/home`:

```sh
cp /root/SlurmPortal/deploy/slave/slurm-portal-slave.service /etc/systemd/system/
mkdir -p /etc/systemd/system/slurm-portal-slave.service.d
cat > /etc/systemd/system/slurm-portal-slave.service.d/site.conf <<'EOF'
[Service]
# The trigger runs through sudo, and writes home directories; the trust folder is in /home.
NoNewPrivileges=false
ProtectHome=false
ReadWritePaths=
ReadWritePaths=/var/lib/slurm-portal /home
EOF
systemctl daemon-reload
```

## 5. Rehearse, then start

Before starting the slave for the first time, run `-plan` offline to inspect the exact LDAP operations the initial sync will perform. Export a copy of the master database—on a container master host:

```sh
sqlite3 data/master/portal.db ".backup /tmp/portal-copy.db"
```

*(For a serverless master, run `npx wrangler@4 d1 export <database> --remote --output dump.sql` and load it with `sqlite3 /tmp/portal-copy.db < dump.sql`.)* Copy `/tmp/portal-copy.db` to `ctl` as `/root/portal-copy.db`, and export the current OpenLDAP directory on `infra` to `/root/cluster.ldif`:

```sh
# on infra
slapcat -n 1 > /root/cluster.ldif
```

Copy `/root/cluster.ldif` to `ctl` and run `-plan`:

```sh
slurm-portal-slave -plan -db file:/root/portal-copy.db -source-base-dn dc=example,dc=org \
  -ldif /root/cluster.ldif -local-base-dn dc=example,dc=org -local-group-dn ou=groups,dc=example,dc=org \
  -admin-dn cn=admin,dc=example,dc=org
```

Review the JSON plan output:

- **On a fresh OpenLDAP directory:** Expect `add-user`, `add-group`, and `add-member` operations for the accounts in the portal, with `"guard": "pass"`.
- **When adopting an existing directory that already contains the same accounts:** Expect zero `loginOps` and `"safe": true`. If any unexpected deletions or modifications appear, resolve the discrepancy before starting the daemon.

Delete `/root/portal-copy.db` and `/root/cluster.ldif` once verified, then enable and start the slave service:

```sh
systemctl enable --now slurm-portal-slave
journalctl -u slurm-portal-slave -f        # "sync check failed" lines say what is wrong; Ctrl-C to stop
```

## Point sshd at the trust files

Once the first sync succeeds, `/srv/slurm-portal-trust/current/ssh/` on `login` contains `trusted-user-ca-keys.pub` and `revoked.krl`. Configure `sshd` on `login` to trust certificates signed by the portal CA and enforce the revocation list:

```sh
ls /srv/slurm-portal-trust/current/ssh/
cat > /etc/ssh/sshd_config.d/50-slurm-portal.conf <<'EOF'
TrustedUserCAKeys /srv/slurm-portal-trust/current/ssh/trusted-user-ca-keys.pub
RevokedKeys /srv/slurm-portal-trust/current/ssh/revoked.krl
EOF
sshd -t && systemctl reload ssh
```

Because `current` is an atomic symlink to an immutable generation directory, `sshd` never sees a partially written CA file or KRL.

:::warning
If `RevokedKeys` points to a file path that does not exist, OpenSSH rejects **all** public-key logins—including root and administrator keys. Always confirm `ls /srv/slurm-portal-trust/current/ssh/revoked.krl` succeeds and keep a root shell open before reloading `sshd`.
:::

## Check it works

On `ctl`, confirm that `/readyz` reports `"ready": true` and that a portal user (such as `alice` in primary group `research`) resolves in NSS, has a Slurm association, and has a provisioned home directory:

```sh
curl -s 127.0.0.1:8081/readyz; echo
getent passwd alice
sacctmgr -nP show assoc where user=alice format=cluster,account,user
ls -ld /home/research/alice
```

```text
{"ready":true,"hash":"5630…","dirty":false,"lastOk":"…"}
alice:*:10000:10000:alice:/home/research/alice:/bin/bash
cluster1|alice|alice
drwx------ 2 alice research 4096 … /home/research/alice
```

| Symptom | What to Check |
| --- | --- |
| `/readyz` returns `503` (`"ready": false`) | Inspect `journalctl -u slurm-portal-slave` for the exact failing step |
| `x509: certificate signed by unknown authority` | Verify `tls.ca-file` (for container master) or the system CA bundle (for LDAP) |
| `remote error: tls: unknown certificate` | The slave's client certificate fingerprint is missing from `CLUSTERS` / `SYNC_CLIENT_FINGERPRINTS` on the master |
| LDAP `Invalid credentials` | Verify `/etc/slurm-portal/ldap-password` (no trailing newline) and `ldap.bind-dn` |
| `trigger … failed: exit status 1` once right after user creation | Expected if SSSD had not cached the new LDAP entry yet; the slave automatically retries on the next cycle |
| Trigger fails repeatedly | Run the logged command manually as root to inspect the error; check NSS (`getent`), NFSv4 mount options, `sudoers`, and `slurmdbd` |

## Updating

To upgrade the slave binary on `ctl`:

```sh
cd /root/SlurmPortal && git pull
CGO_ENABLED=0 go build -trimpath -o bin/slave ./cmd/slave
install -m 0755 bin/slave /usr/local/sbin/slurm-portal-slave
systemctl restart slurm-portal-slave
```

The slave resumes from `/var/lib/slurm-portal/applied.json` without rewriting unchanged LDAP entries.

## In a container

You can also run the slave in Docker Compose using [`deploy/compose/slave`](https://github.com/TING-HiuYu/SlurmPortal/tree/main/deploy/compose/slave):

```sh
cd deploy/compose/slave
./init.sh                       # .env, data folders, slave.yml from the example
cp /path/to/bundle/client.crt /path/to/bundle/client.key secrets/
cp /path/to/bundle/master-ca.crt secrets/            # container master only
printf '%s' 'the LDAP admin password' > secrets/ldap-password
chmod 600 secrets/*
docker compose up -d
```

Set `TRUST_DIR` in `.env` to your shared trust directory. Note two differences when running the slave inside a container:

- **Account triggers:** A container cannot run `sacctmgr` on the host directly. Configure [`cmd/host-bridge`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/triggers/README.md#slave-in-a-container) so the containerized slave pipes trigger events over SSH stdin to `slurm_trigger.py` on the controller host.
- **Live cluster telemetry:** Collecting status requires `scontrol`, `squeue`, `/etc/slurm/slurm.conf`, and the Munge socket inside the container image (extend [`slave.Dockerfile`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/docker/slave.Dockerfile) with your Slurm client packages).

For local testing without a Slurm cluster, `docker compose --profile demo up -d` starts the slave alongside a disposable OpenLDAP container.

## Next

Proceed to the optional cluster integrations:
- [Job Notifications & GPU Monitoring](optional.md)
- [Web Access Gateway](gateway.md)
- Or skip straight to [End-to-End Verification](verify.md).
