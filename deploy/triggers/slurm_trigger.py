#!/usr/bin/env python3
"""Run one slave trigger after LDAP reconciliation. No HTTP listener or shell.

The JSON argument comes from go-ldap-admin-slave. Repeated calls are safe:
the slave delivery queue is at least once, and this script verifies the
resulting Slurm state before reporting success.
"""
import argparse
import json
import os
import re
import shlex
import stat
import subprocess
from pathlib import Path

USERNAME = re.compile(r"^[a-z_][a-z0-9_-]{0,31}$")


def run(prefix, *args):
    result = subprocess.run([*prefix, *args], check=True, timeout=30,
                            text=True, capture_output=True)
    return result.stdout.strip()


def rows(output):
    return {tuple(line.split("|")) for line in output.splitlines() if line}


def identity(prefix, username):
    output = run(prefix, "passwd", username)
    matches = [line.split(":") for line in output.splitlines() if line]
    if len(matches) != 1 or len(matches[0]) != 7 or matches[0][0] != username:
        raise ValueError("expected one LDAP POSIX identity")
    return matches[0]


def account(prefix, cluster, username):
    return (cluster, username, "") in rows(run(prefix, "-nP", "show", "association", "where",
                                               f"cluster={cluster}", f"account={username}",
                                               "format=Cluster,Account,User"))


def association(prefix, cluster, username):
    return (cluster, username, username) in rows(run(prefix, "-nP", "show", "association", "where",
                                                     f"cluster={cluster}", f"account={username}",
                                                     f"user={username}", "format=Cluster,Account,User"))


def default_account(prefix, cluster, username):
    return (username, cluster, username) in rows(run(prefix, "-nP", "show", "user", "WithAssoc", "where",
                                                    f"name={username}", f"cluster={cluster}",
                                                    "format=User,Cluster,DefaultAccount"))


def provision(prefix, cluster, username):
    if not account(prefix, cluster, username):
        run(prefix, "-i", "add", "account", username, f"cluster={cluster}")
    if not association(prefix, cluster, username):
        run(prefix, "-i", "add", "user", username, f"account={username}",
            f"defaultaccount={username}", f"cluster={cluster}")
    if not association(prefix, cluster, username):
        raise RuntimeError("Slurm association was not created")
    if not default_account(prefix, cluster, username):
        run(prefix, "-i", "modify", "user", "where", f"name={username}",
            f"cluster={cluster}", "set", f"defaultaccount={username}")
    if not default_account(prefix, cluster, username):
        raise RuntimeError("Slurm DefaultAccount was not updated")


def delete(prefix, cluster, username):
    if association(prefix, cluster, username):
        run(prefix, "-i", "delete", "user", f"name={username}", f"cluster={cluster}")
    if association(prefix, cluster, username):
        raise RuntimeError("Slurm user association still exists")
    if account(prefix, cluster, username):
        run(prefix, "-i", "delete", "account", f"name={username}", f"cluster={cluster}")
    if account(prefix, cluster, username):
        raise RuntimeError("Slurm account still exists")


def disable(prefix, cluster, username):
    # Retain accounting history; block future submissions.
    if not association(prefix, cluster, username):
        return
    def blocked():
        return (username, cluster, "0") in rows(run(prefix, "-nP", "show", "user", "WithAssoc", "where",
                                                      f"name={username}", f"cluster={cluster}",
                                                      "format=User,Cluster,MaxSubmitJobs"))
    if not blocked():
        run(prefix, "-i", "modify", "user", "where", f"name={username}",
            f"cluster={cluster}", "set", "MaxSubmitJobs=0")
    if not blocked():
        raise RuntimeError("Slurm MaxSubmitJobs was not set to zero")


