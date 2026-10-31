#!/usr/bin/env python3
"""Reconcile HOME ACLs only on a verified GlusterFS or NFSv4 export."""
import argparse
import grp
import json
import os
import pwd
import re
import shlex
import shutil
import subprocess
import sys
from pathlib import Path

NAME = re.compile(r"^[a-zA-Z0-9][a-zA-Z0-9_.-]*$")

def run(*argv):
    return subprocess.run(argv, check=True, text=True, capture_output=True).stdout.strip()


def acl_backend(path, home_root):
    """Reject local filesystems and paths outside the HOME export."""
    root = home_root.resolve(strict=True)
    target = path.resolve(strict=True)
    if home_root.is_symlink() or path.is_symlink() or not target.is_relative_to(root):
        raise RuntimeError(f"unsafe HOME ACL path: {path}")

    def mount(value):
        pairs = run("findmnt", "--noheadings", "--pairs", "-o",
                    "TARGET,SOURCE,FSTYPE", "--target", str(value))
        return dict(item.split("=", 1) for item in shlex.split(pairs))

    root_mount, target_mount = mount(root), mount(target)
    if root_mount.get("TARGET") != str(root) or target_mount != root_mount:
        raise RuntimeError(f"HOME path is not on the same mounted export: {path}")
    if root_mount["FSTYPE"] == "fuse.glusterfs":
        backend, tools = "gluster", ("setfacl",)
    elif root_mount["FSTYPE"] == "nfs4" and ":" in root_mount["SOURCE"]:
        backend, tools = "nfs4", ("nfs4_getfacl", "nfs4_setfacl", "runuser")
    else:
        raise RuntimeError(f"HOME ACL requires GlusterFS or NFSv4, got {root_mount['FSTYPE']}")
    for tool in tools:
        if not shutil.which(tool):
            raise RuntimeError(f"HOME ACL on {backend} requires {tool}")
    return backend


def nfs4_as_owner(uid, *args):
    username = pwd.getpwuid(uid).pw_name
    # find must start from a directory the mapped NFS user can traverse;
    # the root caller's working directory may be /root.
    return subprocess.run(("runuser", "-u", username, "--", *args),
                          check=True, text=True, capture_output=True, cwd="/").stdout.strip()

