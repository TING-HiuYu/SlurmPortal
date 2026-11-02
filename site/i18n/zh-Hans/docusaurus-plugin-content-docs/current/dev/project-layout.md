# 项目目录结构

本页提供代码仓库的目录导览，说明各目录与 Go 包的职责边界。Go 模块路径为 `github.com/TING-HiuYu/SlurmPortal`，每个包顶部均包含说明其职责的文档注释。

```text
.
├── cmd/                  Executable entrypoints (one subdirectory per binary, package main)
├── internal/             Private Go packages organized by architectural layer
├── migrations/           Embedded SQL schema migrations applied in order at startup
├── web/                  React 19 + Ant Design 6 single-page application
├── deploy/               Docker Compose stacks, Worker host scripts, systemd units, and cluster scripts
├── docs/                 Markdown source for the Developer and Deployment Wikis
├── site/                 Docusaurus documentation site and Simplified Chinese localization
└── .github/              GitHub Actions CI/Pages workflows and issue/PR templates
```

## `cmd/`：可执行程序 {#cmd-programs}

| 子目录 | 构建产物 | 说明 |
| --- | --- | --- |
| `worker` | Cloudflare Worker 使用的 `main.wasm` | 使用 `GOOS=js GOARCH=wasm` 编译，向 `deploy/worker/worker.js` 导出 `goV2Fetch`、`goV2Tick` 等入口 |
| `server` | 原生主服务（容器化部署使用） | 运行同一个 `internal/app`，基于 `database/sql` + SQLite，同时提供前端静态资源与双向 TLS 同步端口 |
| `slave` | 集群同步代理 | 支持持续守护运行（`-config`）、单次检查（`-once`）与离线预演（`-plan`） |
| `gateway` | Web 访问网关 | 部署在登录节点，提供浏览器终端、SFTP 与跨集群 `rsync` 传输 |
| `slurm-lifecycle-hook` | Slurm Prolog / Epilog 钩子 | 失败放行（Fail-open）设计，向 slave 的生命周期运行时上报作业开始与结束事件 |
| `slurm-notify-observer` | 只读 `slurmrestd` 代理 | 运行在控制节点，使 slave 无需持有 Slurm JWT 密钥即可读取作业详情 |
| `host-bridge` | 容器化 slave 的触发器主机桥 | 通过 SSH 在宿主机上运行 `slurm_trigger.py` |
| `migration-check` | 运维迁移校验工具 | 在数据库副本上演练迁移、验证 `PKI_MASTER_KEY` 可解密所有 CA，并导出测试快照 |
| `convert-passwords` | 旧版密码转换工具 | 将 v1 可逆密码批量转换为单向哈希 |

## `internal/`：内部核心包 {#internal-libraries}

**请求处理与基础设施**（建议从 `internal/app` 开始阅读）：

| 包路径 | 职责说明 |
| --- | --- |
| `app` | 组装整个应用：执行数据库迁移、初始化插件、挂载中间件与核心领域模块 |
| `httpx` | 强制权限声明的路由器、中间件链、严格 JSON 解析与响应输出 |
| `authn` | 身份认证：用户会话令牌（JWT）与机器客户端证书（`TLSPeer`、`BridgePeer`） |
| `authz` | 权限授权：路由权限模型、Casbin 角色策略评估与旧版策略导入 |
| `apperr` | 返回给客户端的统一结构化错误类型 |
| `config` | YAML 与环境变量配置加载及严格校验 |
| `audit` | SHA-256 哈希链审计日志及其 HTTP 中间件 |
| `telemetry` | 链路追踪与指标接口、缓冲上报器及带追踪的存储包装器 |

**核心业务模块：**

| 包路径 | 职责说明 |
| --- | --- |
| `domain/identity` | 用户、用户组、角色、POSIX UID/GID 分配与防提权等级校验 |
| `domain/auth` | 密码登录、令牌刷新与注销、修改密码及邮箱重置验证码 |
| `domain/pki` | 证书颁发机构（CA）与用户证书管理 |
| `mail` | 邮件中枢：服务商驱动（SMTP、REST）、模板渲染与投递统计 |
| `password` | Argon2id 与 SHA-512 crypt 密码哈希计算与恒定时间校验 |
| `secretbox` | 使用由 `PKI_MASTER_KEY` 派生的子密钥加密数据库中的敏感凭据 |
| `legacyimport` | 一次性导入 `go-ldap-admin` v1 存量数据 |

**存储层：**

| 包路径 | 职责说明 |
| --- | --- |
| `store` | `Store` 接口定义、`database/sql` 实现与数据库迁移执行器 |
| `store/d1` | 基于 JavaScript 桥的 Cloudflare D1 存储实现 |
| `store/storetest` | 两套存储实现共用的一致性测试套件 |
| `objstore` | 面向 R2 / S3 的轻量级 SigV4 客户端 |
| `statusreport` | 按集群与类型保存和查询最新上报状态 |
| `testutil` | 供单元测试使用的预迁移内存数据库与测试夹具 |