def ensure_hdd_acl(config, user, hdd):
    """Give current LDAP supplementary groups read/traverse on XFS HDD."""
    gids = {int(value) for value in run(config["groupIdsPrefix"], user["username"]).split()}
    gids = {gid for gid in gids if gid >= 10000 and gid != user["gidNumber"]}
    state_root = Path("/var/lib/go-ldap-admin-hdd-acl")
    state_root.mkdir(mode=0o700, exist_ok=True)
    info = state_root.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o077:
        raise ValueError("unsafe HDD ACL state directory")
    state_path = state_root / f'{user["uidNumber"]}.json'
    old = json.loads(state_path.read_text()) if state_path.exists() else {}
    if old and old.get("path") != str(hdd):
        raise ValueError("HDD ACL path changed; explicit migration required")
    old_gids = set(old.get("gids", []))

    def acl(*args):
        subprocess.run(["/usr/bin/setfacl", *args], check=True, timeout=240,
                       capture_output=True, text=True)

    def walk(kind, *args):
        subprocess.run(["/usr/bin/find", "-P", str(hdd), "-xdev", *kind,
                        "-exec", "/usr/bin/setfacl", *args, "{}", "+"],
                       check=True, timeout=240, capture_output=True, text=True)

    acl("-m", "g::---,o::---", str(hdd))
    acl("-d", "-m", "u::rwx,g::---,o::---", str(hdd))
    for gid in sorted(old_gids - gids):
        walk(["(", "-type", "d", "-o", "-type", "f", ")"], "-x", f"g:{gid}")
        walk(["-type", "d"], "-d", "-x", f"g:{gid}")
    for gid in sorted(gids - old_gids):
        walk(["(", "-type", "d", "-o", "-type", "f", ")"], "-m", f"g:{gid}:r-X")
        walk(["-type", "d"], "-d", "-m", f"g:{gid}:r-x")
    for gid in sorted(gids):
        acl("-m", f"g:{gid}:r-x", str(hdd))
        acl("-d", "-m", f"g:{gid}:r-x", str(hdd))
    for gid in sorted(old_gids - gids):
        acl("-x", f"g:{gid}", str(hdd))
        acl("-d", "-x", f"g:{gid}", str(hdd))
    temporary = state_path.with_suffix(".tmp")
    temporary.write_text(json.dumps({"path": str(hdd), "gids": sorted(gids)}) + "\n")
    os.chmod(temporary, 0o600)
    os.replace(temporary, state_path)


def ensure_storage(config, user):
    """Use the cluster's existing ACL/quota services, then install safe links."""
    home_root = Path(config["homeRoot"])
    home = Path(user["homeDirectory"])
    username = user["username"]
    if len(home.parts) != len(home_root.parts) + 2 or home.parts[:len(home_root.parts)] != home_root.parts or home.name != username:
        raise ValueError("HOME is outside the configured organization tree")
    group = home.parent.name
    if not USERNAME.fullmatch(group):
        raise ValueError("invalid HOME group")
    filesystem = run(["/usr/bin/findmnt"], "-n", "-o", "FSTYPE", "--mountpoint", str(home_root))
    if filesystem not in ("nfs4", "fuse.glusterfs"):
        raise ValueError("HOME must be a mounted NFSv4 or GlusterFS filesystem")
    if home_root.is_symlink() or home_root.stat().st_uid != 0:
        raise ValueError("unsafe HOME root")
    if not home.parent.exists() and not home.parent.is_symlink():
        home.parent.mkdir(mode=0o755)
    parent_info = home.parent.lstat()
    if (not stat.S_ISDIR(parent_info.st_mode) or home.parent.is_symlink() or
            parent_info.st_uid != 0 or parent_info.st_mode & 0o022 or
            parent_info.st_dev != home_root.stat().st_dev):
        raise ValueError("unsafe HOME parent directory")
    if not home.exists() and not home.is_symlink():
        home.mkdir(mode=0o700)
        os.chown(home, user["uidNumber"], user["gidNumber"])
    for command in config.get("storageCommands", []):
        if not isinstance(command, list) or not command or not all(isinstance(arg, str) for arg in command):
            raise ValueError("storageCommands must contain argument arrays")
        argv = [username if arg == "%USERNAME%" else arg for arg in command]
        subprocess.run(argv, check=True, timeout=240, capture_output=True, text=True)

    has_hdd = "hddHostRoot" in config and "hddClientRoot" in config
    hdd = Path(config["hddHostRoot"]) / group / username if has_hdd else None
    shared = home.parent / "shared"
    if not shared.is_dir():
        shared = Path(config["sharedRoot"])
    if not shared.is_dir() or shared.is_symlink():
        raise ValueError("shared directory is unavailable")
    uid, gid = user["uidNumber"], user["gidNumber"]
    directories = [home]
    if hdd is not None:
        directories.append(hdd)
    for path in directories:
        info = path.lstat()
        if not stat.S_ISDIR(info.st_mode) or path.is_symlink() or (info.st_uid, info.st_gid) != (uid, gid):
            raise ValueError(f"unsafe user directory: {path}")
        parent = path.parent.lstat()
        if not stat.S_ISDIR(parent.st_mode) or parent.st_uid != 0 or parent.st_mode & 0o022:
            raise ValueError(f"user could delete the directory through its parent: {path}")
    if hdd is not None:
        ensure_hdd_acl(config, user, hdd)

    links = [("SHARED", shared)]
    if has_hdd:
        links.insert(0, ("HDD_POOL", Path(config["hddClientRoot"]) / group / username))
    for name, target in links:
        link = home / name
        if not link.exists() and not link.is_symlink():
            link.symlink_to(target)
            os.lchown(link, uid, gid)

    snippet = "\n# >>> go-ldap-admin-slave managed links >>>\n"
    for name, target in links:
        snippet += (f'if [ ! -e "$HOME/{name}" ] && [ ! -L "$HOME/{name}" ]; then '
                    f'ln -s {shlex.quote(str(target))} "$HOME/{name}" 2>/dev/null || true; fi\n')
    snippet += "# <<< go-ldap-admin-slave managed links <<<\n"
    for name in (".profile", ".bashrc", ".zshrc"):
        path = home / name
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o644)
        try:
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, uid):
                raise ValueError(f"unsafe shell profile: {path}")
            existing = os.read(fd, 1 << 20)
            if b"go-ldap-admin-slave managed links" not in existing:
                os.lseek(fd, 0, os.SEEK_END)
                os.write(fd, snippet.encode())
                os.fsync(fd)
            if info.st_uid == 0 and info.st_size == 0:
                os.fchown(fd, uid, gid)
        finally:
            os.close(fd)


