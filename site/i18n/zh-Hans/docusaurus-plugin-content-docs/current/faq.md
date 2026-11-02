# 常见问题与故障排查

## 基础问题 {#general}

### Slurm Portal 会取代 Slurm 原生命令行工具吗？ {#does-slurm-portal-replace-slurms-native-cli-tools}

不会。作业调度仍由 Slurm 自身完成，用户依然使用 `sbatch`、`srun` 和 `squeue` 提交与管理作业。Slurm Portal 负责跨集群的**身份认证、权限治理与日常运维**：集中管理 POSIX 账号、用户组、RBAC 角色、SSH 证书及由此派生的 Slurm 记账关联（Association），同时提供实时集群监控、作业完成邮件通知，以及开箱即用的浏览器终端与文件管理器。

### 用户需要在个人电脑上安装专用客户端吗？ {#do-users-need-to-install-any-custom-client-software}

不需要，标准的 OpenSSH 客户端即可。用户只需每月从门户下载一次有效期 30 天的 SSH 证书与私钥即可像往常一样通过终端登录，也可以直接在浏览器中使用 Web 终端。

### 主服务（Master）需要主动连接集群内网吗？ {#does-the-master-need-inbound-network-access-to-the-clusters}

不需要。Master 从不主动向集群发起连接；所有同步与注册均由集群侧的 slave 代理和 Web 访问网关通过双向 TLS（mTLS）主动向外连接 Master。因此，集群可以部署在仅允许出站 HTTPS 流量的防火墙或 NAT 之后。

### 应该选择 Serverless 还是容器主服务？ {#should-i-choose-the-serverless-or-container-master}

如果您已经在使用 Cloudflare，希望免去服务器运维并以极低成本运行，请选择 **Serverless 主服务**；如果门户必须运行在机房自有硬件或单位内网环境中，请选择 **容器主服务**。两种方式运行完全相同的 Go 核心代码，功能也完全一致（仅前置的 Cloudflare Access 集成有所区别）。详见[部署总览](deploy/README.md)。

### 如果主服务暂时宕机，集群还能正常使用吗？ {#what-happens-if-the-master-goes-offline}

可以。每个集群都拥有独立的本地 OpenLDAP 目录与 SSH 信任文件：已同步的用户依然可以正常登录节点并提交 Slurm 作业，已签发的 SSH 证书在 30 天有效期内继续有效。只有新增账号或吊销证书等变更会等待主服务恢复后再同步。

### 可以在已有存量 LDAP 账号的集群上接入 Slurm Portal 吗？ {#can-i-adopt-slurm-portal-on-a-cluster-with-existing-ldap-accounts}

