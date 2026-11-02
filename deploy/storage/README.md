# Storage and node trust tooling

| File | Purpose |
| --- | --- |
| `reconcile-home-acl.py`, `test_reconcile_home_acl.py` | Reconciles home directory ACLs (NFSv4 or POSIX) for a single user or all users; invoked automatically by `slurm_trigger.py` |
| `install-trust-nfs.sh` | Mounts the shared SSH trust directory over NFS with a 5-second attribute cache so certificate revocations take effect promptly |
| `install-compute-ssh-trust.sh` | Configures a compute node's `sshd` and `sssd` to trust the portal SSH CA while restricting direct SSH logins to `root` |

## HOME ACLs

`reconcile-home-acl.py` enforces a consistent permission layout on each user's home directory:

- the user (owner) has full read, write, and execute permissions;
- the user's supplementary POSIX groups (from LDAP `memberUid`) have read and directory traversal access;
- all other users are denied access at the home directory root;
- default/inheritance ACLs ensure newly created files and subdirectories inherit the same rules.

```sh
python3 reconcile-home-acl.py --home-root /home --user alice --no-backfill   # provision or update a single user
python3 reconcile-home-acl.py --home-root /home --no-backfill                # reconcile every user's home root
```

For safety, the script refuses to touch any path outside the mounted `--home-root` export. Before writing, it verifies that the target directory's `findmnt` source, mount point, and filesystem type match `--home-root`. When running a recursive backfill, it uses `find -xdev` and checks the filesystem type so nested submounts inside a home directory are never traversed.

- **NFSv4 exports:** Run the script on a host with LDAP NSS resolution (`sssd`) and install `nfs4-acl-tools`. To work with `root_squash` exports, the script runs `nfs4_setfacl` as the home directory owner and uses numeric UIDs/GIDs (`sec=sys`). UIDs and GIDs must be identical across the NFS server and all client nodes.
- **POSIX ACL filesystems** (local ext4/XFS, GlusterFS, etc.): Install the `acl` package (`setfacl`).

Passing `--no-backfill` updates only the top-level home directory and its default inheritance ACLs, recording existing subdirectories as pending. A full recursive backfill requires every file in the tree to be owned by the user and aborts before making changes if it detects mixed ownership. When testing permissions on network filesystems, verify access by reading files as a group member and as a non-member rather than relying solely on immediate ACL listings, which can reflect stale client attribute caches.

## Trust files on login and compute nodes

The slave agent publishes the SSH CA public keys and revocation list into a shared directory read by `sshd` on every login and compute node:

```text
TrustedUserCAKeys <trust dir>/current/ssh/trusted-user-ca-keys.pub
RevokedKeys       <trust dir>/current/ssh/revoked.krl
```

- **`install-trust-nfs.sh SERVER:/export/path/of/the/trust/dir`** mounts the exported trust directory over NFS with a 5-second attribute cache (`actimeo=5`), bind-mounts it read-only at `/etc/go-ldap-admin-trust`, and records both entries in `/etc/fstab` (after backing up the original file). Use this helper when your main `/home` NFS mount uses long attribute caching that would delay KRL revocation updates.
- **`install-compute-ssh-trust.sh <trust dir>`** locks down SSH access on compute nodes:
  - bind-mounts the trust directory read-only and configures `sshd` to use `TrustedUserCAKeys` and `RevokedKeys`;
  - configures `sssd` to use LDAP for POSIX identity lookups only (`auth_provider = none`, `access_provider = deny`);
  - restricts direct SSH login to `root` (`AllowUsers root`), preventing regular users from bypassing Slurm via SSH certificates even when `UsePAM no` is set.

  Original `/etc/fstab`, `sssd`, and `sshd` configuration files are backed up under `/var/backups/go-ldap-admin/`.
