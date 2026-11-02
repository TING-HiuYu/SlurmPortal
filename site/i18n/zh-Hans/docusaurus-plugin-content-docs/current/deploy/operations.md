# 日常运维指南

本页涵盖日常管理操作、备份恢复、密钥轮换以及常见故障排查流程。当系统出现异常时，请首先打开 **系统管理 → 运行诊断（Diagnostics）**（`/admin/diagnostics`）：该页面以拓扑图形式直观展示数据库、各集群上报、Web 网关与邮件服务商的实时健康状态。

## 日常管理操作 {#everyday-tasks}

| 运维任务 | 控制台入口与说明 |
| --- | --- |
| 创建用户、分配用户组与角色 | **用户管理（Users）**。新建用户通常在 1 分钟内出现在所有集群的 OpenLDAP 中。 |
| 禁用用户 | **用户管理** → 状态开关。禁用用户的同时会自动吊销其所有有效证书。 |
| 重置用户密码 | **用户管理** → **重置密码**（随机密码仅展示一次），或由用户在登录页点击 **忘记密码？** 通过邮箱验证码自助重置。 |
| 签发、轮换或吊销用户 SSH 证书 | **用户管理** → 证书操作，或由用户在 **证书与凭据** 页面自助操作。 |
| 创建或导入 CA | **证书颁发机构（Certificate authorities）** |
| 配置邮件服务商 | **邮件服务（Mail service）**：配置 SMTP、REST 或邮件池，支持测试发送与按服务商统计投递量。 |
| 配置作业、传输与密码验证码邮件模板 | **作业通知**、**传输通知**、**密码验证码**：支持在线编辑模板、实时预览与测试发送。 |
| 配置 GitHub OAuth 登录 | **OAuth 连接器**：每个 GitHub 应用对应一个连接器，按有序规则控制准入与自动分组。 |
| 查阅与校验审计日志 | **审计日志（Audit log）**；可通过 `GET /api/v2/audit/verify` 校验整条 SHA-256 哈希链。 |

## 变更同步到集群的完整链路 {#how-changes-reach-the-clusters}

1. 管理员保存变更后，主服务发布最新快照哈希：Serverless 主服务写入 DNS TXT 记录（`_sync.portal.example.org`），容器主服务在 `/sync/v2/snapshot-hash` 提供查询。
2. 各集群 slave 每 60 秒检查一次哈希，发现变化后下载最新快照。
3. Slave 更新本地 OpenLDAP 与信任文件，随后按序执行触发器（创建 Slurm 关联账户与主目录）。
4. 各节点上的 SSSD 按 `entry_cache_timeout`（推荐配置为 60 秒，SSSD 默认值为 90 分钟）缓存条目；执行 `sss_cache -E` 可立即清空缓存。

### 排查“门户变更未同步到集群” {#a-change-did-not-reach-a-cluster}

请按以下顺序逐段排查：

1. **快照哈希是否已发布？**
   - **SaaS 主服务：** 执行 `dig +short TXT _sync.portal.example.org`，确认返回 `v2:<哈希>` 且在修改后发生变化。若未更新，请检查 **运行诊断** 页面与 Worker 日志（Cron 任务也会每 5 分钟自动重发布一次）。
   - **容器主服务：** Slave 直接向主服务查询，请检查 `docker compose logs master`。
2. **Slave 是否处于就绪状态？** 在 slave 所在主机上：
   - 执行 `curl -s 127.0.0.1:8081/readyz` 查看最近一次同步时间与哈希；
   - 查看日志：`docker compose logs slave`（容器）或 `journalctl -u slurm-portal-slave`（systemd）。
   常见失败原因包括：
   - 客户端证书过期或更换后未在主服务更新指纹（同步端点返回 `403`）；
   - 触发安全防线拦截（单次删除用户过多，或试图移除受信 CA）；
   - OpenLDAP 拒绝写入。
3. **条目是否已写入 OpenLDAP？** 在 `slapd` 主机上执行 `ldapsearch -x -b <base> uid=<user>`。若 LDAP 中已有该用户，说明是登录节点的 SSSD 缓存尚未过期，执行 `sss_cache -u <user>` 即可。
4. **触发器是否成功执行？** 待执行的触发器事件按顺序保存在 slave 状态目录中；若某次投递失败，会阻塞后续事件直至修复。请查看 slave 日志定位失败命令并修复根因（权限、NSS、挂载或 Slurm），切勿直接删除队列文件（触发器脚本可安全重复执行）。

