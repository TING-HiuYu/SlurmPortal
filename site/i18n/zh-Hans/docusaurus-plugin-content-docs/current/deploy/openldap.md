# OpenLDAP

集群的账户数据保存在 OpenLDAP 中：Slurm Portal 的 slave 代理负责将用户、用户组与证书写入目录，各节点上的 SSSD 则从中读取身份信息。由于具体条目完全由门户自动维护，本页只需初始化一个启用 TLS 的空目录。

**执行节点：** `infra`。**预计用时：** 15 分钟。

## 1. 安装 slapd {#1-install-slapd}

预先注入 `debconf` 配置项，使软件包安装时自动以 `dc=example,dc=org` 作为目录后缀、以 `cn=admin,dc=example,dc=org` 作为管理员 DN：

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

请妥善保留 `/root/ldap-admin-password`，后续配置 slave 时需要使用。

## 2. 创建基础组织单元分支 {#2-create-the-two-branches}

门户将用户保存在 `ou=people` 下；当按[集群 Slave 代理指南](slaves.md)设置 `local-group-dn` 时，slave 会将用户组保存在 `ou=groups` 下：

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
`ldapadd -y` 会将整个文件内容原样作为密码读取，因此密码文件末尾不能包含换行符——这也是上方使用 `printf '%s'` 而非 `echo` 写入文件的原因。
:::

## 3. 添加索引 {#3-indexes}

Ubuntu 默认的 `slapd` 已对 `uid`、`uidNumber`、`gidNumber` 与 `memberUid` 建立索引。请为组织架构组使用的 `uniqueMember` 属性补充相等性索引：

```sh
ldapmodify -Q -Y EXTERNAL -H ldapi:/// <<EOF
dn: olcDatabase={1}mdb,cn=config
changetype: modify
add: olcDbIndex
olcDbIndex: uniqueMember eq
EOF
```

## 4. 配置 TLS 加密 {#4-tls}

除与 `slapd` 同机运行的进程外，所有客户端连接均须启用 TLS，以保护传输途中的密码哈希与证书。下方命令为目录服务创建一个小型私有 CA 与服务器证书（若您已有单位内部 CA，也可直接使用）：

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

配置 `slapd` 加载该证书，并同时启用 `ldaps:///`（636 端口）监听：

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

## 5. 在所有集群节点上信任该 CA {#5-trust-the-ca-on-every-host}

将 `ca.crt` 安装为所有集群节点（含 `infra` 自身）的系统受信任根证书，供 SSSD 与 slave 共同使用。在 `ctl` 上执行：

```sh
scp root@infra:/etc/ldap/tls/ca.crt /usr/local/share/ca-certificates/example-ldap-ca.crt
update-ca-certificates
for host in infra node1 login; do
  scp /usr/local/share/ca-certificates/example-ldap-ca.crt root@$host:/usr/local/share/ca-certificates/
  ssh root@$host update-ca-certificates
done
```

## 验证是否生效 {#check-it-works}

在任意其他节点上（先执行 `apt install -y ldap-utils libldap-common`，其中 `libldap-common` 会配置 `/etc/ldap/ldap.conf` 指向系统 CA 证书包），测试 StartTLS 与 LDAPS 连接：

```sh
ldapwhoami -x -ZZ -H ldap://infra.example.org      # StartTLS
ldapwhoami -x -H ldaps://infra.example.org         # LDAPS
ldapsearch -x -LLL -ZZ -H ldap://infra.example.org -b dc=example,dc=org dn
```

两条 `ldapwhoami` 命令均应输出 `anonymous`，且 `ldapsearch` 应列出根后缀及两个组织单元分支。

| 报错信息 | 原因与排查方法 |
| --- | --- |
| `Can't contact LDAP server` | `slapd` 未运行，或 389 / 636 端口被防火墙拦截 |
| `-ZZ` 报 `Connect error` | 客户端未信任 CA 证书，或访问的主机名不在服务器证书的 SAN 列表中 |
| `Invalid credentials` | 管理员密码错误，或密码文件末尾多余了换行符 |

## 访问控制说明 {#who-can-read-what}

Ubuntu 默认的 `slapd` 访问规则允许读取除 `userPassword`（仅允许用于绑定认证）以外的所有属性，正好满足 SSSD 的工作要求。如果希望限制仅集群节点可读，可在目录中创建一个只读服务账号并在 `sssd.conf` 中配置 `ldap_default_bind_dn`。

## 下一步 {#next}

继续阅读 [SSSD 与 SSH](sssd-ssh.md)。
