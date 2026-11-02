# 分层：HTTP 与边缘层

HTTP 与边缘层位于网络接入点与业务处理器之间，负责：

- 接收请求并执行主机名隔离的边缘运行时（Edge）；
- 强制要求声明权限的 HTTP 路由器；
- 通用中间件处理链；
- 严格的 JSON 请求解析与统一错误模型。

**相关代码包：** `internal/httpx`、`internal/apperr`、`deploy/worker`（Cloudflare Worker 边缘层）与 `cmd/server`（原生容器边缘层）。

## 边缘接入层（Edge） {#the-edge}

| 维度 | Cloudflare Worker（`deploy/worker`） | 容器部署（`cmd/server` + Caddy） |
| --- | --- | --- |
| 浏览器 TLS 终结 | Cloudflare 边缘节点（可选前置 Cloudflare Access） | Caddy 反向代理，通过 `BEHIND_TLS_PROXY` 让服务信任 `X-Forwarded-Proto` |
| 静态前端资源 | Workers Static Assets（不计入 Worker 请求计费） | `STATIC_DIR` 本地目录，带 SPA 路由回退，`/assets/*` 永久缓存 |
| 机器同步路由 | 独立主机名 `SYNC_HOSTNAME`，由 Cloudflare 验证客户端证书 | 独立双向 TLS 监听端口 `SYNC_LISTEN`，使用 `SYNC_CLIENT_CA_FILE` 校验客户端证书 |
| 请求体大小上限 | `worker.js` 中限制为 1 MiB | 每个处理器默认限制为 1 MiB（`httpx.DefaultMaxBody`） |
| 客户端 IP 获取 | `CF-Connecting-IP` | `TRUSTED_IP_HEADER`（Caddy 后方为 `X-Forwarded-For`） |
| 写后快照发布 | `ctx.waitUntil(goV2Publish())` | `cmd/server` 内置的 5 秒定时器 |

`deploy/worker/edge.js` 实现了两条核心边缘安全规则（均由 `edge.test.mjs` 覆盖测试）：

- `allowed(url, env)`：同步主机名仅允许访问 `/sync/*` 与 `/healthz`，其他主机名严禁访问 `/sync/*`。
- `verifiedPeer(request, env)`：仅当请求到达同步主机名且客户端证书已通过 Cloudflare 验证时，才向 Go 传递证书指纹。

## 路由器（Router） {#router}

`httpx.Router` 封装了 Go 标准库的 `http.ServeMux`（支持 Go 1.22+ 的 `GET /api/v2/users/{id}` 路由模式）：

```go
rt.Handle(httpx.Route{
	Method:     http.MethodGet,
	Path:       "/api/v2/hello",
	Permission: authz.Authenticated(),
	Handler: func(w http.ResponseWriter, r *http.Request) error {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"hello": "world"})
		return nil
	},
})
```

| 字段 | 说明 |
| --- | --- |
| `Permission` | **必填项。** 取值为 `authz.Public()`、`authz.Authenticated()`、`authz.Peer()` 或 `authz.Policy(object, action)` 之一。缺失时 `Handle` 在启动阶段直接 panic。 |
| `Handler` | 返回 `error`；非空错误由路由器统一调用 `WriteError` 序列化输出。 |
| `Routine` | 标记高频周期性机器上报路由，仅在请求失败时写入审计日志。 |

`Router.Routes()` 返回所有已注册路由的元数据，`tools/apidoc` 据此自动生成[API 参考](api-reference.md)。

## 中间件链（Middleware） {#middleware}

在 `internal/app/app.go` 中按从外到内的顺序应用：

