# SSSD 与 SSH

所有运行作业或允许用户登录的节点都必须能够识别门户创建的账号。**SSSD** 从 OpenLDAP 读取并缓存账号与用户组信息，使 `id alice`、文件归属关系以及 Slurm 在全集群范围内看到完全一致的用户身份。

**执行节点：** `ctl`、所有计算节点、`login`。**预计用时：** 10 分钟。

## 1. 安装软件包 {#1-install}

在 `ctl`、所有计算节点与 `login` 上执行：

```sh
apt install -y sssd-ldap sssd-tools libnss-sss libpam-sss ldap-utils libldap-common
```

安装后会自动在 `/etc/nsswitch.conf` 的 `passwd`、`group` 与 `shadow` 行中添加 `sss`。

## 2. 配置 SSSD {#2-configure}

编写 `/etc/sssd/sssd.conf`：

```ini title="/etc/sssd/sssd.conf"
[sssd]
services = nss, pam
domains = example.org

[domain/example.org]
id_provider = ldap
auth_provider = ldap
ldap_uri = ldap://infra.example.org
ldap_search_base = dc=example,dc=org
ldap_user_search_base = ou=people,dc=example,dc=org
ldap_group_search_base = ou=groups,dc=example,dc=org
# The portal writes POSIX groups with memberUid.
ldap_schema = rfc2307
ldap_id_use_start_tls = true
ldap_tls_reqcert = demand
ldap_tls_cacert = /etc/ssl/certs/ca-certificates.crt
# New and changed accounts appear within a minute.
entry_cache_timeout = 60
cache_credentials = false
```

```sh
chmod 600 /etc/sssd/sssd.conf
systemctl restart sssd
```

如果配置文件对其他用户可读，SSSD 会拒绝启动。

| 配置项 | 作用说明 |
| --- | --- |
| `ldap_schema = rfc2307` | 与门户写出的 `posixGroup`（使用 `memberUid` 保存成员用户名）保持一致 |
| `ldap_id_use_start_tls`、`ldap_tls_reqcert`、`ldap_tls_cacert` | 强制启用 StartTLS 加密，并使用系统 CA 证书包（含[OpenLDAP 章节](openldap.md#5-trust-the-ca-on-every-host)中安装的根证书）校验服务端证书 |
| `entry_cache_timeout = 60` | 门户中的账号变更大约在一次 slave 轮询加 1 分钟缓存过期内即可在节点生效 |
| `cache_credentials = false` | 使密码修改立即生效；若希望登录节点在 `infra` 短暂维护期间仍允许密码登录，可设为 `true` |

## 3. 计算节点：禁止普通用户直接 SSH 登录 {#3-compute-nodes-no-user-logins}

普通用户应通过 Slurm 调度使用计算节点，而不应直接 SSH 登录计算节点。在**所有计算节点**上执行：

```sh
sed -i 's/^auth_provider = ldap/auth_provider = none/' /etc/sssd/sssd.conf
cat > /etc/ssh/sshd_config.d/50-slurm-portal-compute.conf <<'EOF'
AllowUsers root
PasswordAuthentication no
KbdInteractiveAuthentication no
EOF
systemctl restart sssd
systemctl reload ssh
```

此时账号依然可以通过 NSS 正常解析（Slurm 作业仍以该用户身份运行），但除 `root` 外任何人无法直接 SSH 登录计算节点。如果您的集群允许用户 SSH 进入正在运行自己作业的计算节点，请改用 Slurm 的 `pam_slurm_adopt` 模块替代 `AllowUsers root`。

## 验证是否生效 {#check-it-works}

此时尚未同步门户用户，可先检查 SSSD 在线状态与配置合法性：

```sh
sssctl domain-status example.org         # "Online status: Online"
getent passwd root                       # still resolves locally
sssctl config-check                      # no issues
```

待后续 slave 完成首次用户同步后，在任意节点执行 `getent passwd <用户名>` 与 `id <用户名>` 即可解析出 UID 从 `10000` 起步的门户用户。

| 常见问题 | 排查方法 |
| --- | --- |
| `Online status: Offline` | 在当前主机执行 `ldapwhoami -x -ZZ -H ldap://infra.example.org`，确认已安装 LDAP CA 证书 |
| `sssd` 无法启动 | 检查 `chmod 600 /etc/sssd/sssd.conf` 并查看 `journalctl -u sssd` |
| 新同步的用户暂时查不到 | 等待 `entry_cache_timeout`（60 秒）过期，或执行 `sss_cache -E` 立即清空缓存 |

## SSH 证书认证说明 {#ssh-certificates}

用户使用门户签发的 SSH 证书登录 `login` 节点。`sshd` 通过 slave 发布的两个文件（`trusted-user-ca-keys.pub` 与 `revoked.krl`）校验用户证书。在 [集群 Slave 代理](slaves.md#point-sshd-at-the-trust-files)完成首次同步后，我们再将 `sshd` 指向这两个文件——因为在吊销列表文件生成之前配置 `RevokedKeys` 会导致 `sshd` 拒绝所有公钥登录。

## 下一步 {#next}

继续阅读[共享主目录与信任文件](shared-home.md)。
