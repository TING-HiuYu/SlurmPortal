# A minimal OpenLDAP for trying the slave: base DN with ou=people and ou=groups, nothing else.
FROM alpine:3.22
RUN apk add --no-cache openldap openldap-back-mdb openldap-clients \
    && install -d -m 0777 /ldap-data
COPY entrypoint.sh /usr/local/bin/demo-ldap
ENTRYPOINT ["/bin/sh", "/usr/local/bin/demo-ldap"]