| 中间件 | 行为说明 |
| --- | --- |
| `WithScope` | 保留格式合法的 `X-Request-Id` 或生成新 UUID，在上下文中维护单次请求状态（匹配路由、状态码、审计目标、调用方主体）。 |
| `telemetry.HTTPMiddleware` | 当启用遥测插件时，按路由模式名称为每个请求创建一个追踪 Span。 |
| `Recover` | 捕获 panic，附带请求 ID 记录错误日志并返回 `500`。 |
| `SecurityHeaders` | 写入 `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: no-referrer`、`Cache-Control: no-store` 与 `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`。 |
| `RateLimit` | 按客户端 IP 执行令牌桶限流：`HTTP.RatePerSecond`（默认 20）与 `HTTP.RateBurst`（默认 200），超限返回 `429 rate_limited`。 |
| `CORS` | 仅放行 `CORS_ORIGINS` 中的精确源；允许请求头 `Authorization, Content-Type, X-Request-Id, Traceparent` 与方法 `GET, POST, PUT, PATCH, DELETE`，预检结果缓存 600 秒。同源部署无需配置 CORS。 |
| 写操作追踪 | 标记 `/sync/` 之外成功的非 `GET` 请求，以触发快照哈希重发布。 |
| `audit.Middleware` | 在处理器执行完毕后记录审计日志（详见[领域模块层：审计](layer-domain.md#audit)）。 |

## 请求体解析 {#request-bodies}

所有处理器统一通过 `httpx.DecodeJSONLimit(w, r, &dst, limit)` 读取 JSON 请求体：

- `Content-Type` 必须为 `application/json`，否则返回 `415`；
- 请求体大小不得超过 `limit`（默认 1 MiB），否则返回 `413`；
- 拒绝未知字段（`DisallowUnknownFields`），字段名拼写错误会立即报错而不会被静默忽略；
- 严格校验请求体仅包含单个完整 JSON 值。

输入结构体均实现 `Validate() error`，由处理器在执行业务逻辑前调用。

## 统一错误模型 {#error-model}

客户端收到的所有错误均为 `apperr.Error`，包含 HTTP 状态码、稳定的机器错误码（`code`）与可读消息（`message`）。`apperr.From(err)` 会将任何未知内部错误映射为 `500 internal`；原始错误原因仅记录在服务端日志中，绝不返回给客户端。

| 错误码 | 状态码 | 触发场景 |
| --- | --- | --- |
| `bad_request` | `400` | 请求格式错误或参数校验未通过（可通过 `apperr.BadRequest(msg)` 指定说明） |
| `unauthenticated` | `401` | 缺失、无效、过期或已吊销的令牌，或账号已被禁用 |
| `forbidden` | `403` | 调用方角色缺少所需权限，或目标对象等级不低于调用方 |
| `not_found` | `404` | 目标资源不存在 |
| `conflict` | `409` | 状态冲突（如用户名或用户组名重复） |
| `too_large` | `413` | 请求体超过大小上限 |
| `unsupported_media_type` | `415` | 请求内容不是 JSON |
| `rate_limited` | `429` | 超过单 IP 请求速率限制 |
| `internal` | `500` | 未预期的服务端内部错误（服务端日志中含 `requestId`） |
| `unavailable` | `503` | 下游依赖（如数据库或 PKI 密钥）不可用 |

各业务模块还定义了领域专属错误码，例如 `insecure_transport`（要求 HTTPS）与 `main_group_required`。客户端代码应始终根据 `code` 而非 `message` 进行分支处理。

## 响应输出辅助函数 {#writing-responses}

- `httpx.WriteJSON(w, status, data)` 输出 `{"data": data}`（`204` 状态码无响应体）。
- `httpx.WriteError(w, r, err)` 输出 `{"error": {"code", "message", "requestId"}}`。
- `httpx.SetTarget(ctx, "user:alice")` 标记本次请求操作的目标对象，供审计日志记录。

## 下一步阅读 {#next-steps}

- [身份与访问层](layer-access.md)：了解路由守卫如何执行认证与授权校验。
- [请求生命周期](request-lifecycle.md)：通过完整时序图查看请求处理全貌。
