# 分层：存储层

主服务的所有持久化状态均保存在一个关系型数据库中（Serverless 模式使用 Cloudflare D1，容器模式使用本地 SQLite）。精简的 `store.Store` 接口屏蔽了底层差异，使所有领域模块与插件只需编写一套数据访问代码。

**相关代码包：** `internal/store`、`internal/store/d1`、`internal/store/storetest`、`migrations`、`internal/objstore`（实时状态对象存储）与 `internal/statusreport`。

## 存储接口（`store.Store`） {#the-interface}

```go
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (Rows, error)
	Exec(ctx context.Context, query string, args ...any) (Result, error)
}

type Store interface {
	Querier
	// Batch executes all statements atomically: all commit or none do.
	Batch(ctx context.Context, stmts []Statement) ([]Result, error)
	Close() error
}
```

- `store.QueryRow(ctx, q, query, args, &dest…)` 读取单行记录，未找到时返回 `store.ErrNoRows`。
- 接口层不设跨多次调用的交互式事务（`BEGIN` … `COMMIT`），需要将多条修改原子化提交时统一放入单个 `Batch`。这是 Cloudflare D1 支持的唯一原子执行单元，也保证了两种后端语义完全等价。
- 所有 SQL 语句均采用标准 SQLite 方言（D1 底层即为 SQLite）。

## 两种后端实现 {#implementations}

| 对比维度 | `store.SQL`（原生容器） | `d1.Store`（Cloudflare Worker） |
| --- | --- | --- |
| 底层驱动 | `database/sql` + `modernc.org/sqlite`（纯 Go，无需 CGo） | JavaScript 桥 `goD1Bridge` → D1 绑定 `DB` |
| `Batch` 实现 | 单个 SQL 事务 | 单次 D1 `batch` 调用（每次最多 100 条语句） |
| 连接管理 | 单连接（`SetMaxOpenConns(1)`），容器镜像默认开启 WAL 模式 | 由 Cloudflare 托管 |
| 二进制 Blob | 原生字节切片 | 跨桥传输时编码为 `{"blob": base64}`（`d1-bridge.js`） |

`store/storetest.Run(t, store)` 是两套实现都必须通过的一致性测试套件，覆盖数据类型、`NULL` 值、二进制 Blob、失败自动回滚的 `Batch` 以及 `ErrNoRows`。JavaScript 桥逻辑由 `deploy/worker/d1-bridge.test.mjs` 独立测试。

## 数据库迁移（Migrations） {#migrations}

表结构变更以 SQL 文件形式保存在 `migrations/` 目录下，编译期内嵌至二进制（`migrations.FS`），并在启动时由 `store.Migrate` 自动按序执行：

- 每个迁移文件按文件名顺序仅执行一次，执行记录保存在 `v2_schema_migrations` 中。
- 单个文件内的多条 SQL 语句使用仅含 `--;` 的独立行分隔。
- **纯增量原则：** 严禁修改或删除已应用的迁移文件；新增变更请添加 `00NN_description.sql`。由于只做增量扩展，旧版本程序可以直接运行在已升级的表结构之上，从而保证版本回滚安全。
- **并发冷启动安全：** 在 Cloudflare Worker 上，多个 Isolate 可能同时启动；迁移与旧版数据导入逻辑均具备幂等性，可安全并发执行。

完整的表结构说明请参阅[数据模型](data-model.md)。

## 对象存储（Object Storage） {#object-storage}

高频更新的集群实时状态不写入关系型数据库。`internal/objstore` 实现了一个面向单存储桶的轻量级 S3 客户端（AWS SigV4 签名）：

- 在 Cloudflare 上连接 R2，在容器部署中通过 `endpoint` 连接任意兼容 S3 的存储服务（默认使用 RustFS）。
- 支持带 `If-Match` / `If-None-Match` 的条件写入（`Put`），确保乱序晚到的旧报告绝不会覆盖已经写入的新状态。
- 通过 `PresignGET` 为浏览器生成短期预签名链接（默认 15 分钟有效），使浏览器直接从对象存储读取监控数据，无需消耗主服务算力。

`objstore/objstoretest` 提供内存版 S3 测试服务端；可选集成测试 `TestLiveR2`（`internal/plugins/currentstatus/live_test.go`）则可针对真实 R2 存储桶执行完整验证。

## 面向成本优化的查询准则 {#cost-aware-queries}

在 Cloudflare 上，D1 按读写行数计费，且 Worker Isolate 每小时可能冷启动多次。本仓库的所有数据库代码严格遵循以下准则：

- 仅需判断是否存在记录时，使用 `EXISTS` 或 `LIMIT 1` 代替 `COUNT(*)`。
- 确保每条高频查询均命中索引（包括部分索引），严禁在单次请求或冷启动路径上执行全表扫描。
- 将周期性机器上报路由标记为 `Routine`，使其仅在失败时写入审计日志。
- 实时状态走对象存储，静态文件走 Workers Static Assets（`run_worker_first` 仅拦截 `/api/*`、`/sync/*` 与 `/healthz`）。
- 仅在发生数据写入后或 5 分钟定时任务中发布快照哈希，绝不在普通读请求中触发发布。

在修改查询前后，可使用 `wrangler d1 insights <database>` 及 Cloudflare 分析面板对比实际读写行数。

## 下一步阅读 {#next-steps}

- [数据模型](data-model.md)：查看完整的表结构定义。
- [同步协议](sync-protocol.md)：了解数据库状态如何构建为集群快照。
- [本地开发指南](development.md)：运行存储层一致性测试。
