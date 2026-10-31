// Thin Worker edge for the v2 Go/Wasm master. It only binds platform resources:
// D1 (goD1Bridge), verified mTLS identity (peer), cron (goV2Tick) and post-write
// hash publication. Every authorization and business decision is made in Go.
import { connect } from "cloudflare:sockets";
import masterWasm from "./dist/main.wasm";
import { allowed, verifiedPeer } from "./edge.js";
export { verifiedPeer } from "./edge.js";
import "./dist/wasm_exec.js";
import { createD1Bridge } from "./d1-bridge.js";
import { createSMTPBridge } from "./smtp-bridge.js";

const MAX_BODY = 1 << 20;
const ENV_KEYS = [
  "JWT_KEYS", "JWT_ACTIVE_KID", "PKI_MASTER_KEY", "CLOUDFLARE_API_TOKEN", "AUDIT_ENABLED", "AUDIT_MAX_RECORDS",
  "AUTH_ISSUER", "AUTH_AUDIENCE", "CORS_ORIGINS", "LDAP_BASE_DN", "LDAP_USER_DN",
  "SYNC_DNS_NAME", "CLOUDFLARE_ZONE_ID", "CLOUDFLARE_RECORD_ID", "SYNC_CLIENT_FINGERPRINTS", "PLUGINS_ENABLED",
  "BOOTSTRAP_ADMIN_USERNAME", "BOOTSTRAP_ADMIN_PASSWORD", "TELEMETRY_SAMPLE_RATIO", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS",
  "PLUGINS_CONFIG", "CLUSTERS",
];
// Plugin secrets referenced as "env:NAME" must use one of these prefixes to reach Go.
const PLUGIN_SECRET = /^(OAUTH|PLUGIN)_[A-Z0-9_]{1,64}$/;

let booting = null;
let booted = false;
let bootStarted = 0;
let generation = 0;

// A boot whose request was cancelled never finishes (its D1 I/O dies with that request), so
// waiters take over after this long instead of polling forever.
const BOOT_TAKEOVER_MS = 5000;

// Requests must not await work started by another request: on Workers the continuation (and any
// D1 call it makes) would run in the other request's I/O context and be cancelled as hung once that
// request returns. The first request boots; concurrent ones poll with their own timers.
async function boot(env) {
  if (booted) return;
  if (!booting) return startBoot(env);
  for (;;) {
    await new Promise((r) => setTimeout(r, 10));
    if (booted) return;
    // The other boot failed, or stalled because its request went away: boot ourselves.
    if (!booting || Date.now() - bootStarted > BOOT_TAKEOVER_MS) return startBoot(env);
  }
}

function startBoot(env) {
  const mine = ++generation;
  bootStarted = Date.now();
  booting = (async () => {
    globalThis.goD1Bridge = createD1Bridge(env.DB);
    globalThis.goV2SMTP = createSMTPBridge(connect);
    globalThis.goV2Env = Object.fromEntries(ENV_KEYS.map((k) => [k, typeof env[k] === "string" ? env[k] : ""]));
    for (const [k, v] of Object.entries(env)) {
      if (PLUGIN_SECRET.test(k) && typeof v === "string") globalThis.goV2Env[k] = v;
    }
    globalThis.goV2Env.TRUSTED_IP_HEADER = "CF-Connecting-IP";
    const ready = new Promise((resolve, reject) => {
      globalThis.goV2Ready = (error) => (error ? reject(new Error(error)) : resolve());
    });
    const go = new Go();
    go.env = { ...globalThis.goV2Env };
    const instance = await WebAssembly.instantiate(masterWasm, go.importObject);
    go.run(instance).catch((error) => console.error("Go runtime exited", error));
    await ready;
    if (mine === generation) booted = true;
  })();
  booting.catch(() => { if (mine === generation) booting = null; });
  return booting;
}


export default {
  async fetch(request, env, ctx) {
    try {
      const url = new URL(request.url);
      if (!allowed(url, env)) return new Response("not found", { status: 404 });
      const length = Number(request.headers.get("content-length") || 0);
      if (length > MAX_BODY) return new Response("request too large", { status: 413 });
      const body = request.method === "GET" || request.method === "HEAD" ? new Uint8Array() : new Uint8Array(await request.arrayBuffer());
      if (body.length > MAX_BODY) return new Response("request too large", { status: 413 });
      await boot(env);
      const out = await globalThis.goV2Fetch({
        method: request.method,
        url: request.url,
        headers: [...request.headers],
        body,
        clientIp: request.headers.get("cf-connecting-ip") || "0.0.0.0",
        peer: verifiedPeer(request, env),
      });
      if (globalThis.goV2TakeWrites()) {
        ctx.waitUntil(globalThis.goV2Publish().then((e) => { if (e) console.error("publish failed", e); }));
      }
      ctx.waitUntil(globalThis.goV2Flush().then((e) => { if (e) console.error("telemetry flush failed", e); }));
      return new Response(out.status === 204 ? null : out.body, { status: out.status, headers: out.headers });
    } catch (error) {
      console.error("v2 master request failed", error);
      return new Response("master unavailable", { status: 503 });
    }
  },
  async scheduled(_event, env, ctx) {
    ctx.waitUntil((async () => {
      await boot(env);
      const error = await globalThis.goV2Tick();
      if (error) throw new Error(error);
    })());
  },
};
