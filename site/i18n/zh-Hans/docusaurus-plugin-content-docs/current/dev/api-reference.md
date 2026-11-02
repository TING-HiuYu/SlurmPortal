# API 参考

<!-- 由 go run ./tools/apidoc 生成，请勿手工修改。 -->

本页列出启用全部内置插件时 master 注册的 93 个 HTTP 路由。每条路由均显式声明所需的访问权限级别：

- **公开**：无需认证即可调用。
- **已登录**：需在 `Authorization` 请求头中携带有效的 Bearer 会话令牌。
- **机器（mTLS）**：供 slave 代理或 Web 网关凭客户端证书调用，仅在同步端点上提供。
- **`对象:操作`**：调用者拥有的角色中至少有一个具备该策略授权（详见[身份与访问层](layer-access.md)）。

所有接口均返回 JSON：成功时为 `{"data": …}`，失败时为 `{"error": {"code", "message", "requestId"}}`。

## 核心

健康检查接口，以及用于渲染前端导航菜单的当前用户权限与插件映射。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/me/access` | 已登录 | 返回当前登录用户可用的插件与权限集合，用于构建前端导航菜单。 |
| `GET` | `/healthz` | 公开 | 存活检查接口；在同步端点上同样提供。 |

## 登录与密码

`internal/domain/auth` 包：用户名密码登录、令牌刷新与注销、密码修改及邮箱验证码重置。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `POST` | `/api/v2/auth/login` | 公开 | 使用用户名与密码登录并签发会话令牌（仅限 HTTPS）。 |
| `POST` | `/api/v2/auth/logout` | 已登录 | 吊销当前会话令牌。 |
| `GET` | `/api/v2/auth/password-reset` | 公开 | 查询是否已启用自助邮箱验证码重置密码功能。 |
| `POST` | `/api/v2/auth/password-reset/confirm` | 公开 | 凭有效的 6 位邮箱验证码设置新密码。 |
| `POST` | `/api/v2/auth/password-reset/request` | 公开 | 向匹配用户名或邮箱的账号发送 6 位密码重置验证码。 |
| `POST` | `/api/v2/auth/refresh` | 已登录 | 用当前令牌换取新会话令牌，同时吊销旧令牌 ID。 |
| `PUT` | `/api/v2/me/password` | 已登录 | 通过当前密码或邮箱验证码修改个人密码。 |
| `POST` | `/api/v2/me/password/code` | 已登录 | 向当前登录用户的邮箱发送修改密码验证码。 |
| `GET` | `/api/v2/password-mail/config` | `notification:read` | 获取密码验证码邮件的配置与模板。 |
| `PUT` | `/api/v2/password-mail/config` | `notification:update` | 更新密码验证码邮件的配置与模板。 |
| `POST` | `/api/v2/password-mail/preview` | `notification:read` | 使用示例变量渲染密码验证码邮件模板预览。 |
| `GET` | `/api/v2/password-mail/sample` | `notification:read` | 返回密码验证码邮件的示例数据与可用模板变量。 |
| `POST` | `/api/v2/password-mail/test` | `notification:update` | 通过配置的邮件服务商发送测试验证码邮件。 |

## 用户、用户组与角色

`internal/domain/identity` 包：所有变更操作均强制校验调用者的角色等级高于目标对象。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/groups` | `group:read` | 列出所有 POSIX 用户组与组织架构节点。 |
| `POST` | `/api/v2/groups` | `group:create` | 创建用户组；POSIX 组自动分配 GID 并支持配置主目录与 Shell 预设。 |
| `DELETE` | `/api/v2/groups/{id}` | `group:delete` | 删除指定的用户组。 |
| `GET` | `/api/v2/groups/{id}` | `group:read` | 获取指定用户组的详情及成员列表。 |
| `PATCH` | `/api/v2/groups/{id}` | `group:update` | 更新用户组的备注、主目录根路径或默认 Shell 预设。 |
| `POST` | `/api/v2/groups/{id}/members` | `group:update` | 在单次批处理操作中原子化添加与移除组成员。 |
| `GET` | `/api/v2/me` | 已登录 | 获取当前登录用户的个人资料与所属用户组。 |
| `PUT` | `/api/v2/me/shell` | 已登录 | 修改当前登录用户的默认登录 Shell。 |
| `GET` | `/api/v2/roles` | `role:read` | 按等级（Rank）顺序获取所有角色列表。 |
| `POST` | `/api/v2/roles` | `role:create` | 创建等级低于调用者自身最高等级的新角色。 |
| `DELETE` | `/api/v2/roles/{id}` | `role:delete` | 删除等级低于调用者的角色。 |
| `PATCH` | `/api/v2/roles/{id}` | `role:update` | 修改等级低于调用者的角色信息。 |
| `GET` | `/api/v2/roles/{id}/permissions` | `role:read` | 获取指定角色拥有的 `(对象, 操作)` 权限列表。 |
| `PUT` | `/api/v2/roles/{id}/permissions` | `role:update` | 替换角色的权限集合（调用者只能授予自身已拥有的权限）。 |
| `GET` | `/api/v2/users` | `user:read` | 分页列出用户，支持按关键字、状态和用户组筛选。 |
| `POST` | `/api/v2/users` | `user:create` | 创建用户账号，分配 POSIX UID、主 POSIX 组、附加组及角色。 |
| `DELETE` | `/api/v2/users/{id}` | `user:delete` | 删除等级低于调用者的用户，并同步吊销其所有有效证书。 |
| `GET` | `/api/v2/users/{id}` | `user:read` | 获取指定用户的详细资料、所属组与角色。 |
| `PATCH` | `/api/v2/users/{id}` | `user:update` | 修改等级低于调用者的用户资料、用户组或角色。 |
| `POST` | `/api/v2/users/{id}/password-reset` | `user:update` | 为用户生成随机重置密码并仅返回一次。 |
| `PUT` | `/api/v2/users/{id}/status` | `user:update` | 启用或禁用用户账号；禁用时自动吊销其有效证书。 |

