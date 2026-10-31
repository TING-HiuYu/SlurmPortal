// Idempotently create the GitHub OAuth connector on a deployed v2 Worker.
// Usage: BASE=https://portal.example.org ADMIN_PASSWORD=... OAUTH_GITHUB_CLIENT_ID=... OAUTH_GITHUB_CLIENT_SECRET=... \
//        [CF_ACCESS_CLIENT_ID=... CF_ACCESS_CLIENT_SECRET=...] node connector-github.mjs
const e = process.env;
const BASE = e.BASE || "https://portal.example.org";
for (const k of ["ADMIN_PASSWORD", "OAUTH_GITHUB_CLIENT_ID", "OAUTH_GITHUB_CLIENT_SECRET"]) if (!e[k]) throw new Error(`${k} is required`);
const accessHeaders = e.CF_ACCESS_CLIENT_ID ? { "cf-access-client-id": e.CF_ACCESS_CLIENT_ID, "cf-access-client-secret": e.CF_ACCESS_CLIENT_SECRET } : {};

async function call(method, path, token, body) {
  const res = await fetch(BASE + path, {
    method,
    headers: { ...accessHeaders, ...(token ? { authorization: `Bearer ${token}` } : {}), ...(body ? { "content-type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path} ${res.status} ${text.slice(0, 300)}`);
  return text ? JSON.parse(text).data : null;
}

const { token } = await call("POST", "/api/v2/auth/login", null, { username: e.ADMIN_USERNAME || "admin", password: e.ADMIN_PASSWORD });
const existing = (await call("GET", "/api/v2/oauth/connectors", token)).find((c) => c.provider === "github" && c.clientId === e.OAUTH_GITHUB_CLIENT_ID);
if (existing) {
  console.log("connector exists", existing.id);
  process.exit(0);
}
const cas = await call("GET", "/api/v2/pki/cas", token);
let ca = (Array.isArray(cas) ? cas : cas.items || []).find((c) => c.kind === "ssh" && c.enabled);
if (!ca) ca = (await call("POST", "/api/v2/pki/cas", token, { kind: "ssh", name: "oauth" })).ca;
const roles = await call("GET", "/api/v2/roles", token);
const user = (Array.isArray(roles) ? roles : roles.items).find((r) => r.keyword === "user");
const c = await call("POST", "/api/v2/oauth/connectors", token, {
  name: "GitHub", provider: "github", caId: ca.id, enabled: true, defaultStatus: 1,
  defaultRoleIds: user ? [user.id] : [],
  clientId: e.OAUTH_GITHUB_CLIENT_ID, clientSecret: e.OAUTH_GITHUB_CLIENT_SECRET,
  redirectUri: `${BASE}/api/v2/oauth/callback/github`,
});
console.log("connector", c.id, "ca", ca.id, "secretSet", c.clientSecretSet);