**插件体系：**

| 包路径 | 职责说明 |
| --- | --- |
| `plugin` | 插件注册表、`Host` 宿主对象、进程角色定义与插件间共享的 `Services` 注册表 |
| `plugins/all` | 通过 `!no_<name>` 编译标签链入所有内置插件 |
| `plugins/*` | 各内置插件的具体实现（详见[插件系统](plugins.md)） |

**集群同步与代理侧：**

| 包路径 | 职责说明 |
| --- | --- |
| `replication` | 快照数据结构、快照哈希计算、DNS TXT 发布器与快照下载接口 |
| `slave` | 同步状态机、OpenLDAP 对账写入、信任世代发布、触发器队列、状态采集与 `-plan` 模式 |
| `gateway` | Web 访问网关服务端：票据校验、SSH 会话池、SFTP 与跨集群传输 |
| `lifecycle/runtime` | Slave 内部的生命周期运行时及钩子配置 |
| `lifecycle/eventbus` | 有界内存事件总线 |
| `lifecycle/jobnotify` | 作业事件类型定义、`slurmrestd` 读取客户端与观察者代理 |
| `lifecycle/jobdoneemail` | 监听作业结束事件并上报主服务发送通知邮件 |
| `lifecycle/resources` | 周期性采样 GPU 用量并生成单作业资源摘要 |

## `migrations/` {#migrations}

`0000_legacy_base.sql` 至 `0016_*.sql` 定义了数据库表结构。其中 `0000` 为 `go-ldap-admin` v1 的原始表结构，因此 v1 数据库可直接无缝打开。所有迁移脚本按文件名顺序仅执行一次，且遵循纯增量原则。

## `web/` {#web}

基于 React 19 与 Ant Design 6、使用 Vite 构建的前端单页应用。

| 路径 | 内容说明 |
| --- | --- |
| `src/routes.jsx` | 所有页面路由、菜单项及其依赖的插件或权限声明 |
| `src/session.js` | 登录状态管理及从 `/api/v2/me/access` 获取的权限映射 |
| `src/api/client.js` | API 请求封装与令牌自动刷新 |
| `src/views/admin/` | 管理后台页面（用户、用户组、角色、PKI、邮件、通知、诊断、审计等） |
| `src/views/status/` | 集群节点状态、资源与作业队列、GPU 用量统计页 |
| `src/views/workspace/` | Web 终端与文件浏览器 |
| `src/components/` | 公共组件：整体布局、图表、模板编辑器与终端组件 |
| `src/i18n.js`、`src/locales/en.json` | 国际化支持（以中文作为源键名，英文翻译保存在 `en.json`） |
| `public/branding/` | 登录页与侧边栏展示的品牌图标 |

## `deploy/` {#deploy}

| 路径 | 内容说明 |
| --- | --- |
| `compose/master/` | 容器主服务编排文件（Caddy、master、RustFS）、`init.sh` 与 `add-cluster.sh` |
| `compose/slave/` | 容器化 slave 编排文件、`init.sh`、`slave.yml.example` 与演示用 OpenLDAP |
| `docker/` | `master.Dockerfile` 与 `slave.Dockerfile` |
| `worker/` | Serverless 主服务：Worker 外壳（`worker.js`、D1 与 SMTP 桥）、`build.sh`、`wrangler.example.toml` 与冒烟测试 |
| `certs/` | `issue.sh`：生成私有 CA 与双向 TLS 证书 |
| `slave/` | 原生部署 slave 使用的 systemd 服务单元 |
| `triggers/` | `slurm_trigger.py`、主机桥分发脚本与触发器配置示例 |
| `storage/` | 主目录 ACL 同步工具、信任目录挂载单元与计算节点 SSH 信任安装脚本 |
| `gateway/` | 网关配置示例、systemd 服务单元与 Nginx 配置示例 |
| `gateway/acme/` | 公网 HTTPS 证书自动续期脚本（Certbot + Cloudflare DNS-01） |
| `gateway/container/` | 将网关与 Nginx 打包在一起的容器镜像 |
| `lifecycle/` | Slurm Prolog / Epilog 包装脚本与观察者 systemd 服务单元 |

各部署组件如何配合使用请参阅[部署 Wiki](../deploy/README.md)。

## 历史兼容命名说明 {#names-you-will-still-see}

部分安装路径与内部常量保留了项目更名前的 `go-ldap-admin` 前缀（如 `/etc/go-ldap-admin/`、`/usr/local/libexec/go-ldap-admin/` 以及 HKDF 派生前缀），以便更名前部署的存量集群无需重装即可平滑升级。在无历史包袱的新组件中均统一使用 `slurm-portal` 命名；中心服务统一称为 **master**（旧配置中的 `main` 别名仍继续兼容）。
