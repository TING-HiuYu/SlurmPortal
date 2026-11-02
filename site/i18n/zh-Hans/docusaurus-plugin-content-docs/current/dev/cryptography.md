# 密码学与签名

本页汇总系统中使用的每一把密钥、签名机制、加密方案与哈希算法：它们各自保护什么、由谁校验、有效期多长以及如何安全轮换。在修改本页涉及的任何参数前请务必仔细阅读——部分派生参数与附加认证数据已绑定在持久化密文中，随意更改会导致存量数据无法解密。

## 算法与密钥总览 {#at-a-glance}

| 机制 | 算法 | 签名 / 加密方 | 校验 / 解密方 | 有效期 |
| --- | --- | --- | --- | --- |
| 用户会话令牌 | JWT `HS256` | Master（`JWT_KEYS`） | Master | 默认 12 小时（最长 168 小时） |
| Web 网关票据 | JWT `EdDSA`（Ed25519） | Master（`PLUGIN_WEB_ACCESS_TICKET_KEY`） | Web 访问网关 | ≤ 5 分钟 |
| 用户 SSH 登录证书 | OpenSSH 证书，Ed25519 CA | Master（SSH CA） | 各集群 `sshd` | 30 天 |
| Web 会话临时 SSH 证书 | OpenSSH 证书，Ed25519 CA | Master（SSH CA） | Web 访问网关与 `sshd` | ≤ 5 分钟 |
| SSH 密钥吊销列表 | OpenSSH KRL | Slave（`ssh-keygen -k`） | 各集群 `sshd`（`RevokedKeys`） | 持续有效直至下次同步更新 |
| 用户 X.509 客户端证书 | ECDSA P-256 | Master（导入的 X.509 CA） | 集群内部服务 | 1 年 |
| X.509 吊销列表（CRL） | CA 签名 CRL | Master | 集群内部服务 | 48 小时（剩余不足 12 小时自动重签） |
| 机器节点身份 | mTLS，ECDSA P-256（`issue.sh`） | 私有同步 CA 或 Cloudflare CA | Master（按指纹）与各代理程序 | 默认 825 天 |
| CA 私钥静态封存 | AES-256-GCM | `PKI_MASTER_KEY` | Master | 直至移除该 CA |
| 服务商密钥静态封存 | AES-256-GCM + HKDF-SHA256 | `PKI_MASTER_KEY` | Master | 直至更新密钥 |
| 集群同步快照 | SHA-256 | Master | Slave | 单次同步周期 |
| 审计哈希链 | SHA-256 哈希链 | Master | `GET /api/v2/audit/verify` | 所有保留的审计记录 |
| 门户登录密码 | Argon2id | Master | Master | 直至用户修改密码 |
| OpenLDAP 密码 | SHA-512 crypt | Master | OpenLDAP / SSSD | 直至用户修改密码 |
| 密码重置验证码 | HMAC-SHA256 | Master | Master | 10 分钟 |

## 需由运维人员提供的密钥 {#keys-you-provide}

| 密钥变量 | 格式 | 用途 | 轮换方式 |
| --- | --- | --- | --- |
| `JWT_KEYS`、`JWT_ACTIVE_KID` | `kid:base64,…`（每个密钥 ≥ 32 字节） | 签发会话令牌与生成重置验证码 HMAC | 追加新密钥并切换 `JWT_ACTIVE_KID`，待旧令牌过期后再移除旧密钥 |
| `PKI_MASTER_KEY` | 32 字节随机数的 Base64 | 加密封存 CA 私钥与数据库中的服务商密钥 | **不可直接替换**（替换前需重加密所有密文），请务必妥善离线备份 |
| `PLUGIN_WEB_ACCESS_TICKET_KEY` | 32 字节 Ed25519 种子的 Base64 | 签发 Web 访问网关票据 | 以新 `ticket-key-id` 生成新密钥，先在所有网关配置中同时保留新旧公钥，再切换主服务 |
| mTLS 同步 CA 与客户端证书 | PEM（`deploy/certs/issue.sh`） | 主服务与各集群代理之间的双向认证 | 签发新客户端证书，先在主服务更新指纹白名单，再替换代理节点上的证书 |

