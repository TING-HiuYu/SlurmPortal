# sssd and SSH

Every host that schedules jobs, runs tasks, or accepts user logins must resolve POSIX accounts from OpenLDAP. **SSSD** (System Security Services Daemon) queries OpenLDAP over StartTLS and caches identity lookups locally so `id alice`, NFSv4 file ownership, and Slurm all see consistent UIDs and GIDs across the cluster.

**Target hosts:** `ctl`, all compute nodes (`node1`), and `login`. **Estimated time:** 10 minutes.

## 1. Install

On `ctl`, every compute node, and `login`:

```sh
apt install -y sssd-ldap sssd-tools libnss-sss libpam-sss ldap-utils libldap-common
```

Installing `libnss-sss` and `libpam-sss` automatically registers `sss` in `/etc/nsswitch.conf` for `passwd`, `group`, and `shadow`.

## 2. Configure

Create `/etc/sssd/sssd.conf` on `ctl`, every compute node, and `login`:

```ini title="/etc/sssd/sssd.conf"
[sssd]
services = nss, pam
domains = example.org

[domain/example.org]
id_provider = ldap
auth_provider = ldap
ldap_uri = ldap://infra.example.org
ldap_search_base = dc=example,dc=org
ldap_user_search_base = ou=people,dc=example,dc=org
ldap_group_search_base = ou=groups,dc=example,dc=org
# The portal writes POSIX groups with memberUid.
ldap_schema = rfc2307
ldap_id_use_start_tls = true
ldap_tls_reqcert = demand
ldap_tls_cacert = /etc/ssl/certs/ca-certificates.crt
# New and changed accounts appear within a minute.
entry_cache_timeout = 60
cache_credentials = false
```

Lock down file permissions (`sssd` refuses to start if `sssd.conf` is group- or world-readable) and restart the service:

```sh
chmod 600 /etc/sssd/sssd.conf
systemctl restart sssd
```

| Parameter | Purpose |
| --- | --- |
| `ldap_schema = rfc2307` | Matches the `posixGroup` (`memberUid`) schema written by the Slurm Portal slave |
| `ldap_id_use_start_tls`, `ldap_tls_reqcert`, `ldap_tls_cacert` | Enforces StartTLS encryption and validates the LDAP server certificate against the system CA bundle configured in [OpenLDAP](openldap.md#5-trust-the-ca-on-every-host) |
| `entry_cache_timeout = 60` | Limits positive cache TTL to 60 seconds so newly created accounts or group changes take effect within ~2 minutes end-to-end |
| `cache_credentials = false` | Ensures password changes or account locks take effect immediately (set to `true` on login nodes if you want cached offline password logins when `infra` is unreachable) |

## 3. Compute nodes: no user logins

Regular users should submit jobs through Slurm rather than SSHing directly into compute nodes. On **every compute node** (`node1`), disable PAM LDAP authentication and restrict direct SSH logins to `root`:

```sh
sed -i 's/^auth_provider = ldap/auth_provider = none/' /etc/sssd/sssd.conf
cat > /etc/ssh/sshd_config.d/50-slurm-portal-compute.conf <<'EOF'
AllowUsers root
PasswordAuthentication no
KbdInteractiveAuthentication no
EOF
systemctl restart sssd
systemctl reload ssh
```

With `id_provider = ldap` still active, NSS continues resolving UIDs and GIDs so Slurm jobs run as the user, while direct SSH access is blocked. (If your site allows users to SSH into compute nodes where they have a running job, configure Slurm's `pam_slurm_adopt` module instead of `AllowUsers root`.)

## Check it works

Verify that SSSD is online and passes configuration validation:

```sh
sssctl domain-status example.org         # "Online status: Online"
getent passwd root                       # still resolves locally
sssctl config-check                      # no issues
```

Once the slave agent syncs your first portal users later in the guide, `getent passwd <username>` and `id <username>` will resolve them across every host with UIDs starting at `10000`.

| Symptom | What to Check |
| --- | --- |
| `Online status: Offline` | Run `ldapwhoami -x -ZZ -H ldap://infra.example.org` from this host and confirm the LDAP CA certificate is installed |
| `sssd` fails to start | Confirm `chmod 600 /etc/sssd/sssd.conf` and check `journalctl -u sssd` |
| Newly synced user does not resolve yet | Wait up to 60 seconds for `entry_cache_timeout`, or flush the cache immediately with `sss_cache -E` |

## SSH certificates

Users log in to `login` using short-lived OpenSSH certificates signed by Slurm Portal's SSH CA. `sshd` validates those certificates using two files published by the slave agent: `trusted-user-ca-keys.pub` and `revoked.krl`. You will configure `sshd` to reference those files in [Cluster Slave Agent](slaves.md#point-sshd-at-the-trust-files) right after the slave completes its first sync (configuring `RevokedKeys` before `revoked.krl` exists would cause `sshd` to reject all public keys).

## Next

Proceed to [Shared Home & Trust](shared-home.md).