def apply(config, payload):
    event = payload.get("event")
    if event not in {"user.created", "user.updated", "user.deleted", "membership.added", "membership.removed"}:
        raise ValueError("unsupported event")
    user = payload.get("data", {}).get("user", {})
    username = user.get("username")
    if not isinstance(username, str) or not USERNAME.fullmatch(username):
        raise ValueError("invalid username")
    # The master snapshot also contains its application administrator. Only
    # ordinary POSIX identities are provisioned into Slurm and storage.
    if user.get("uidNumber", 0) < 10000 or user.get("gidNumber", 0) < 10000:
        return
    prefix = config["sacctmgrPrefix"]
    cluster = config["cluster"]
    try:
        entry = identity(config["identityPrefix"], username)
    except subprocess.CalledProcessError as exc:
        if exc.returncode != 2:
            raise
        entry = None
    if event == "user.deleted":
        if entry is not None:
            raise ValueError("LDAP identity still exists; refusing deletion")
        if config["deletePolicy"] == "delete":
            delete(prefix, cluster, username)
        else:
            disable(prefix, cluster, username)
        return
    if entry is None and event == "membership.removed":
        # Deleting a user also removes its memberships. The user deletion
        # delivery handles Slurm deprovisioning separately.
        return
    if entry is None:
        raise ValueError("LDAP identity has not appeared yet")
    if int(entry[2]) != user["uidNumber"] or int(entry[3]) != user["gidNumber"] or entry[5] != user["homeDirectory"]:
        raise ValueError("LDAP identity differs from synchronized snapshot")
    if int(entry[2]) < 10000 or int(entry[3]) < 10000:
        raise ValueError("system identity is not eligible")
    provision(prefix, cluster, username)
    ensure_storage(config, user)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", required=True)
    parser.add_argument("payload")
    args = parser.parse_args()
    path = Path(args.config)
    if not path.is_file() or path.stat().st_mode & 0o077:
        parser.error("config must be a regular 0600 file")
    config = json.loads(path.read_text())
    if config.get("deletePolicy") not in {"delete", "disable"}:
        parser.error("deletePolicy must be delete or disable")
    for key in ("sacctmgrPrefix", "identityPrefix"):
        prefix = config.get(key)
        if not isinstance(prefix, list) or not prefix or not all(isinstance(x, str) for x in prefix):
            parser.error(f"{key} must be a nonempty argument array")
    for key in ("homeRoot", "sharedRoot"):
        if not isinstance(config.get(key), str) or not Path(config[key]).is_absolute():
            parser.error(f"{key} must be an absolute path")
    if ("hddHostRoot" in config) != ("hddClientRoot" in config):
        parser.error("hddHostRoot and hddClientRoot must be configured together")
    if "hddHostRoot" in config:
        prefix = config.get("groupIdsPrefix")
        if not isinstance(prefix, list) or not prefix or not all(isinstance(x, str) for x in prefix):
            parser.error("groupIdsPrefix must be a nonempty argument array when HDD is configured")
    for key in ("hddHostRoot", "hddClientRoot"):
        if key in config and (not isinstance(config[key], str) or not Path(config[key]).is_absolute()):
            parser.error(f"{key} must be an absolute path")
    if not re.fullmatch(r"[a-zA-Z0-9_.-]+", config.get("cluster", "")):
        parser.error("invalid cluster name")
    apply(config, json.loads(args.payload))


if __name__ == "__main__":
    main()
