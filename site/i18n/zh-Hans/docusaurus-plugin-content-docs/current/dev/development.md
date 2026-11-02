# 本地开发指南

本页介绍本地开发环境搭建、各组件启动方式、测试命令以及代码库遵循的工程规范。

## 环境要求 {#prerequisites}

| 工具 | 版本 | 用途 |
| --- | --- | --- |
| Go | 1.26（见 `go.mod`） | 编译与测试 `cmd/` 和 `internal/` 下的所有 Go 代码 |
| Node.js | 22 | 构建前端应用及运行 Worker 桥接层测试 |
| Python | 3.12（含 `PyYAML`） | 运行 `deploy/` 下的集群脚本及其单元测试 |
| OpenLDAP `slapd` | 可选 | 运行 `make ldap-it`（针对真实 `slapd` 的 slave 集成测试） |
| Wrangler | 4（通过 `npx` 调用） | 构建与发布 Cloudflare Worker |

## 在本地启动门户 {#run-the-portal-locally}

在本地以 `standalone` 模式启动原生主服务（使用 `./data` 目录下的 SQLite 数据库）：

```sh
mkdir -p data
JWT_KEYS="dev:$(openssl rand -base64 32)" JWT_ACTIVE_KID=dev \
PKI_MASTER_KEY="$(openssl rand -base64 32)" \
BOOTSTRAP_ADMIN_USERNAME=admin BOOTSTRAP_ADMIN_PASSWORD='change-me-now-123' \
SYSTEM_ROLE=standalone HTTP_LISTEN=127.0.0.1:8080 \
go run ./cmd/server
```

- 所有配置项均可通过环境变量传入，也可通过 `-config file.yml` 叠加一份 YAML 配置文件。
- 默认 SQLite 数据库路径为 `data/slurm-portal.db`（可通过 `DB_DSN` 修改）。
- 初始管理员账号仅在首次启动时创建，创建完成后即可移除 `BOOTSTRAP_ADMIN_PASSWORD`。
- 设置 `PLUGINS_ENABLED` 可启用扩展插件，例如 `PLUGINS_ENABLED=diagnostics,job-notification`。

随后在另一个终端启动前端开发服务器：

```sh
cd web
npm ci
API_ORIGIN=http://127.0.0.1:8080 npm run dev    # opens http://localhost:5173
```

`API_ORIGIN` 默认指向 `http://127.0.0.1:8080`，会将前端发起的 `/api/*` 请求代理至本地主服务。

