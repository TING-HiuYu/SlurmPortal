# OpenLDAP

OpenLDAP stores the cluster's local POSIX accounts, groups, and user certificates. The Slurm Portal slave agent reconciles snapshots into OpenLDAP, while SSSD on each cluster node queries it for user and group lookups. Because the slave manages all account entries, this page simply initializes a clean directory with TLS encryption.

**Target host:** `infra`. **Estimated time:** 15 minutes.

## 1. Install slapd

Pre-seed `debconf` before installing `slapd` so the directory is initialized with suffix `dc=example,dc=org` and administrator DN `cn=admin,dc=example,dc=org`:

```sh
LDAP_ADMIN_PW=$(openssl rand -base64 24)
printf '%s' "$LDAP_ADMIN_PW" > /root/ldap-admin-password && chmod 600 /root/ldap-admin-password

debconf-set-selections <<EOF
slapd slapd/domain string example.org
slapd shared/organization string Example
slapd slapd/password1 password $LDAP_ADMIN_PW
slapd slapd/password2 password $LDAP_ADMIN_PW
EOF
apt install -y slapd ldap-utils
```

Keep `/root/ldap-admin-password` safe—you will copy it to `ctl` when configuring the slave agent.

## 2. Create the two branches

Create the two base organizational units expected by the slave (`ou=people` for users and `ou=groups` for groups when `local-group-dn` is configured):

```sh
ldapadd -x -H ldap://localhost -D cn=admin,dc=example,dc=org -y /root/ldap-admin-password <<EOF
dn: ou=people,dc=example,dc=org
objectClass: organizationalUnit
ou: people

dn: ou=groups,dc=example,dc=org
objectClass: organizationalUnit
ou: groups
EOF
```

:::note
`ldapadd -y` reads the entire password file verbatim, including any trailing newline. Writing the file with `printf '%s'` (instead of `echo`) avoids appending a newline character.
:::

## 3. Indexes

Ubuntu's default `slapd` configuration already indexes `uid`, `uidNumber`, `gidNumber`, and `memberUid`. Add an equality index on `uniqueMember` for organizational unit (`groupOfUniqueNames`) lookups:

```sh
ldapmodify -Q -Y EXTERNAL -H ldapi:/// <<EOF
dn: olcDatabase={1}mdb,cn=config
changetype: modify
add: olcDbIndex
olcDbIndex: uniqueMember eq
EOF
```

## 4. TLS

Because LDAP connections carry password hashes and user certificates across the cluster network, both SSSD and the slave agent require StartTLS or LDAPS whenever connecting over a non-loopback address. Below, we generate a dedicated ECDSA P-256 CA and server certificate for `infra.example.org` (or substitute a certificate from your existing institutional CA):

```sh
mkdir -p /etc/ldap/tls && cd /etc/ldap/tls
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 \
  -subj "/CN=Example LDAP CA" -keyout ca.key -out ca.crt
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -subj "/CN=infra.example.org" -addext "subjectAltName=DNS:infra.example.org,DNS:infra" \
  -keyout ldap.key -out ldap.csr
openssl x509 -req -in ldap.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 825 \
  -copy_extensions copy -out ldap.crt
chown root:openldap ldap.key && chmod 640 ldap.key && chmod 600 ca.key
```

Configure `slapd` to use the TLS certificate and enable `ldaps:///` (port `636`) alongside `ldap:///` (port `389` with StartTLS):

```sh
ldapmodify -Q -Y EXTERNAL -H ldapi:/// <<EOF
dn: cn=config
changetype: modify
replace: olcTLSCACertificateFile
olcTLSCACertificateFile: /etc/ldap/tls/ca.crt
-
replace: olcTLSCertificateFile
olcTLSCertificateFile: /etc/ldap/tls/ldap.crt
-
replace: olcTLSCertificateKeyFile
olcTLSCertificateKeyFile: /etc/ldap/tls/ldap.key
EOF
sed -i 's|^SLAPD_SERVICES=.*|SLAPD_SERVICES="ldap:/// ldaps:/// ldapi:///"|' /etc/default/slapd
systemctl restart slapd
```

## 5. Trust the CA on every host

Install `/etc/ldap/tls/ca.crt` into the system trust store on every cluster host (`ctl`, `infra`, compute nodes, and `login`) so SSSD, OpenLDAP CLI tools, and the slave agent trust the LDAP server certificate. Run from `ctl`:

```sh
scp root@infra:/etc/ldap/tls/ca.crt /usr/local/share/ca-certificates/example-ldap-ca.crt
update-ca-certificates
for host in infra node1 login; do
  scp /usr/local/share/ca-certificates/example-ldap-ca.crt root@$host:/usr/local/share/ca-certificates/
  ssh root@$host update-ca-certificates
done
```

## Check it works

From any other cluster host (after running `apt install -y ldap-utils libldap-common` so `/etc/ldap/ldap.conf` points to the system CA bundle), test both StartTLS and LDAPS:

```sh
ldapwhoami -x -ZZ -H ldap://infra.example.org      # StartTLS
ldapwhoami -x -H ldaps://infra.example.org         # LDAPS
ldapsearch -x -LLL -ZZ -H ldap://infra.example.org -b dc=example,dc=org dn
```

Both `ldapwhoami` commands should output `anonymous`, and `ldapsearch` should list `dc=example,dc=org`, `ou=people`, and `ou=groups`.

| Error Message | Cause & Resolution |
| --- | --- |
| `Can't contact LDAP server` | `slapd` is stopped on `infra`, or firewall rules block ports `389`/`636` |
| `Connect error` with `-ZZ` | The client host has not run `update-ca-certificates`, or the hostname does not match the certificate's SAN |
| `Invalid credentials` | Incorrect admin password, or a trailing newline in `/root/ldap-admin-password` |

## Who can read what

Ubuntu's default `slapd` ACLs allow anonymous reads of POSIX attributes (`uid`, `uidNumber`, `gidNumber`, `homeDirectory`, `loginShell`) while restricting `userPassword` to authentication binds—which matches what SSSD requires out of the box. If your security policy prohibits anonymous directory reads on the cluster network, you can create a read-only bind DN and configure `ldap_default_bind_dn` in `sssd.conf`.

## Next

Proceed to [SSSD & SSH](sssd-ssh.md).
