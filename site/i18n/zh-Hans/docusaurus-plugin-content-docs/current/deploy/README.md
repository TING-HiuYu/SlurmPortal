# 部署总览

本 Wiki 带您从全新的 **Ubuntu 24.04 LTS** 服务器起步，一步步搭建一套由 Slurm Portal 统一接管的生产级 Slurm 集群：在门户中创建的用户与用户组会自动同步至集群 OpenLDAP，用户凭门户 CA 签发的短期 SSH 证书登录节点，在各自的 Slurm 记账账户下提交作业，并可直接通过浏览器使用交互式终端与文件管理器。

本指南中的每一条命令均已在全新的 Ubuntu 24.04 主机上实际执行验证。涉及站点特定硬件或公网证书的步骤（如物理 NVIDIA GPU 或公网 ACME 证书）均已在对应章节中明确标注。

## 系统组成 {#the-pieces}

一套 Slurm Portal 部署由一个中心的 **Master（主服务）** 控制平面与每个集群一个 **Slave（从属代理）** 组成：

| 组件 | 核心职责 | 运行位置 |
| --- | --- | --- |
| **Master（主服务）** | 门户控制平面：Web 应用、REST API、身份数据库、SSH/X.509 CA 与审计日志 | Cloudflare（Serverless）或自有 Linux 主机（Docker Compose） |
| **Slave（集群代理）** | 将主服务快照同步至集群本地 OpenLDAP、发布 SSH 信任文件、运行触发器并上报状态 | 每个集群部署一台（`ctl`），仅需发起出站 HTTPS 连接 |
| **Web 访问网关** *(可选)* | 浏览器终端、SFTP 文件管理与跨集群 `rsync` 传输 | 集群登录节点（`login`） |
| **生命周期钩子与观察者** *(可选)* | 作业结束邮件通知与单作业 GPU 资源用量采集 | Slurm 控制节点（`ctl`） |

主服务支持两种运行方式，集群侧的配置流程在两种方式下完全相同：

| 对比维度 | [Serverless 主服务](master-saas.md) | [容器主服务](master-container.md) |
| --- | --- | --- |
| 运行环境 | Cloudflare：Worker、D1、R2 与 Cloudflare Access | 自有主机：Docker Compose，包含主服务（SQLite）、S3 存储（RustFS）与 Caddy |
| 运维职责 | 仅需维护 Cloudflare 配置与密钥 | 维护一台 Linux 主机及其系统更新与本地备份 |
| 登录安全边界 | 前置 Cloudflare Access 加上门户自身登录 | Caddy 自动 HTTPS 加上门户自身登录（密码、GitHub OAuth） |
| Slave 感知变更方式 | 查询 `_sync` DNS TXT 记录（空闲集群零 Worker 请求） | 每分钟向主服务发起一次轻量级 mTLS HTTPS 查询 |

## 部署路线图 {#the-path}

| 步骤 | 指南页面 | 预计用时 |
| --- | --- | --- |
| 1 | [站点规划](plan.md)：主机角色、域名解析、防火墙端口与 UID/GID 号段 | 15 分钟 |
| 2 | [Munge](munge.md)：配置集群共享认证密钥 | 5 分钟 |
| 3 | [Slurm](slurm.md)：构建 Slurm 26.05 软件包，部署控制节点与计算节点 | 30 分钟 |
| 4 | [记账服务](accounting.md)：部署 MariaDB 与 `slurmdbd` | 10 分钟 |
| 5 | [slurmrestd](slurmrestd.md)：配置基于 JWT 认证的 Slurm REST API | 10 分钟 |
| 6 | [OpenLDAP](openldap.md)：搭建启用 TLS 的目录服务 | 15 分钟 |
| 7 | [SSSD 与 SSH](sssd-ssh.md)：节点身份解析与计算节点登录管控 | 10 分钟 |
| 8 | [共享主目录与信任文件](shared-home.md)：配置 NFSv4 共享存储 | 15 分钟 |
| 9 | 部署主服务：[Serverless（Cloudflare）](master-saas.md)或[容器（Docker Compose）](master-container.md) | 25 分钟 |
| 10 | [门户初始化配置](portal-setup.md)：配置 SSH CA、POSIX 组预设、邮件服务与 OAuth | 10 分钟 |
| 11 | [集群 Slave 代理](slaves.md)：将集群接入主服务并启用账户触发器 | 20 分钟 |
| 12 | [作业通知与 GPU 监控](optional.md)及[Web 访问网关](gateway.md) *(可选)* | 45 分钟 |
| 13 | [端到端验证](verify.md)：使用测试账号完成全链路验收 | 10 分钟 |

如果您已有正在运行的 Slurm 集群，可直接从**第 6 步**（[OpenLDAP](openldap.md)）开始；如果集群已经配置好 OpenLDAP 与 SSSD，可直接从**第 9 步**开始。

## 机器身份认证 {#machine-identity}

Slave 代理与 Web 访问网关使用 X.509 客户端证书向主服务证明身份，主服务通过证书的 SHA-256 指纹识别并绑定对应集群：

- **Serverless 主服务：** 客户端证书由 Cloudflare 控制台的客户端证书 CA 签发。
- **容器主服务：** 由 `deploy/certs/issue.sh` 维护本地私有 CA（通过 `init.sh` 与 `add-cluster.sh` 自动调用）。

当轮换客户端证书时，其 SHA-256 指纹也会随之改变，请在同一次变更中同步更新主服务上的指纹配置。

## 从 go-ldap-admin v1 迁移 {#moving-from-go-ldap-admin-v1}

在 `go-ldap-admin` v1 数据库上启动 v2 主服务时，程序会自动：

- 应用增量数据库表结构迁移（`0001`–`0016`）；
- 一次性导入 v1 的 Web 访问设置、作业通知设置与 GPU 作业历史；
- 直接从 v1 原有数据表中读取用户、用户组、角色、CA 与证书。

正式割接前，请先使用 `cmd/migration-check` 在数据库副本上演练迁移并确认 `PKI_MASTER_KEY` 能够正常解密每个 CA；同时在各集群首次同步前运行 [slave 离线计划模式](slaves.md#5-rehearse-then-start)，确认输出为 `"safe": true`。

## 后续运维 {#after-that}

日常管理、数据备份、密钥轮换与故障排查请参阅[日常运维指南](operations.md)。
