# Shared home and trust

Users need a shared `$HOME` directory across the login node and compute nodes, and `sshd` on every host needs access to the SSH trust files (`trusted-user-ca-keys.pub` and `revoked.krl`) published by the slave agent. A single NFSv4 export on `infra` provides both.

**Target hosts:** `infra` (NFSv4 server); `ctl`, compute nodes, and `login` (NFSv4 clients). **Estimated time:** 15 minutes.

| Path on `infra` | Mount Point | Access on `ctl` | Access on Compute & Login Nodes |
| --- | --- | --- | --- |
| `/srv/nfs/home` | `/home` | Read-write, `no_root_squash` | Read-write, `root_squash` (default) |
| `/srv/nfs/home/.slurm-portal-trust` | `/home/.slurm-portal-trust` on `ctl`; `/srv/slurm-portal-trust` elsewhere | Written by the slave (`slurm-portal` user) | Read-only mount with 5-second attribute cache (`actimeo=5`) |

`ctl` is granted `no_root_squash` because the slave's trigger script runs there to create new user home directories as `root`, and the slave daemon writes trust generations into `.slurm-portal-trust`.

## Home layout

When a new user is provisioned, `slurm_trigger.py` creates `/home/<primary-group>/<username>` owned by `<uid>:<gid>` with mode `0700`. In the portal, configure each primary POSIX group with home root `/home/<group>`. If `/home/<group>/shared` exists (or `/home/shared` as a fallback), the trigger automatically creates a `~/SHARED` symlink inside the user's home directory.

## 1. The server

The slave daemon runs on `ctl` as the `slurm-portal` system user, so `.slurm-portal-trust` on the NFS server must be owned by that user's **numeric UID and GID**. Pick an unused system UID on `ctl` (this guide uses `990`; verify with `getent passwd 990`) and create the account on `ctl`:

```sh
useradd --system --uid 990 --user-group --home-dir /var/lib/slurm-portal --shell /usr/sbin/nologin slurm-portal
```

On `infra`, install the NFS server, create the export directories, and assign ownership of `.slurm-portal-trust` to UID/GID `990`:

```sh
apt install -y nfs-kernel-server
mkdir -p /srv/nfs/home/shared /srv/nfs/home/.slurm-portal-trust
chown 990:990 /srv/nfs/home/.slurm-portal-trust
```

Configure `/etc/exports` on `infra` (replace `10.0.0.0/24` with your cluster subnet):

```text title="/etc/exports"
/srv/nfs/home  ctl(rw,fsid=0,sync,no_subtree_check,no_root_squash) 10.0.0.0/24(rw,fsid=0,sync,no_subtree_check)
```

Apply and verify the export table:

```sh
exportfs -ra
exportfs -v
```

Setting `fsid=0` designates `/srv/nfs/home` as the NFSv4 pseudo-root, allowing clients to mount `infra:/` directly.

:::note
Export only `/srv/nfs/home` on `infra`. Exporting both a parent directory and a subdirectory on the same underlying filesystem with different options can cause the Linux NFS server to apply the wrong permission flags to client requests.
:::

## 2. The clients

On `ctl`, every compute node, and `login`, install the NFS client utilities:

```sh
apt install -y nfs-common
```

On `ctl`, mount `infra:/` at `/home` (the slave writes directly to `/home/.slurm-portal-trust`):

```text title="/etc/fstab (ctl)"
infra:/  /home  nfs4  rw,hard,_netdev  0 0
```

On **compute nodes and `login`**, mount `/home` read-write and mount `/.slurm-portal-trust` read-only at `/srv/slurm-portal-trust` with a 5-second attribute cache:

```text title="/etc/fstab (compute nodes, login)"
infra:/                     /home                    nfs4  rw,hard,_netdev  0 0
infra:/.slurm-portal-trust  /srv/slurm-portal-trust  nfs4  ro,hard,actimeo=5,nosharecache,_netdev  0 0
```

Create the mount point on compute and login nodes and mount all NFSv4 filesystems:

```sh
mkdir -p /srv/slurm-portal-trust      # compute nodes and login only
systemctl daemon-reload
mount -a
findmnt -t nfs4
```

:::warning
Mounting over `/home` masks any existing local directories under `/home`. Move any local administrator home directories outside `/home` before mounting.
:::

**Why use a separate mount for `/srv/slurm-portal-trust`?** By default, NFS clients cache directory and file attributes for up to 60 seconds. Mounting the trust directory separately with `ro,actimeo=5,nosharecache` ensures that when a certificate is revoked, `sshd` on every login node observes the updated `revoked.krl` within 5 seconds—while `/home` retains normal NFS caching performance for user workloads.

## Check it works

On `ctl`, verify that `slurm-portal` can write to the trust directory and `root` can create files in `/home` without root squashing:

```sh
sudo -u slurm-portal touch /home/.slurm-portal-trust/probe && echo "trust writable"
touch /home/probe && ls -l /home/probe     # owned by root: no_root_squash works
```

On `login`, verify that both files are visible, `/srv/slurm-portal-trust` is read-only, and the 5-second attribute cache is active:

```sh
ls -l /srv/slurm-portal-trust/probe /home/probe
touch /srv/slurm-portal-trust/x            # "Read-only file system": expected
findmnt -n -o OPTIONS /srv/slurm-portal-trust | grep -o 'acregmax=5'
```

Remove the test probe files from `ctl`:

```sh
rm /home/.slurm-portal-trust/probe /home/probe
```

| Symptom | What to Check |
| --- | --- |
| `access denied by server` | Client IP or reverse DNS does not match `/etc/exports` on `infra`—check `exportfs -v` |
| `Permission denied` writing trust files on `ctl` | `/srv/nfs/home/.slurm-portal-trust` is not owned by UID `990`, or `slurm-portal` on `ctl` has a different UID |
| `Read-only file system` or `nobody` ownership on `ctl` | `infra` did not resolve `ctl` to the IP that connected, falling back to the subnet rule with `root_squash` |
| User files appear owned by `nobody` | SSSD is not resolving LDAP UIDs on that host—see [SSSD & SSH](sssd-ssh.md) |

## Next

Your cluster foundation is ready. Next, deploy the Slurm Portal master control plane:
- [Serverless Master (Cloudflare)](master-saas.md), or
- [Container Master (Docker Compose)](master-container.md).
