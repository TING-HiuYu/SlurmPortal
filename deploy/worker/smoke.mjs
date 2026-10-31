// End-to-end smoke test against a running v2 Worker (local `wrangler dev` or an isolated test deployment).
// Usage: BASE=https://host ADMIN_PASSWORD=... node smoke.mjs
import assert from "node:assert/strict";

const BASE = process.env.BASE || "https://localhost:8799";
const ADMIN = process.env.ADMIN_USERNAME || "admin";
const PASSWORD = process.env.ADMIN_PASSWORD;
assert.ok(PASSWORD, "ADMIN_PASSWORD is required");
const suffix = Date.now().toString(36);
// Optional Access service token for hosts behind Cloudflare Access.
const accessHeaders = process.env.CF_ACCESS_CLIENT_ID
  ? { "cf-access-client-id": process.env.CF_ACCESS_CLIENT_ID, "cf-access-client-secret": process.env.CF_ACCESS_CLIENT_SECRET }
  : {};

async function call(method, path, { token, body, expect } = {}) {
  const res = await fetch(BASE + path, {
    method,
    headers: { ...accessHeaders, ...(token ? { authorization: `Bearer ${token}` } : {}), ...(body ? { "content-type": "application/json" } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  const json = text ? JSON.parse(text) : null;
  if (expect !== undefined) assert.equal(res.status, expect, `${method} ${path} -> ${res.status} ${text.slice(0, 300)}`);
  return { status: res.status, data: json?.data, error: json?.error, headers: res.headers };
}

const steps = [];
async function step(name, fn) {
  const t = Date.now();
  await fn();
  steps.push(`ok  ${name} (${Date.now() - t} ms)`);
}

let admin, alice, aliceToken, group, ca;
await step("health", async () => { await call("GET", "/healthz", { expect: 200 }); });
await step("anonymous denied", async () => { await call("GET", "/api/v2/users", { expect: 401 }); });
await step("admin login", async () => {
  const r = await call("POST", "/api/v2/auth/login", { body: { username: ADMIN, password: PASSWORD }, expect: 200 });
  admin = r.data.token;
  assert.ok(r.headers.get("traceparent"), "traceparent header missing");
});
await step("wrong password denied", async () => {
  await call("POST", "/api/v2/auth/login", { body: { username: ADMIN, password: "wrong-password" }, expect: 401 });
});
await step("me", async () => {
  const r = await call("GET", "/api/v2/me", { token: admin, expect: 200 });
  assert.equal(r.data.user.username, ADMIN);
});
await step("create posix group", async () => {
  const r = await call("POST", "/api/v2/groups", { token: admin, body: { groupName: `g${suffix}`, groupType: "cn", groupClass: "posixGroup", homeRoot: "/HOME/test", loginShell: "/bin/bash" }, expect: 201 });
  group = r.data;
  assert.ok(group.gidNumber >= 10000);
});
await step("create user", async () => {
  const r = await call("POST", "/api/v2/users", { token: admin, expect: 201,
    body: { username: `u${suffix}`, mail: `u${suffix}@example.org`, gidNumber: group.gidNumber } });
  alice = r.data;
  assert.ok(alice.initialPassword && alice.user.uidNumber >= 10000);
  assert.equal(alice.user.homeDirectory, `/HOME/test/u${suffix}`, "home comes from the main group preset");
});
await step("user login and least privilege", async () => {
  const r = await call("POST", "/api/v2/auth/login", { body: { username: `u${suffix}`, password: alice.initialPassword }, expect: 200 });
  aliceToken = r.data.token;
  await call("GET", "/api/v2/users", { token: aliceToken, expect: 403 });
  await call("PATCH", `/api/v2/users/${alice.user.id}`, { token: aliceToken, body: { roleIds: [1] }, expect: 403 });
});
await step("ssh CA + certificate", async () => {
  const r = await call("POST", "/api/v2/pki/cas", { token: admin, body: { kind: "ssh", name: `ca${suffix}` }, expect: 201 });
  ca = r.data.ca;
  assert.match(r.data.privateKeyPem, /OPENSSH PRIVATE KEY/);
  const issued = await call("POST", `/api/v2/users/${alice.user.id}/certificate`, { token: admin, body: { caId: ca.id }, expect: 201 });
  assert.match(issued.data.certificatePem, /ssh-ed25519-cert-v01@openssh.com/);
  const mine = await call("GET", "/api/v2/me/certificate", { token: aliceToken, expect: 200 });
  assert.equal(mine.data.serialHex, issued.data.serialHex);
});
await step("oauth connector + authorize", async () => {
  const redirectUri = `${BASE}/api/v2/oauth/callback/github`;
  const r = await call("POST", "/api/v2/oauth/connectors", { token: admin, expect: 201,
    body: { name: `gh${suffix}`, provider: "github", caId: ca.id, enabled: true, clientId: "smoke-client", redirectUri, clientSecret: "smoke-secret",
      rules: [{ name: "all", match: "any", mainGroupId: group.id }] } });
  assert.equal(r.data.clientSecretSet, true);
  assert.ok(!JSON.stringify(r.data).includes("smoke-secret"));
  await call("POST", "/api/v2/oauth/connectors", { token: aliceToken, body: { name: "x" }, expect: 403 });
  const pub = await call("GET", "/api/v2/oauth/connectors/public", { expect: 200 });
  assert.ok(pub.data.some((c) => c.id === r.data.id && !("clientId" in c)));
  const auth = await fetch(`${BASE}/api/v2/oauth/authorize/${r.data.id}`, { redirect: "manual", headers: accessHeaders });
  assert.equal(auth.status, 302);
  const loc = new URL(auth.headers.get("location"));
  assert.equal(loc.origin + loc.pathname, "https://github.com/login/oauth/authorize");
  assert.match(auth.headers.get("set-cookie") || "", /__Host-gla-oauth=.*HttpOnly/i);
  const cb = await fetch(`${BASE}/api/v2/oauth/callback/github?code=x&state=${loc.searchParams.get("state")}`, { headers: accessHeaders });
  assert.equal(cb.status, 400);
  assert.match(await cb.text(), /invalid_state/);
  await call("DELETE", `/api/v2/oauth/connectors/${r.data.id}`, { token: admin, expect: 204 });
});
await step("cloudflare access rejects missing assertion", async () => {
  const r = await call("GET", "/api/v2/auth/access", { expect: 200 });
  assert.equal(r.data.available, false);
  await call("POST", "/api/v2/auth/access/session", { expect: 401 });
});
await step("snapshot is peer-only", async () => {
  // 404 when the edge splits machine endpoints onto SYNC_HOSTNAME, else 401 from the peer check.
  const res = await fetch(BASE + "/sync/v2/snapshot", { headers: { ...accessHeaders, authorization: `Bearer ${admin}` } });
  assert.ok([401, 404].includes(res.status), `snapshot served to a browser token: ${res.status}`);
});
await step("mail providers + mail-pool pool", async () => {
  const smtp = (from) => ({ kind: "smtp", enabled: false, config: { host: "smtp.example.com", port: 465, security: "tls", username: "u", fromAddress: from, timeoutMs: 5000 } });
  await call("PUT", `/api/v2/mail/providers/s1${suffix}`, { token: admin, body: { ...smtp("a@example.com"), secret: "smoke-pw" }, expect: 200 });
  await call("PUT", `/api/v2/mail/providers/s2${suffix}`, { token: admin, body: smtp("b@example.com"), expect: 200 });
  const pool = await call("PUT", `/api/v2/mail/providers/pool${suffix}`, { token: admin, expect: 200,
    body: { kind: "mail-pool", enabled: true, config: { strategy: "priority", period: "day", members: [{ provider: `s1${suffix}`, quota: 300 }, { provider: `s2${suffix}` }] } } });
  assert.equal(pool.data.config.members[1].weight, 1);
  const list = await call("GET", "/api/v2/mail/providers", { token: admin, expect: 200 });
  assert.ok(!JSON.stringify(list.data).includes("smoke-pw"));
  assert.equal(list.data.find((p) => p.name === `s1${suffix}`).secretSet, true);
  const usage = await call("GET", "/api/v2/mail-pool/usage", { token: admin, expect: 200 });
  assert.deepEqual(usage.data.find((u) => u.pool === `pool${suffix}`).members.map((m) => [m.provider, m.remaining]), [[`s1${suffix}`, 300], [`s2${suffix}`, null]]);
  await call("DELETE", `/api/v2/mail/providers/s1${suffix}`, { token: admin, expect: 409 });
  await call("GET", "/api/v2/mail/providers", { token: aliceToken, expect: 403 });
  for (const n of [`pool${suffix}`, `s1${suffix}`, `s2${suffix}`]) await call("DELETE", `/api/v2/mail/providers/${n}`, { token: admin, expect: 204 });
});
await step("disable revokes access", async () => {
  await call("PUT", `/api/v2/users/${alice.user.id}/status`, { token: admin, body: { status: 2 }, expect: 204 });
  await call("GET", "/api/v2/me", { token: aliceToken, expect: 401 });
  await call("GET", "/api/v2/me/certificate", { token: aliceToken, expect: 401 });
});
await step("cleanup", async () => {
  await call("DELETE", `/api/v2/users/${alice.user.id}`, { token: admin, expect: 204 });
  await call("DELETE", `/api/v2/groups/${group.id}`, { token: admin, expect: 204 });
});
await step("logout", async () => {
  await call("POST", "/api/v2/auth/logout", { token: admin, expect: 204 });
  await call("GET", "/api/v2/me", { token: admin, expect: 401 });
});
console.log(steps.join("\n"));