def reconcile_home_acl(state_root, uid, home, supplementary_gids, home_root, backfill_existing=True):
    """Grant explicit LDAP supplementary groups read/traverse; deny everyone else."""
    backend = acl_backend(home, home_root)
    state_path = state_root / f"home-acl-{uid}.json"
    gids = sorted(set(supplementary_gids))
    desired = {"home": str(home), "group_gids": gids, "backend": backend}
    if state_path.exists():
        previous = json.loads(state_path.read_text())
        if previous["home"] != str(home):
            raise ValueError(f"HOME path changed for {home}; explicit data migration required")
        old_gids = previous.get("group_gids", [previous["subgroup_gid"]] if "subgroup_gid" in previous else [])
        if previous.get("backend", "gluster") != backend:
            old_gids = []
    else:
        old_gids = []
        previous = {}
    if backend == "gluster":
        # The root is the access boundary, including when an owner chmods a
        # file below it. Reassert it on every reconciliation.
        run("setfacl", "-m", "g::---,o::---", str(home))
        run("setfacl", "-d", "-m", "g::---,o::---", str(home))
        for old_gid in set(old_gids) - set(gids):
            run("setfacl", "-x", f"g:{old_gid}", str(home))
            run("setfacl", "-d", "-x", f"g:{old_gid}", str(home))
        for gid in set(gids) - set(old_gids):
            # find -xdev does not descend into a local filesystem mounted
            # below a user's HOME; setfacl -R would cross that boundary.
            run("find", str(home), "-xdev", "-fstype", "fuse.glusterfs", "(", "-type", "d", "-o", "-type", "f", ")",
                "-exec", "setfacl", "-m", f"g:{gid}:r-X", "{}", "+")
            run("find", str(home), "-xdev", "-fstype", "fuse.glusterfs", "-type", "d", "-exec",
                "setfacl", "-d", "-m", f"g:{gid}:r-x", "{}", "+")
    else:
        # NFSv4 ACLs live on the server. Use numeric GIDs: this export uses
        # sec=sys and rejects the textual group principal without an idmap.
        # Run as the owner so root_squash does not block the ACL update.
        additions = set(gids) - set(old_gids)
        pending = (set(previous.get("pending_backfill_gids", [])) | additions) & set(gids)
        if backfill_existing and pending:
            foreign = nfs4_as_owner(uid, "find", "-P", str(home), "-xdev", "-fstype", "nfs4",
                                    "!", "-uid", str(uid), "-print", "-quit")
            if foreign:
                raise RuntimeError(f"cannot backfill ACL on non-owned HOME entry: {foreign}")
        for gid in sorted(pending if backfill_existing else ()):
            nfs4_as_owner(uid, "find", "-P", str(home), "-xdev", "-fstype", "nfs4", "-type", "d",
                          "-exec", "nfs4_setfacl", "-a", f"A:fdg:{gid}:RX", "{}", "+")
            nfs4_as_owner(uid, "find", "-P", str(home), "-xdev", "-fstype", "nfs4", "-type", "f",
                          "-exec", "nfs4_setfacl", "-a", f"A:g:{gid}:RX", "{}", "+")
        acl = ["A::OWNER@:rwaDxtTnNcCoy"]
        acl += [f"A:fdg:{gid}:rxtncy" for gid in gids]
        acl += ["A:g:GROUP@:tncy", "A::EVERYONE@:tncy"]
        nfs4_as_owner(uid, "nfs4_setfacl", "-s", ",".join(acl), str(home))
        if not backfill_existing and pending:
            desired["pending_backfill_gids"] = sorted(pending)
    tmp = state_path.with_suffix(".tmp")
    tmp.write_text(json.dumps(desired, sort_keys=True) + "\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, state_path)

def nss_home_identities(home_root):
    """ACL-only mode uses the slave-backed NSS view on an NFS client."""
    identities = []
    groups = grp.getgrall()
    root_parts = home_root.resolve(strict=True).parts
    for account in pwd.getpwall():
        name, uid, gid, home = account.pw_name, account.pw_uid, account.pw_gid, account.pw_dir
        parts = Path(home).parts
        if uid < 10000 or gid < 10000 or len(parts) != len(root_parts) + 2 or parts[:len(root_parts)] != root_parts:
            continue
        if parts[-1] != name or not NAME.fullmatch(name) or not NAME.fullmatch(parts[-2]):
            raise ValueError(f"unsafe NSS HOME identity: {name}")
        supplemental = {g.gr_gid for g in groups if g.gr_gid >= 10000 and name in g.gr_mem} - {gid}
        identities.append((name, uid, gid, parts[-2], home, supplemental))
    if not identities:
        raise RuntimeError("no LDAP POSIX HOME identities were found through NSS")
    return identities

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--home-root", type=Path, default=Path("/HOME"))
    parser.add_argument("--state-root", type=Path,
                        default=Path("/var/lib/go-ldap-admin-home-acl"))
    parser.add_argument("--user", help="limit reconciliation to one LDAP POSIX user")
    parser.add_argument("--no-backfill", action="store_true",
                        help="set root ACL and inheritance, deferring existing children")
    args = parser.parse_args()
    if os.geteuid() != 0:
        raise PermissionError("run as root")
    acl_backend(args.home_root, args.home_root)
    identities = nss_home_identities(args.home_root)
    if args.user:
        identities = [item for item in identities if item[0] == args.user]
        if not identities:
            raise ValueError(f"POSIX user {args.user} is not in the HOME NSS view")
    args.state_root.mkdir(mode=0o700, parents=True, exist_ok=True)
    if args.state_root.is_symlink() or not args.state_root.is_dir():
        raise ValueError(f"unsafe ACL state directory: {args.state_root}")
    os.chmod(args.state_root, 0o700)
    for name, uid, _, _, home, supplementary in identities:
        reconcile_home_acl(args.state_root, uid, Path(home), supplementary,
                           args.home_root, backfill_existing=not args.no_backfill)
        print(f"{name}: HOME ACL on {acl_backend(Path(home), args.home_root)}")


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(f"HOME ACL reconciliation failed: {exc}", file=sys.stderr)
        raise SystemExit(1)