## 证书

`internal/domain/pki` 包：管理 SSH 与 X.509 CA、用户证书及吊销记录；CA 私钥永不离开 master。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/me/certificate` | 已登录 | 查看当前登录用户持有的有效证书信息。 |
| `POST` | `/api/v2/me/certificate` | 已登录 | 为当前登录用户签发新证书与私钥（私钥仅在响应中返回一次）。 |
| `GET` | `/api/v2/pki/cas` | `pki:read` | 列出所有证书颁发机构（绝不返回私钥内容）。 |
| `POST` | `/api/v2/pki/cas` | `pki:create` | 生成或导入 SSH / X.509 CA，并使用 `PKI_MASTER_KEY` 加密封存私钥。 |
| `POST` | `/api/v2/pki/cas/{id}/revocations` | `pki:delete` | 在指定 CA 下吊销对应序列号的证书。 |
| `DELETE` | `/api/v2/users/{id}/certificate` | `pki:delete` | 吊销目标用户的当前有效证书。 |
| `POST` | `/api/v2/users/{id}/certificate` | `pki:create` | 在指定 CA 下为目标用户签发首张证书。 |
| `POST` | `/api/v2/users/{id}/certificate/rotate` | `pki:update` | 轮换用户证书并自动吊销旧证书序列号。 |

## 审计

`internal/audit` 包：查询并校验 SHA-256 哈希链审计日志。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/audit` | `audit:read` | 分页查询审计日志记录。 |
| `GET` | `/api/v2/audit/verify` | `audit:read` | 遍历校验 SHA-256 审计哈希链，若存在篡改或断裂则返回首条异常记录。 |

## 邮件中心

`internal/mail` 包：配置命名的邮件服务商并统计投递指标。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/mail/kinds` | `mail:read` | 列出当前构建支持的邮件服务商类型（`smtp`、`rest`、`pool`）。 |
| `GET` | `/api/v2/mail/providers` | `mail:read` | 列出已配置的邮件服务商（密钥字段已脱敏）。 |
| `DELETE` | `/api/v2/mail/providers/{name}` | `mail:update` | 删除指定的邮件服务商。 |
| `PUT` | `/api/v2/mail/providers/{name}` | `mail:update` | 创建或更新邮件服务商，并加密封存其凭据。 |
| `POST` | `/api/v2/mail/providers/{name}/test` | `mail:update` | 通过指定邮件服务商发送测试邮件。 |
| `GET` | `/api/v2/mail/usage` | `mail:read` | 按天、服务商与业务用途查询邮件投递统计。 |

## 集群同步

`internal/replication` 包：仅在同步主机上提供的机器认证接口。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/sync/v2/snapshot` | 机器（mTLS） | 下载完整的身份与信任快照（响应头 `X-Data-Hash` 携带其 SHA-256 摘要）。 |
| `GET` | `/sync/v2/snapshot-hash` | 机器（mTLS） | 仅返回当前快照的 SHA-256 摘要，供未启用 DNS TXT 轮询的部署使用。 |

## 插件：cloudflare-access

`internal/plugins/cfaccess` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/auth/access` | 公开 | 检查当前请求是否携带已验证的 Cloudflare Access 身份。 |
| `POST` | `/api/v2/auth/access/session` | 公开 | 使用已验证的 Cloudflare Access JWT 换取门户会话令牌（仅限 HTTPS）。 |

## 插件：cluster-status

`internal/plugins/clusterstatus` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/cluster-status` | 已登录 | 获取所有集群的最新节点状态汇总。 |
| `POST` | `/sync/v2/cluster-status` | 机器（mTLS） | 接收 slave 代理上报的集群节点状态。 |

## 插件：current-status

`internal/plugins/currentstatus` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/current-status/links` | 已登录 | 获取各集群实时状态对象的短期预签名 S3/R2 读取链接。 |
| `PUT` | `/sync/v2/current-status` | 机器（mTLS） | 接收 slave 代理上报的综合实时状态快照并条件写入 R2/S3。 |

## 插件：diagnostics

`internal/plugins/diagnostics` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/diagnostics` | `diagnostics:read` | 返回数据库、集群同步代理、Web 网关与邮件服务商的实时健康诊断报告。 |

