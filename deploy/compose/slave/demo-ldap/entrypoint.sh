#!/bin/sh
# Writes slapd.conf for LDAP_BASE_DN with the admin password from /run/ldap-password, creates the
# base entries on first start, and runs slapd in the foreground on port 1389.
set -eu
base=${LDAP_BASE_DN:-dc=example,dc=org}
data=/ldap-data
conf=/tmp/slapd.conf
pw=$(slappasswd -s "$(cat /run/ldap-password)")
cat > "$conf" <<CONF
include /etc/openldap/schema/core.schema
include /etc/openldap/schema/cosine.schema
include /etc/openldap/schema/inetorgperson.schema
include /etc/openldap/schema/nis.schema
pidfile /tmp/slapd.pid
modulepath /usr/lib/openldap
moduleload back_mdb.so
database mdb
suffix "$base"
rootdn "cn=admin,$base"
rootpw $pw
directory $data
index objectClass eq
index uid,memberUid eq
CONF
if [ ! -f "$data/data.mdb" ]; then
  dc=$(printf '%s' "$base" | sed -E 's/^dc=([^,]+).*/\1/')
  slapadd -f "$conf" <<LDIF
dn: $base
objectClass: dcObject
objectClass: organization
dc: $dc
o: $dc

dn: ou=people,$base
objectClass: organizationalUnit
ou: people

dn: ou=groups,$base
objectClass: organizationalUnit
ou: groups
LDIF
fi
exec slapd -d 0 -f "$conf" -h "ldap://0.0.0.0:1389/"
