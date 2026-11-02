# 版本更新日志

## Slurm Portal 2.0 {#slurm-portal-20}

项目正式更名为 **Slurm Portal** 后的首个大版本发布——基于 `go-ldap-admin` 全面重构，专为多集群 Slurm HPC 环境打造。

**控制平面（Master）**

- **双运行时架构：** 支持在 Cloudflare（Workers、D1、R2、Cloudflare Access）上无服务器运行，或通过 Docker Compose（SQLite、RustFS S3 存储、Caddy）在自有主机上容器化部署，两端共用同一套 Go 核心代码。
- **身份管理与分级 RBAC：** 集中管理 POSIX 用户、主/附加 POSIX 组、树形组织架构及角色，内置基于等级（Rank）的防提权校验。
- **认证与凭据安全：** 支持本地密码、GitHub OAuth（支持按有序规则自动分配用户组与角色）及 Cloudflare Access 登录；支持邮箱验证码自助重置密码；密码仅保存单向哈希（门户使用 Argon2id，OpenLDAP 使用 SHA-512 crypt）。
- **内置 PKI 证书中心：** 内置 SSH 与 X.509 证书颁发机构（CA），签发 30 天有效期的 OpenSSH 用户证书，并向所有集群自动分发吊销列表（KRL 与 CRL）。
- **邮件中枢（Mail Hub）：** 支持 SMTP、自定义 HTTP REST 接口及多通道负载均衡邮件池（支持按每日配额、轮询或权重路由及自动故障切换），提供在线模板预览与分通道投递统计。
- **防篡改审计日志：** 基于 SHA-256 哈希链的只追加审计日志，支持容量上限控制与在线完整性校验。
- **模块化插件体系：** 内置实时集群与队列监控、近 7 天 GPU 资源统计、作业完成邮件、Web 终端与文件管理、跨集群文件传输、依赖拓扑诊断及 OpenTelemetry 链路追踪插件。

**集群代理（Cluster Agents）**

- **Slave 同步代理（`cmd/slave`）：** 在批量删除拦截与 CA 移除保护下将主服务快照同步至本地 OpenLDAP，以原子化不可变世代发布 SSH 信任文件，并按序执行本地触发器（管理 Slurm 关联账户与 NFSv4 主目录）。支持 `-plan` 离线预演模式。
- **Web 访问网关（`cmd/gateway`）：** 利用浏览器端生成的 5 分钟临时 WebCrypto SSH 证书，以当前登录用户身份提供在线终端、SFTP 文件浏览下载及跨集群 `rsync` 传输。
- **作业生命周期钩子与观察者（`cmd/slurm-lifecycle-hook`、`cmd/slurm-notify-observer`）：** 采用失败放行（Fail-open）设计的 Slurm Prolog/Epilog 通知器与只读 `slurmrestd` 代理，在不阻塞作业调度、不暴露 Slurm JWT 密钥的前提下采集作业结束事件与 NVML GPU 用量。

**从 go-ldap-admin v1 升级**

- v2 主服务可直接打开 v1 数据库，自动执行增量迁移并一次性导入旧版配置与历史记录。
- 提供 `cmd/migration-check` 离线迁移校验工具与 `cmd/convert-passwords` 单向密码哈希转换工具。