## 集群状态显示过期（Stale） {#cluster-status-looks-stale}

- 每张集群卡片都会显示数据采集时间，超过插件配置的 `stale-seconds` 后即标记为过期。
- 请检查 slave 的 `current-status` 配置段及日志中是否有上报被拒记录。
- 浏览器通过预签名链接直接从 R2 / S3 读取状态；若浏览器报 `"Failed to fetch"`，通常是 R2 存储桶的 CORS 规则未包含门户域名（`deploy/worker/r2-cors.json`）。

## Web 终端或文件管理异常 {#web-terminal-or-files-fail}

- **浏览器提示 `"Failed to fetch"`：** 网关前置的 Nginx CORS 映射必须放行门户域名。
- **运行诊断页面显示网关离线：** 网关会周期性向主服务注册心跳，请检查 `systemctl status web-access-gateway`、网关日志及其客户端证书。
- **SSH 登录被拒绝：** 登录节点的 `sshd` 必须信任 `<信任目录>/current/ssh/trusted-user-ca-keys.pub`，可执行 `sshd -T | grep -i trusteduser` 核对。

## 邮件投递排查 {#mail}

- 在 **邮件服务 → 测试** 中可向指定服务商发送测试邮件并直接查看上游返回的原始响应。
- 每次发送均按天、服务商与用途（作业、传输、密码验证码、测试）统计计数；邮件池的发送量计入实际完成投递的成员服务商名下。
- 收件人地址被拒不会在其他服务商上重复重试；连接或配额错误则会在开启故障切换时自动切往池内下一个成员。

## 密钥与证书轮换 {#keys-and-certificates}

| 轮换对象 | 操作步骤 |
| --- | --- |
| **会话签名密钥（`JWT_KEYS`）** | 在 `JWT_KEYS` 中追加新密钥并将 `JWT_ACTIVE_KID` 指向新密钥 ID，重新发布主服务；待旧令牌最长有效期过后移除旧密钥。 |
| **Slave 或网关客户端证书** | 签发新证书，先在主服务（`wrangler.toml` 或容器主服务的 `.env`）中更新指纹白名单并重新发布，随后替换代理节点上的证书文件。 |
| **网关票据签名密钥** | 先将新公钥加入所有网关的 `ticket_public_keys`，再更新主服务的 `PLUGIN_WEB_ACCESS_TICKET_KEY` 与 `web-access.ticket-key-id`。 |
| **用户 SSH 证书** | 有效期 30 天，用户可在 **证书与凭据** 页面自助续期（或由管理员轮换）；吊销记录会在下一次同步时通过 KRL 送达 `sshd`。 |
| **`PKI_MASTER_KEY`** | **请勿随意更换。** 数据库中封存的所有 CA 私钥与服务商密钥均依赖它解密，请务必做好离线备份。 |

## 备份与恢复 {#backups-and-recovery}

- **主服务数据库：** SaaS 模式可执行只读导出 `npx wrangler@4 d1 export <database> --remote --output backup.sql`，同时 D1 支持按时间点恢复（Time Travel）；容器模式请备份 `deploy/compose/master/data/master/`、`.env` 与 `secrets/`。
- **SSH 信任文件：** 每个世代均为不可变目录；如需紧急回滚，在信任共享目录上执行 `ln -sfn <上一世代目录> <信任目录>/current` 即可。
- **OpenLDAP 目录：** 执行高风险操作前先用 `slapcat -n 0` 与 `slapcat -n 1` 导出备份；主服务托管的条目随时可由 slave 根据最新快照重建。
- **主服务版本回滚：** SaaS 模式执行 `npx wrangler@4 rollback` 即可回退至上一版 Worker；容器模式切换至旧版本标签并执行 `docker compose up -d --build`。由于数据库迁移均为纯增量操作，旧版主服务可直接运行在新表结构上。

## 脚本化调用 API {#scripted-access}

在启用 Cloudflare Access 的部署下，脚本调用 API 时需携带服务令牌请求头 `cf-access-client-id` 与 `cf-access-client-secret`，同时必须设置常规的 `User-Agent` 请求头（否则会被 Cloudflare Bot 防护拦截并返回错误码 `1010`）。