## 插件：job-notification

`internal/plugins/jobnotification` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/notification/config` | `notification:read` | 获取作业完成邮件通知的配置与模板。 |
| `PUT` | `/api/v2/notification/config` | `notification:update` | 保存作业完成邮件通知的配置与模板。 |
| `POST` | `/api/v2/notification/preview` | `notification:read` | 使用示例作业数据渲染作业邮件模板预览。 |
| `GET` | `/api/v2/notification/sample` | `notification:read` | 获取最近一次真实完成作业的数据（或内置示例）及可用模板变量。 |
| `POST` | `/api/v2/notification/test` | `notification:update` | 发送测试作业完成邮件。 |
| `POST` | `/sync/v2/job-completions` | 机器（mTLS） | 接收 slave 代理上报的已完成作业事件并向作业所有者发送通知邮件。 |

## 插件：mail-pool

`internal/plugins/mailpool` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/mail-pool/usage` | `mail:read` | 查询各邮件池内成员服务商在当前周期的配额使用情况。 |

## 插件：oauth

`internal/plugins/oauth` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/oauth/authorize/{id}` | 公开 | 发起 OAuth 登录：绑定防伪 state Cookie 并重定向至身份提供商。 |
| `GET` | `/api/v2/oauth/callback/{provider}` | 公开 | 处理 OAuth 回调：匹配连接器规则，创建或关联用户并签发会话令牌。 |
| `GET` | `/api/v2/oauth/connectors` | `oauth:read` | 列出所有 OAuth 连接器及其有序准入规则。 |
| `POST` | `/api/v2/oauth/connectors` | `oauth:create` | 创建 OAuth 连接器。 |
| `GET` | `/api/v2/oauth/connectors/public` | 公开 | 列出登录页展示的已启用 OAuth 登录连接器。 |
| `DELETE` | `/api/v2/oauth/connectors/{id}` | `oauth:delete` | 删除指定的 OAuth 连接器。 |
| `PUT` | `/api/v2/oauth/connectors/{id}` | `oauth:update` | 更新 OAuth 连接器及其准入规则。 |

## 插件：resource-monitor

`internal/plugins/resourcemonitor` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/resource-monitor/my-stats` | 已登录 | 获取当前登录用户近 7 天的 GPU 资源使用统计。 |
| `GET` | `/api/v2/resource-monitor/stats` | `resource-monitor:read` | 获取全站近 7 天按天与按用户汇总的 GPU 资源使用统计。 |
| `POST` | `/sync/v2/resource-summaries` | 机器（mTLS） | 接收 slave 代理上报的单作业 GPU 使用摘要。 |

## 插件：resource-status

`internal/plugins/resourcestatus` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/resource-status` | 已登录 | 获取各集群最新的 CPU/内存/GPU 容量与作业队列状态。 |
| `POST` | `/sync/v2/resource-status` | 机器（mTLS） | 接收 slave 代理上报的集群资源与作业队列状态。 |

## 插件：web-access

`internal/plugins/webaccess` 包。

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| `GET` | `/api/v2/web-access/clusters` | 已登录 | 列出当前用户有权打开 Web 终端或文件管理器的集群。 |
| `GET` | `/api/v2/web-access/config` | `web-access:read` | 获取各集群的 Web 访问网关配置与注册状态。 |
| `PUT` | `/api/v2/web-access/config` | `web-access:update` | 保存各集群的 Web 访问网关连接参数与允许访问的角色。 |
| `GET` | `/api/v2/web-access/keys` | `web-access:read` | 返回供网关校验会话票据的 Ed25519 公钥列表。 |
| `POST` | `/api/v2/web-access/ssh-certificate` | 已登录 | 为浏览器端 WebCrypto 生成的临时公钥签发 5 分钟有效期的 SSH 用户证书。 |
| `POST` | `/api/v2/web-access/ticket` | 已登录 | 签发绑定用户、集群与操作权限范围的 5 分钟 Ed25519 签名网关票据。 |
| `GET` | `/api/v2/web-access/transfer-notification` | `web-access:read` | 获取跨集群文件传输完成邮件的配置与模板。 |
| `PUT` | `/api/v2/web-access/transfer-notification` | `web-access:update` | 保存跨集群文件传输完成邮件的配置与模板。 |
| `POST` | `/api/v2/web-access/transfer-notification/preview` | `web-access:read` | 使用示例数据渲染文件传输邮件模板预览。 |
| `GET` | `/api/v2/web-access/transfer-notification/sample` | `web-access:read` | 获取最近一次文件传输事件（或示例）及可用模板变量。 |
| `POST` | `/api/v2/web-access/transfer-notification/test` | `web-access:update` | 发送测试文件传输完成通知邮件。 |
| `POST` | `/sync/v2/web-access/events` | 机器（mTLS） | 接收网关上报的跨集群 `rsync` 传输完成事件。 |
| `POST` | `/sync/v2/web-access/register` | 机器（mTLS） | 接收 Web 访问网关的心跳注册及其支持的功能列表。 |