:::warning
一旦丢失 `PKI_MASTER_KEY`，数据库中封存的所有 CA 私钥与服务商密钥将永久无法解密。已签发的用户证书在到期前仍可使用，但必须重新创建并信任新的 CA 后才能继续签发新证书。
:::

## 会话令牌（Session Tokens） {#session-tokens}

- 仅接受 `HS256` 算法；声明其他 `alg` 的令牌在验签前直接拒绝。
- 包含 `iss`、`aud`、`sub`、`jti`、`iat`、`nbf`、`exp`、`src` 声明；JWT 头部的 `kid` 用于在密钥轮换期间定位校验密钥。
- 退出登录或刷新令牌时，旧令牌的 `jti` 会写入 `v2_revoked_tokens` 直至其自然过期。
- 每次请求都会从数据库重新加载用户的启用状态与当前角色，确保撤权或禁用立即生效。

详见[身份与访问层：会话令牌](layer-access.md#users-session-tokens)。

## 网关票据与 Web 会话临时证书 {#gateway-tickets-and-web-session-certificates}

浏览器在使用 Web 终端时从不接触长期有效的 SSH 凭据：

1. 浏览器通过 WebCrypto 在内存中生成一对**临时 Ed25519 密钥**。
2. 调用 `POST /api/v2/web-access/ssh-certificate` 为该公钥签发一张有效期最长 5 分钟的 **SSH 证书**（Principal 为当前用户名，仅含 `permit-pty` 权限，Key ID 为 `web-access/<cluster>/access/user:<id>/<serial>`）。
3. 调用 `POST /api/v2/web-access/ticket` 获取一张由 Ed25519 签名的 **JWT 票据**（包含目标网关 ID、集群 ID、权限范围，有效期 300 秒）。
4. 网关使用 `ticket_public_keys` 校验票据，使用受信 CA 校验临时证书，并确认两者指向同一个用户与集群。

这类临时证书受 5 分钟有效期严格限制（`pki.MaxTemporarySSH`），因此不写入数据库或吊销列表。

## SSH 证书颁发机构（SSH CA） {#ssh-certificate-authority}

- CA 密钥采用 **Ed25519** 算法，支持在门户内直接生成或由管理员外部导入。
- **标准用户证书**有效期为 30 天；门户生成密钥对后仅在响应中返回一次私钥，服务端绝不留存。
- 证书 Principal 与 Key ID 均为用户名，启用 `permit-pty`、X11 转发、Agent 转发与端口转发扩展。
- **证书吊销：** 吊销后序列号写入 `revoked_certificates` 表并随下一份快照下发，各集群 slave 调用 `ssh-keygen -k` 重新生成 `ssh/revoked.krl`，由 `sshd` 的 `RevokedKeys <trust>/current/ssh/revoked.krl` 强制拦截。
- **信任保护：** `ssh/trusted-user-ca-keys.pub` 列出所有已启用的 SSH CA。除非显式开启 `limits.allow-ca-removal`，否则 slave 会拒绝任何移除现有受信 CA 的同步计划，防止误操作导致全员无法登录。

## X.509 证书颁发机构 {#x509-authority}

- 支持导入外部 X.509 CA 证书与私钥。
- 用户证书采用 **ECDSA P-256** 算法（Subject `CN = 用户名`，仅限客户端认证用途，有效期 1 年且不超过 CA 自身有效期），并以 `userCertificate;binary` 属性写入 OpenLDAP。
- **CRL 吊销列表**有效期为 48 小时；当剩余有效期不足 12 小时或发生新吊销时自动重签，由 slave 写入 `crl/ca-<id>.pem`。

## 机器双向 TLS 认证（mTLS） {#machine-identity-mutual-tls}

`deploy/certs/issue.sh` 用于生成 10 年有效期的私有 ECDSA P-256 根 CA，以及主服务同步端点和各代理节点的证书（默认有效期 825 天，可通过 `DAYS=` 调整）：

- **主服务侧：** 由 Cloudflare（或容器主服务的 `SYNC_CLIENT_CA_FILE`）验证证书链，随后主服务仅放行指纹白名单（`CLUSTERS`、`web-access.gateways` 与 `SYNC_CLIENT_FINGERPRINTS`）中登记的客户端证书。
- **代理侧：** 各代理通过 `tls.ca-file` 与 `tls.server-name` 严格验证主服务身份。
- **私钥权限：** 私钥文件权限必须为 `0600`，否则代理拒绝启动。

所有节点证书签发完毕后，请将根私钥 `ca.key` 离线妥善保存。

## 数据库静态机密加密（Sealing at Rest） {#sealing-secrets-at-rest}

系统采用两套基于 `PKI_MASTER_KEY` 的 AES-256-GCM 加密封存方案：

| 维度 | CA 私钥（`domain/pki`） | 其他服务商密钥（`internal/secretbox`） |
| --- | --- | --- |
| 加密算法 | AES-256-GCM | AES-256-GCM |
| 密钥来源 | 直接使用 32 字节 `PKI_MASTER_KEY` | `HKDF-SHA256(PKI_MASTER_KEY, info = "go-ldap-admin/" + purpose)` |
| 附加认证数据（AAD） | CA 身份元数据的 SHA-256 摘要 | 用途标识字符串（`purpose`） |
| 保护对象 | SSH 与 X.509 CA 私钥 | SMTP 密码、REST 邮件密钥、OAuth 客户端密钥等 |

附加认证数据（AAD）将密文与其所属位置严格绑定：将某一行或某一用途的密文复制到别处将无法通过解密校验。

:::danger
HKDF 的 `info` 前缀 `"go-ldap-admin/"` 及各用途标识字符串已绑定在所有历史密文中，更改它们会导致存量密钥无法解密，因此代码中保留了项目原名。
:::

## 哈希与摘要 {#hashes}

- **快照哈希：** 快照 JSON 的 SHA-256 摘要，发布格式为 `v2:<hex>`。Slave 下载快照后会重新计算 SHA-256 并与 `X-Data-Hash` 头比对。
- **审计哈希链：** 每条审计记录对自身 JSON（`hash` 置空，内含前一条记录的 `prevHash`）计算 SHA-256；首条记录的 `prevHash` 为 64 个 `0`。包含 `password`、`token`、`key` 等敏感键名的详情字段在计算哈希前会先自动脱敏。
- **OAuth 状态防伪：** 随机状态码保存在浏览器 Cookie 中，数据库仅存储其 SHA-256 摘要（有效期 10 分钟）。

## 密码算法对比 {#passwords}

| 维度 | 门户登录密码 | OpenLDAP 密码 |
| --- | --- | --- |
| 哈希算法 | Argon2id（2 轮、19 MiB 内存、1 线程） | `{CRYPT}$6$`（SHA-512 crypt，100,000 轮） |
| 校验位置 | 主服务（恒定时间比对） | 各集群节点上的 OpenLDAP / SSSD |
| 存储字段 | `v2_user_credentials.app_hash` | `v2_user_credentials.ldap_hash` → LDAP `userPassword` |

密码重置验证码为 6 位数字，仅保存 HMAC 摘要（10 分钟有效，最多尝试 5 次）。`cmd/convert-passwords` 工具可将 `go-ldap-admin` v1 的可逆 RSA 密码批量转换为上述单向哈希。

## 下一步阅读 {#next-steps}

- [安全模型](security.md)：了解这些密码学机制所对应的威胁模型。
- [日常运维指南](../deploy/operations.md)：查看生产环境中的密钥与证书轮换操作手册。
