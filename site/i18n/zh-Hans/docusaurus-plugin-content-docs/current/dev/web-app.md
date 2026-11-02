# Web 前端应用

前端是一个位于 `web/` 目录下的 React 单页应用（SPA）。构建产出的静态文件与 API 由同一源（Origin）提供服务：在 Cloudflare 上由 Workers Static Assets 托管，在容器部署中由 `STATIC_DIR` 提供。因此所有 API 请求均为同源请求，无需额外配置 CORS。

| 维度 | 说明 |
| --- | --- |
| 技术栈 | React 19、React Router、Ant Design 6、`xterm.js`；使用 Vite 构建，Vitest 测试 |
| 入口文件 | `src/main.jsx` → `Root.jsx` → `components/Layout.jsx` |
| 构建产物 | `web/dist/`，随 Worker 一并发布或打包进容器镜像 |

## `web/src` 目录结构 {#layout-of-websrc}

| 路径 | 职责说明 |
| --- | --- |
| `routes.jsx` | 定义所有页面路由：路径、标题、图标、依赖的插件 ID 及所需权限 |
| `session.js` | 登录会话状态：当前用户、已启用插件列表、持有权限集合，以及 `can()`、`allowed()` 辅助函数 |
| `api/client.js` | 封装 `get`、`post`、`put`、`patch`、`del`，统一处理令牌刷新与错误转换 |
| `i18n.js`、`locales/en.json` | 多语言国际化支持 |
| `theme.js` | 浅色、深色或跟随系统主题管理 |
| `currentstatus.js` | 通过预签名链接直接从 R2 / S3 读取实时集群状态 |
| `gateway.js` | 浏览器端临时密钥对生成及与 Web 访问网关的会话管理 |
| `hooks.js` | 通用 React Hooks（`usePoll`、`useLoad`、`useConfirm` 等） |
| `views/admin/` | 管理控制台各功能页面 |
| `views/status/`、`views/workspace/` | 集群状态监控页、Web 终端与文件浏览器 |
| `components/` | 公共组件：整体布局、状态卡片、统计图表、邮件模板编辑器与终端组件 |

## 页面路由与权限控制 {#pages-and-permissions}

前端界面分为**管理控制台（Admin）**与**个人工作台（Workspace）**两大区域。`routes.jsx` 中的每条路由通过 `meta` 字段同时控制侧边栏菜单展示与路由守卫：

```js
{ path: "/admin/users", title: t("用户管理"), icon: UserOutlined,
  meta: { perm: ["user", "read"] }, element: lazy(() => import("./views/admin/Users.jsx")) }
```

| `meta` 属性 | 作用 |
| --- | --- |
| `perm: [object, action]` | 仅当用户持有该策略权限时显示并允许访问 |
| `plugin: "id"` | 仅当主服务启用了该插件时显示 |
| `admin: true` | 归入管理控制台菜单 |
| `scope` | 同一视图组件的不同数据范围（管理员为 `"all"`，个人工作台为 `"mine"`） |

用户登录后，`session.load()` 会调用一次 `GET /api/v2/me/access`，根据返回结果构建菜单与可用路由，确保用户不会看到无权使用的功能页面。服务端在每次 API 调用时仍会独立校验权限，前端隐藏仅作为交互优化而非安全边界。

所有页面均采用 `React.lazy()` 按需懒加载，不访问的页面不会下载对应的脚本分包。

## 调用后端 API {#talking-to-the-api}

```js
import { get, post } from "../api/client.js";

const users = await get("/api/v2/users", { page: 1 });
await post(`/api/v2/users/${id}/password-reset`);
```

- **会话令牌：** 保存在 `localStorage`（`gla.session`）中，请求时作为 `Authorization: Bearer` 发送。当距离过期不足 30 分钟时，客户端会在发起下一次请求前自动调用 `POST /api/v2/auth/refresh` 换取新令牌。
- **响应解包：** 客户端自动解包 `{"data": …}`；错误响应被封装为包含 `status`、`code`、`message` 与 `requestId` 的 `ApiError`，通过 `errorText(e)` 输出当前语言的错误提示。
- **`401` 处理：** 自动清空本地会话并跳转回登录页。
- **非 JSON 响应：**（例如 Cloudflare Access 会话过期后返回的 HTML 登录页）会被规范报告为 `HTTP <status>`，而不会抛出 JSON 解析异常。

## 绕过主服务读取实时状态 {#live-status-without-the-master}

监控页面轮询频繁，在 Cloudflare 上若每次都请求主服务会产生不必要的计费。因此前端直接从对象存储读取监控数据：

1. 调用 `GET /api/v2/current-status/links` 获取各集群实时状态对象的预签名 `GET` 链接（有效期 15 分钟）。
2. 浏览器直接从 R2 / S3 拉取各集群的 `current.json`。
3. 当预签名链接剩余有效期不足 1 分钟时自动续期。

`usePoll` 仅在浏览器标签页处于可见状态时执行轮询。若某个集群尚未发布实时状态对象，页面会自动回退至主服务的 `cluster-status` 与 `resource-status` 接口。

## Web 终端与文件管理 {#web-terminal-and-files}

`gateway.js` 实现了[网关会话握手](agents.md#how-a-session-opens)的浏览器侧逻辑：

- `generateKeyPair()` 使用浏览器 WebCrypto API 生成临时 Ed25519 密钥对；
- `temporaryCredential()` 调用 `POST /api/v2/web-access/ssh-certificate` 为该公钥申请 5 分钟有效期的临时 SSH 证书，并在到期前按集群复用；
- `GatewaySession` 负责创建会话、连接终端 WebSocket、列出与下载文件以及发起跨集群传输，并在已打开的 WebSocket 上自动续期来自 `POST /api/v2/web-access/ticket` 的网关票据。

临时私钥与证书仅存在于浏览器内存中，退出登录时由 `forgetCredentials()` 立即清除。

## 国际化（i18n） {#translation}

代码中直接以中文文案作为翻译键，`locales/en.json` 提供对应的英文映射：

```js
import { t } from "../i18n.js";
t("用户管理");                // Returns "Users" in English, or "用户管理" in Chinese
t("{0} 分钟前", 5);           // Positional placeholder interpolation: {0}, {1}, …
```

- 当前语言保存在 `localStorage`（`gla.locale`）中，默认为中文；切换语言时刷新页面，确保启动时构建的静态菜单同步完成翻译。
- `i18n.test.js` 自动检查占位符替换逻辑，并防止任何引入 `t` 的文件意外定义同名局部变量遮蔽翻译函数。
- 日期格式化根据当前语言自动选用 `en-GB` 或 `zh-CN`（`dateLocale`）。

## 主题切换 {#theme}

`theme.js` 在 `localStorage`（`gla.theme`）中保存 `system`、`light` 或 `dark`，并动态切换 Ant Design 的明暗主题算法；选择 `system` 时实时跟随系统 `prefers-color-scheme` 变化。

## 本地开发调试 {#develop}

```sh
cd web
npm ci
API_ORIGIN=http://127.0.0.1:8080 npm run dev   # proxies /api/* to a local cmd/server instance
npm test                                        # runs Vitest unit tests
npm run build                                   # builds production bundle in web/dist
```

如何在本地启动配套的主服务请参阅[本地开发指南](development.md)。

## 下一步阅读 {#next-steps}

- [API 参考](api-reference.md)：查看前端可调用的全部后端路由。
- [集群代理程序](agents.md)：了解 Web 终端所连接的登录节点网关。