可以。Slave 代理绝不会修改或删除非门户托管的 LDAP 条目；对于门户中 LDAP 密码哈希为空的账号，slave 也会保留 OpenLDAP 中原有的 `userPassword` 属性不变。首次接入前，请务必先运行 [slave 离线计划模式](deploy/slaves.md#5-rehearse-then-start)，提前审阅首次同步将执行的全部 LDAP 操作。

### 如何从 go-ldap-admin v1 升级迁移？ {#how-do-i-migrate-from-go-ldap-admin-v1}

将 v2 主服务直接指向 `go-ldap-admin` v1 数据库副本即可：启动时会自动执行增量表结构迁移，并一次性导入旧版配置与历史数据。详见[从 go-ldap-admin v1 迁移](deploy/README.md#moving-from-go-ldap-admin-v1)。

## 账号与 SSH 登录 {#accounts-and-ssh-logins}

### 新创建的用户暂时无法登录集群 {#a-newly-created-user-cannot-log-in-yet}

门户中的变更通常在 1–2 分钟内同步到集群：slave 默认每 60 秒轮询一次，节点上的 SSSD 会按 `entry_cache_timeout`（推荐配置为 60 秒）缓存条目。可在登录节点执行 `sss_cache -E` 立即刷新缓存，并在控制节点执行 `journalctl -u slurm-portal-slave` 查看同步日志。

### 持有有效证书但 SSH 提示 `Permission denied (publickey)` {#ssh-returns-permission-denied-publickey-despite-a-valid-certificate}

在登录节点执行 `journalctl -u ssh` 查看具体原因：

| `sshd` 日志内容 | 原因与解决方法 |
| --- | --- |
| `revoked by file …/revoked.krl` | 该证书已被吊销，请在门户中重新签发新证书。 |
| `Error checking … in revoked keys file …: Permission denied` | `sshd` 无权读取 `revoked.krl`。请升级 slave 程序（早期版本在严格 `umask` 下写出了私有权限文件），或检查 NFS 信任目录的挂载权限。 |
| `Certificate invalid: expired` | 证书已超过 30 天有效期，请在**证书与凭据**页面重新签发。 |
| `Certificate … not signed by a trusted CA` | `sshd_config` 中的 `TrustedUserCAKeys` 未指向 `current/ssh/trusted-user-ca-keys.pub`，或签发该证书的 CA 已在门户中被禁用。 |

### 修改 `sshd_config` 后所有公钥均无法登录 {#nobody-can-log-in-with-ssh-keys-after-updating-sshdconfig}

如果 `RevokedKeys` 指向的文件路径不存在，OpenSSH 会拒绝**所有**公钥与证书登录。请执行 `ls /srv/slurm-portal-trust/current/ssh/revoked.krl` 检查文件是否存在以及 NFS 信任目录是否正常挂载。

### 提交作业报错 `Invalid account or account/partition combination` {#job-submission-fails-with-invalid-account-or-accountpartition-combination}

该用户尚未在 Slurm 中建立记账关联（Association）。关联由 slave 的触发器脚本（`slurm_trigger.py`）创建，请检查 slave 日志中的触发器报错信息。注意：全新创建的用户在首次执行触发器时，若 SSSD 尚未缓存该用户的 UID，可能会失败一次，slave 会在下一轮检查时自动重试。

## 集群状态与邮件通知 {#cluster-status-and-email-notifications}

### 集群状态页面为空或显示数据过期 {#cluster-status-pages-are-empty-or-stale}

- 请确认 `slave.yml` 中已设置 `current-status.enabled: true`，且运行 slave 的系统用户能够正常执行 `scontrol` 与 `squeue`。
- 浏览器通过预签名链接直接从对象存储读取实时状态 JSON。在 Cloudflare 部署下，若浏览器报错 `"Failed to fetch"`，说明 R2 存储桶的 CORS 规则未放行门户域名（详见 `deploy/worker/r2-cors.json`）；在非 443 端口的容器主服务下，请确认已在 `.env` 中设置包含端口号的 `PORTAL_URL`。

### 作业结束后未收到通知邮件 {#job-completion-emails-are-not-being-sent}

作业完成邮件需要同时满足以下条件：
1. 集群运行 Slurm 26.05+ 且已启用 `slurmrestd`；
2. 已部署生命周期钩子、观察者及 slave 的生命周期运行时（见[作业通知与 GPU 监控](deploy/optional.md#job-e-mail-and-gpu-usage)）；
3. 已在门户的**邮件服务**中配置可用服务商，并在**作业通知**中开启通知开关。

此外，只有实际运行时长超过 **60 秒**的作业才会触发通知邮件。

## Web 终端与文件管理 {#web-terminal-and-file-manager}

### 打开 Web 终端时提示 `"Failed to fetch"` {#opening-the-web-terminal-fails-with-failed-to-fetch}

浏览器会直接通过 HTTPS 连接登录节点上的 Web 访问网关。请确认网关前置的 Nginx 已在 CORS 映射中放行门户域名，且配置了受浏览器信任的 TLS 证书。

### 集群的 Web 访问状态显示为“未就绪（Not ready）” {#cluster-web-access-status-shows-not-ready}

说明网关在过去 3 分钟内未能成功向主服务注册心跳。请在登录节点执行 `curl -sk https://127.0.0.1:19934/readyz` 查看具体错误原因（常见原因为 `master_origin` 网络不可达或网关客户端证书指纹未加入主服务白名单）。

## Cloudflare 部署 {#cloudflare-deployment}

### 脚本调用 API 返回错误码 1010 {#cli-scripts-fail-with-http-403--error-1010}

Cloudflare 默认的 Bot 防护会拦截缺少常规 `User-Agent` 头的请求。请在脚本中显式设置 `User-Agent` 请求头，并附带 Cloudflare Access 服务令牌头（`cf-access-client-id` 与 `cf-access-client-secret`）。

### 修改数据后 `_sync` DNS TXT 记录未更新 {#the-sync-dns-txt-record-does-not-update-after-an-edit}

Worker 会在每次写操作完成后异步更新 DNS TXT 记录，并通过每 5 分钟一次的 Cron 任务兜底同步。请执行 `dig +short TXT _sync.portal.example.org` 并在门户的**运行诊断**页面检查 `CLOUDFLARE_API_TOKEN`、`CLOUDFLARE_ZONE_ID` 与 `CLOUDFLARE_RECORD_ID` 是否配置正确。

## Slurm {#slurm}

### `slurmctld` 启动失败并提示 `CLUSTER ID MISMATCH` {#slurmctld-exits-with-cluster-id-mismatch}

这是因为在 `slurmctld` 已经自动注册过集群之后，又手动执行了 `sacctmgr add cluster`，导致数据库生成了冲突的集群 ID。请执行 `sacctmgr -i delete cluster <name>` 删除记录后重启 `slurmctld`，让其自动重新注册。详见[记账服务](deploy/accounting.md)。

### 支持哪些 Slurm 版本？ {#which-slurm-versions-are-supported}

账号同步、SSH 证书、实时队列/节点状态以及 Web 终端支持所有主流 Slurm 版本（包括 Ubuntu 24.04 软件源自带的 Slurm 23.11）。作业完成邮件与按作业统计的 GPU 用量监控依赖 `slurmrestd` 的 `v0.0.45` 接口，因此需要 **Slurm 26.05 及以上版本**。详见 [Slurm 安装指南](deploy/slurm.md)。
