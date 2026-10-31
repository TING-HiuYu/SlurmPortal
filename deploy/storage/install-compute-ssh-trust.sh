#!/usr/bin/env bash
set -euo pipefail

# Install on a compute node whose cluster HOME already contains the slave's
# published trust bundle. Run as root: install-compute-ssh-trust.sh SOURCE
source_dir=${1:?usage: install-compute-ssh-trust.sh /shared/trust/directory}
target_dir=/etc/go-ldap-admin-trust
dropin=/etc/ssh/sshd_config.d/00-go-ldap-admin-trust.conf
stamp=$(date +%Y%m%d-%H%M%S)
backup_dir=/var/backups/go-ldap-admin/compute-ssh-trust-$stamp

test "$(id -u)" -eq 0
test -d "$source_dir/current/ssh"
test -s "$source_dir/current/ssh/trusted-user-ca-keys.pub"
test -s "$source_dir/current/ssh/revoked.krl"
ssh-keygen -lf "$source_dir/current/ssh/trusted-user-ca-keys.pub" >/dev/null

install -d -m 0700 "$backup_dir"
cp -a /etc/fstab /etc/sssd/sssd.conf /etc/ssh/sshd_config "$backup_dir/"
if test -e "$dropin"; then cp -a "$dropin" "$backup_dir/dropin.conf"; fi
install -d -m 0755 "$target_dir"

if ! mountpoint -q "$target_dir"; then
  mount --bind "$source_dir" "$target_dir"
fi
mount -o remount,bind,ro "$target_dir"
if ! grep -Fq " $target_dir none bind,ro,nofail," /etc/fstab; then
  printf '%s %s none bind,ro,nofail,x-systemd.requires-mounts-for=%s 0 0\n' \
    "$source_dir" "$target_dir" "$source_dir" >>/etc/fstab
fi

cat >"$dropin" <<'EOF'
TrustedUserCAKeys /etc/go-ldap-admin-trust/current/ssh/trusted-user-ca-keys.pub
RevokedKeys /etc/go-ldap-admin-trust/current/ssh/revoked.krl
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowUsers root
EOF
chmod 0644 "$dropin"

# NSS still resolves LDAP identities, while PAM cannot authenticate a user
# using the LDAP password on compute nodes. AllowUsers root prevents direct
# user SSH even on nodes where UsePAM is disabled.
sed -i -E 's/^[[:space:]]*auth_provider[[:space:]]*=.*/auth_provider = none/' /etc/sssd/sssd.conf
sed -i -E 's/^[[:space:]]*access_provider[[:space:]]*=.*/access_provider = deny/' /etc/sssd/sssd.conf
# The legacy compute SSSD file used this escape hatch for LDAP password
# authentication; it is obsolete once authentication is disabled.
sed -i -E '/^[[:space:]]*ldap_auth_disable_tls_never_use_in_production[[:space:]]*=/d' /etc/sssd/sssd.conf
if ! grep -Eq '^[[:space:]]*auth_provider[[:space:]]*=[[:space:]]*none([[:space:]]*)$' /etc/sssd/sssd.conf; then
  echo 'SSSD auth_provider setting is missing' >&2
  exit 1
fi
sssctl config-check >/dev/null
/usr/sbin/sshd -t
test "$(/usr/sbin/sshd -T | grep '^allowusers ')" = 'allowusers root'
test "$(findmnt -n -o OPTIONS "$target_dir" | cut -d, -f1)" = ro
test -s "$target_dir/current/ssh/trusted-user-ca-keys.pub"
test -s "$target_dir/current/ssh/revoked.krl"
systemctl daemon-reload
systemctl restart sssd
systemctl reload ssh

printf 'host=%s source=%s backup=%s\n' "$(hostname)" "$source_dir" "$backup_dir"
findmnt -n -o SOURCE,TARGET,FSTYPE,OPTIONS "$target_dir"
/usr/sbin/sshd -T | grep -E '^(trustedusercakeys|revokedkeys|passwordauthentication|kbdinteractiveauthentication) '
grep -E '^[[:space:]]*(id_provider|auth_provider|access_provider)[[:space:]]*=' /etc/sssd/sssd.conf
