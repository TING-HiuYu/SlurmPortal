#!/usr/bin/env bash
set -euo pipefail

# Use a short-cache NFS mount for CA and KRL updates instead of the HOME
# mount, whose attribute cache may delay revocation visibility for 30 minutes.
server=${1:?usage: install-trust-nfs.sh NFS_SERVER:/export/path/of/the/trust/directory}
source_dir=/var/lib/go-ldap-admin-trust-source
target_dir=/etc/go-ldap-admin-trust
test "$(id -u)" -eq 0
test -d "$target_dir"
install -d -m 0755 "$source_dir"
backup=/var/backups/go-ldap-admin/fstab.before-trust-nfs-$(date +%Y%m%d-%H%M%S)
install -d -m 0700 /var/backups/go-ldap-admin
cp -a /etc/fstab "$backup"

if ! mountpoint -q "$source_dir"; then
  mount -t nfs4 -o ro,hard,vers=4.2,actimeo=5 "$server" "$source_dir"
fi
test -s "$source_dir/current/ssh/trusted-user-ca-keys.pub"
test -s "$source_dir/current/ssh/revoked.krl"
ssh-keygen -lf "$source_dir/current/ssh/trusted-user-ca-keys.pub" >/dev/null

if mountpoint -q "$target_dir"; then umount "$target_dir"; fi
mount --bind "$source_dir" "$target_dir"
mount -o remount,bind,ro "$target_dir"
test "$(findmnt -n -o OPTIONS "$target_dir" | cut -d, -f1)" = ro

python3 - "$server" "$source_dir" "$target_dir" <<'PY'
import os
import pathlib
import sys
import tempfile

server, source, target = sys.argv[1:]
fstab = pathlib.Path('/etc/fstab')
lines = fstab.read_text().splitlines()
lines = [line for line in lines if not (
    len(fields := line.split()) >= 2 and fields[1] in (source, target)
)]
lines.extend((
    f'{server} {source} nfs4 ro,hard,vers=4.2,actimeo=5,_netdev,nofail 0 0',
    f'{source} {target} none bind,ro,nofail,x-systemd.requires-mounts-for={source} 0 0',
))
fd, temporary = tempfile.mkstemp(prefix='.fstab.', dir='/etc')
try:
    with os.fdopen(fd, 'w') as output:
        output.write('\n'.join(lines) + '\n')
    os.chmod(temporary, 0o644)
    os.replace(temporary, fstab)
finally:
    if os.path.exists(temporary):
        os.unlink(temporary)
PY
systemctl daemon-reload
/usr/sbin/sshd -t
printf 'host=%s backup=%s\n' "$(hostname)" "$backup"
findmnt -n -o SOURCE,TARGET,FSTYPE,OPTIONS "$source_dir"
findmnt -n -o SOURCE,TARGET,FSTYPE,OPTIONS "$target_dir"
