# Web app

The Slurm Portal frontend is a single-page React application in `web/`. Built once into static assets, it is served from the same origin as the master API—by Cloudflare Workers Static Assets in serverless deployments or from `STATIC_DIR` in container deployments—so API requests are same-origin and require no CORS preflight.

| Aspect | Details |
| --- | --- |
| Tech stack | React 19, React Router, Ant Design 6, `xterm.js`; built with Vite and tested with Vitest |
| Entrypoint | `src/main.jsx` → `Root.jsx` → `components/Layout.jsx` |
| Build output | `web/dist/` (bundled into the Worker deploy or copied into the master container image) |

## Layout of `web/src`

| Path | Responsibility |
| --- | --- |
| `routes.jsx` | Declarative route table defining path, title, icon, required plugin, and RBAC permission |
| `session.js` | Authenticated user state, active plugin list, permission map, and `can()` / `allowed()` helpers |
| `api/client.js` | HTTP client (`get`, `post`, `put`, `patch`, `del`) with automatic token refresh and error normalization |
| `i18n.js`, `locales/en.json` | Internationalization helper (`t()`) and English translation dictionary |
| `theme.js` | Theme manager supporting `light`, `dark`, and live `system` color-scheme tracking |
| `currentstatus.js` | Direct S3/R2 live status fetcher using presigned URLs |
| `gateway.js` | WebCrypto Ed25519 key generation, temporary certificate caching, and Web Access Gateway session client |
| `hooks.js` | Shared React hooks (`usePoll`, `useLoad`, `useConfirm`, etc.) |
| `views/admin/` | Administrative console views (users, groups, roles, PKI, mail, notifications, diagnostics, audit) |
| `views/status/`, `views/workspace/` | Cluster telemetry views, browser terminal, SFTP file manager, and self-service workspace |
| `components/` | Reusable UI components: navigation layout, status cards, charts, mail template editor, and terminal widget |

## Pages and permissions

The UI is organized into two primary workspaces: the **Admin Console** and the user **Workspace**. Every route in `routes.jsx` carries a `meta` descriptor that controls both sidebar visibility and client-side routing:

```js
{ path: "/admin/users", title: t("用户管理"), icon: UserOutlined,
  meta: { perm: ["user", "read"] }, element: lazy(() => import("./views/admin/Users.jsx")) }
```

| `meta` Property | Effect |
| --- | --- |
| `perm: [object, action]` | Renders the route and menu item only if the user holds `object:action` |
| `plugin: "id"` | Renders the route only if the named plugin is enabled on the master |
| `admin: true` | Places the item in the administrative console navigation |
| `scope` | Parameterizes shared views (`"all"` for administrators vs. `"mine"` in the personal workspace) |

Immediately after sign-in, `session.load()` calls `GET /api/v2/me/access` to retrieve the active plugins and the caller's effective permissions. All views are code-split with `React.lazy()`, so users only download the bundles for pages they actually open. Client-side menu filtering is purely a UX convenience; the Go router enforces permissions on every API call.

## Talking to the API

```js
import { get, post } from "../api/client.js";

const users = await get("/api/v2/users", { page: 1 });
await post(`/api/v2/users/${id}/password-reset`);
```

- **Token lifecycle:** Session tokens are stored in `localStorage` (`gla.session`) and sent in the `Authorization` header. When a token is within 30 minutes of expiration, the client automatically exchanges it via `POST /api/v2/auth/refresh` before issuing the next request.
- **Envelope unwrapping:** Successful `{"data": …}` responses are unwrapped automatically. Error responses throw an `ApiError` carrying `status`, `code`, `message`, and `requestId`; `errorText(e)` maps machine error codes to localized messages.
- **Session expiration (`401`):** Clears local session state and redirects to the login screen.
- **Non-JSON responses** (such as an HTML login page when a Cloudflare Access session expires) are reported cleanly as `HTTP <status>` rather than throwing a JSON parse exception.

## Live status without the master

Because status dashboards poll frequently, they read directly from object storage instead of querying the master database:

1. `GET /api/v2/current-status/links` returns 15-minute presigned `GET` URLs for each cluster's status object.
2. The browser fetches each cluster's `current.json` directly from R2 or S3.
3. Presigned URLs are refreshed automatically when less than 60 seconds remain.

`usePoll` pauses polling whenever the browser tab is hidden. If a cluster has not yet uploaded a `current-status` object, the UI seamlessly falls back to the master's `/api/v2/cluster-status` and `/api/v2/resource-status` endpoints.

## Web terminal and files

`gateway.js` implements the browser side of the [Web Access Gateway handshake](agents.md#how-a-session-opens):

- `generateKeyPair()` generates an ephemeral Ed25519 key pair in memory using the browser's `SubtleCrypto` API;
- `temporaryCredential()` requests a 5-minute SSH certificate from `POST /api/v2/web-access/ssh-certificate` and reuses it per cluster until shortly before expiration;
- `GatewaySession` opens the gateway session, attaches the `xterm.js` WebSocket, lists and downloads files over SFTP, and starts cross-cluster `rsync` transfers—automatically renewing gateway tickets in-band over open WebSockets.

Ephemeral private keys and certificates reside strictly in JavaScript memory and are wiped by `forgetCredentials()` on sign-out.

## Translation

Chinese phrases serve as source keys in code, and `locales/en.json` maps them to English:

```js
import { t } from "../i18n.js";
t("用户管理");                // Returns "Users" in English, or "用户管理" in Chinese
t("{0} 分钟前", 5);           // Positional placeholder interpolation: {0}, {1}, …
```

- The active locale is stored in `localStorage` (`gla.locale`), defaulting to Chinese. Switching locales reloads the page so static route definitions re-render in the chosen language.
- `i18n.test.js` verifies placeholder formatting and ensures no module importing `t` accidentally shadows it with a local variable.
- Timestamps are formatted via `Intl` using `en-GB` or `zh-CN` (`dateLocale`).

## Theme

`theme.js` persists `system`, `light`, or `dark` in `localStorage` (`gla.theme`) and configures Ant Design's theme algorithm, updating live when `prefers-color-scheme` changes.

## Develop

```sh
cd web
npm ci
API_ORIGIN=http://127.0.0.1:8080 npm run dev   # proxies /api/* to a local cmd/server instance
npm test                                        # runs Vitest unit tests
npm run build                                   # builds production bundle in web/dist
```

See [Development Guide](development.md) for instructions on starting a local master server.

## Next steps

- [API Reference](api-reference.md): complete list of endpoints consumed by the web app.
- [Cluster Agents](agents.md): how the Web Access Gateway handles browser sessions.