如需在单机上完整体验容器化部署（含主服务、S3 存储、slave 与演示用 OpenLDAP），请按[容器主服务](../deploy/master-container.md)指南以 `localhost` 作为主机名初始化，再参阅[在容器中运行 Slave](../deploy/slaves.md#in-a-container)。

### 常用环境变量配置 {#settings}

`internal/config/config.go` 定义了全部配置项，最常用的变量如下：

| 环境变量 | 含义说明 |
| --- | --- |
| `SYSTEM_ROLE` | 进程角色：`standalone`、`master`、`slave` 或 `gateway` |
| `HTTP_LISTEN`、`CORS_ORIGINS`、`TRUSTED_IP_HEADER` | HTTP 监听地址、允许的浏览器跨域源、反向代理传递客户端 IP 的请求头 |
| `STATIC_DIR` | （仅 `cmd/server`）直接托管前端构建产物（`web/dist`）的本地目录 |
| `BEHIND_TLS_PROXY` | （仅 `cmd/server`）信任前置 TLS 反向代理传来的 `X-Forwarded-Proto: https`（仅当监听端口不直接对外暴露时启用） |
| `SYNC_LISTEN`、`SYNC_CERT_FILE`、`SYNC_KEY_FILE`、`SYNC_CLIENT_CA_FILE` | （仅 `cmd/server`）供 slave 与网关连接的双向 TLS 监听地址与证书配置 |
| `DB_DSN` | 原生 SQLite 数据库文件路径或连接串 |
| `JWT_KEYS`、`JWT_ACTIVE_KID` | 会话令牌签名密钥列表（`kid:base64,kid2:base64`）及当前用于签名的 `kid` |
| `PKI_MASTER_KEY` | 32 字节随机数的 Base64，用于加密封存 CA 私钥与服务商凭据 |
| `AUTH_ISSUER`、`AUTH_AUDIENCE` | 会话令牌的 `iss` 与 `aud` 声明值 |
| `LDAP_BASE_DN`、`LDAP_USER_DN` | 快照中输出的默认 LDAP 目录后缀与用户分支 |
| `AUDIT_ENABLED`、`AUDIT_MAX_RECORDS` | 审计日志开关及最大保留记录数 |
| `PLUGINS_ENABLED`、`PLUGINS_CONFIG` | 启用的插件列表与插件配置（详见[插件系统](plugins.md)） |
| `CLUSTERS` | 已注册集群的 JSON 数组（`{id, name, fingerprint}`） |
| `SYNC_CLIENT_FINGERPRINTS` | 允许访问 `/sync/*` 的客户端证书指纹列表 |
| `SYNC_DNS_NAME`、`CLOUDFLARE_ZONE_ID`、`CLOUDFLARE_RECORD_ID`、`CLOUDFLARE_API_TOKEN` | 发布快照哈希所用的 Cloudflare DNS TXT 记录配置 |
| `TELEMETRY_SAMPLE_RATIO` | 遥测插件记录新建链路追踪的采样比例 |

## 运行测试套件 {#tests}

```sh
make check      # runs vet, test, js, web, wasm, and govulncheck + npm audit
make vet        # verifies gofmt formatting and runs go vet
make test       # runs all Go unit tests with the race detector enabled
make js         # runs Cloudflare Worker bridge unit tests (node --test)
make web        # runs Vitest unit tests and builds the production React bundle
make wasm       # verifies that all internal packages compile for GOOS=js GOARCH=wasm
make ldap-it    # runs the slave integration test against a real slapd binary (set SLAPD and SLAPD_SCHEMA)
```

`deploy/` 目录下的集群脚本同样配有 Python 单元测试：

```sh
for t in $(git ls-files 'deploy/*test_*.py'); do
  (cd "$(dirname "$t")" && python3 -m unittest -q "$(basename "$t" .py)")
done
```

运行 Python 测试前请确保 `umask 022`（TLS 部署脚本会严格拒绝组可写文件，测试夹具文件权限受当前 `umask` 影响）。

GitHub Actions CI（`.github/workflows/ci.yml`）会在每次推送至 `main` 及每个 Pull Request 中自动运行 Go 测试、前端测试、脚本测试、Compose 校验、API 文档检查与文档站点构建。

### 编写测试准则 {#writing-tests}

- 使用 `internal/testutil` 创建带预设夹具的内存迁移数据库。
- 依赖新增 SQL 行为的功能必须在 `store/storetest` 中补充测试用例，确保 SQLite 与 D1 两套实现同时通过。
- 使用 `objstore/objstoretest` 离线模拟 R2 / S3 对象存储。
- 保持单元测试完全离线；针对真实云资源（R2、Cloudflare Access）的测试必须通过环境变量显式开启，默认自动跳过。

## 编码规范 {#conventions}

- **每条路由必须显式声明权限：** 管理接口统一使用 `authz.Policy(object, action)`，遗漏权限声明会在启动时直接 panic。
- **统一使用 `apperr` 返回错误：** 处理器返回 `apperr.BadRequest(...)`、`apperr.New(status, code, msg)` 或预定义错误；底层内部原因通过 `.Wrap(err)` 包装，仅记录在服务端日志中，绝不返回给客户端。
- **表结构变更必须新增迁移文件：** 新增 `migrations/00NN_name.sql`，语句之间用 `--;` 分隔，严禁修改已应用的迁移文件，且 SQL 必须同时兼容 SQLite 与 D1。
- **关注云端运行成本：** 优先使用 `EXISTS` / `LIMIT 1` 替代 `COUNT(*)`，确保高频查询命中索引，避免单次请求或冷启动全表扫描，并将周期性机器上报标记为 `Routine`。
- **数据库中严禁明文保存密钥：** 使用 `Host.Secrets` 加密保存，或通过 `env:NAME` 从环境变量读取。
- **代码注释：** 每个包均须包含包级注释；所有导出的类型与函数均须包含英文文档注释，说明其功能、边界条件与副作用。
- **代码格式化：** Go 代码使用 `gofmt`；前端新增文案以中文写入 `t("…")` 并在 `web/src/locales/en.json` 中补充英文翻译。

## 分模块调试提示 {#working-on}

### 调试 Cloudflare Worker {#the-worker}

`deploy/worker/build.sh` 负责构建 `dist/main.wasm` 与 `web/dist`。`deploy/worker/` 下的 JavaScript 仅负责桥接平台绑定，所有业务逻辑均应保留在 Go 中，桥接层行为由 `*.test.mjs` 覆盖测试。

### 调试 Slave 代理 {#the-slave}

```sh
go run ./cmd/slave -config slave.yml -once      # run a single sync cycle and exit
go run ./cmd/slave -plan -snapshot snap.json -ldif cluster.ldif -trust-dir trust …   # offline dry-run rehearsal
```

可使用 `cmd/migration-check -snapshot-out` 从数据库副本导出快照文件。注意配置文件权限必须为 `0600`。

### 调试 Web 访问网关 {#the-gateway}

网关在生产环境中连接信任门户 CA 的登录节点 `sshd`。在本地开发时，可直接运行 `internal/gateway` 中的单元测试（测试内嵌了真实的 Go SSH 服务端）。

## 提交与 Pull Request 规范 {#commits-and-pull-requests}

- 从 `main` 分支拉取新分支，每个 Pull Request 聚焦单一主题。
- Commit 首行采用 `<模块>: <变更说明>` 格式（例如 `slave: refuse a plan that drops a trusted CA`）。
- 提交前请确保 `make check` 全部通过，并在 PR 描述中说明集群或云端相关改动的验证方式。
