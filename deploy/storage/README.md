# Storage and node trust tooling

| File | Purpose |
| --- | --- |
| `reconcile-home-acl.py`, `test_reconcile_home_acl.py` | Sets HOME ACLs (NFSv4 or POSIX) for one user or for all; called by the Slurm trigger |
| `install-trust-nfs.sh` | Mounts only the trust directory over NFS with a 5-second attribute cache, so revocations show up quickly |
| `install-compute-ssh-trust.sh` | Points a compute node's sshd at the trust files and allows only root SSH |

## HOME ACLs

`reconcile-home-acl.py` gives each user's HOME this shape:

- the owner has full control;
- the user's supplementary POSIX groups (LDAP `memberUid`) get read and traverse;
- everyone else is denied at the HOME root;
- default ACLs make new files follow the same rules.

```sh
python3 reconcile-home-acl.py --home-root /home --user alice --no-backfill   # one new user
python3 reconcile-home-acl.py --home-root /home --no-backfill                # every user's HOME root
```

It refuses paths outside the exact mounted HOME export. It compares the target's `findmnt`
source, mount point and filesystem with `--home-root` before writing. A recursive backfill uses
`find -xdev` and a filesystem filter, so a mount nested under HOME is never touched.

- **NFSv4 exports.** Run it on an LDAP NSS client and install `nfs4-acl-tools`. The script uses
  `nfs4_setfacl` as the HOME owner to cope with `root_squash`, and numeric group IDs (`sec=sys`).
  UIDs and GIDs must match on all clients and the server.
- **POSIX ACL filesystems** (local, GlusterFS and others): install `acl`, which provides
  `setfacl`.

`--no-backfill` sets the HOME root and inheritance only. Existing files are recorded as pending,
because a full backfill must own every child entry, and a mixed-ownership tree is rejected before
any recursive write. Check access by actually reading as a member and as a non-member: attribute
caches on network filesystems can make an immediate ACL listing misleading.

## Trust files on login and compute nodes

The slave writes the trust files into one shared directory. Each login and compute node's sshd reads:

```text
TrustedUserCAKeys <trust dir>/current/ssh/trusted-user-ca-keys.pub
RevokedKeys       <trust dir>/current/ssh/revoked.krl
```

- **`install-trust-nfs.sh SERVER:/export/path/of/the/trust/dir`** mounts only that directory with
  a 5-second NFS attribute cache. It binds it read-only at `/etc/go-ldap-admin-trust` and records
  both mounts in `/etc/fstab`, after a backup. Use it when the normal HOME mount caches attributes
  for a long time, which would delay revocations.
- **`install-compute-ssh-trust.sh <trust dir>`** is for compute nodes:
  - binds the trust directory read-only and points sshd at it;
  - keeps LDAP for identity lookups only (`auth_provider = none`, `access_provider = deny`);
  - allows only root SSH, even where `UsePAM no` would let certificate logins past sssd's access
    rules.

  It backs up fstab, sssd and sshd settings under `/var/backups/go-ldap-admin/`.
