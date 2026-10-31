// Browser client for the per-cluster web access gateway (terminal, SFTP files, rsync transfers).
// Master issues 5-minute gateway tickets and 5-minute SSH certificates for a key pair generated here;
// the private key never leaves this tab except to the gateway that uses it for this session.
import { post, getSession } from "./api/client.js";

export const FILE_ROOT = "/HOME";
const TICKET_SLACK_MS = 15000;

export function gatewayBase(value) {
  const url = new URL(value, window.location.origin);
  if (url.protocol !== "https:" || url.username || url.password || url.search || url.hash) throw new Error("invalid_gateway_url");
  return url.href.replace(/\/$/, "");
}

export function socketURL(base, path) {
  const url = new URL(gatewayBase(base) + path);
  url.protocol = "wss:";
  return url.href;
}

export function parentDirectory(path) {
  if (!path || path === "/") return "/";
  return path.replace(/\/+$/, "").replace(/\/[^/]*$/, "") || "/";
}

export function formatBytes(value) {
  if (value === null || value === undefined || !Number.isFinite(Number(value))) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let size = Number(value);
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit++;
  }
  return `${unit ? size.toFixed(1) : size} ${units[unit]}`;
}

export const transferFinished = (status) => ["completed", "partial", "failed", "unknown"].includes(status);
export const authorizationExpired = (ev) => ev.code === 4001 || (ev.code === 1008 && ["ticket expired", "renewal denied"].includes(ev.reason));
export const sessionUnavailable = (e) => e?.status === 404 && e?.code === "session_unavailable";

const b64 = (bytes) => btoa(Array.from(new Uint8Array(bytes), (v) => String.fromCharCode(v)).join(""));
function sshString(bytes) {
  const v = new Uint8Array(bytes);
  const out = new Uint8Array(v.length + 4);
  new DataView(out.buffer).setUint32(0, v.length);
  out.set(v, 4);
  return out;
}

export function openSSHPublicKey(raw) {
  if (new Uint8Array(raw).length !== 32) throw new Error("invalid_ed25519_public_key");
  const type = sshString(new TextEncoder().encode("ssh-ed25519"));
  const key = sshString(raw);
  const wire = new Uint8Array(type.length + key.length);
  wire.set(type);
  wire.set(key, type.length);
  return `ssh-ed25519 ${b64(wire)}`;
}

export async function generateKeyPair(subtle = window.crypto?.subtle) {
  if (!subtle) throw new Error("ed25519_unavailable");
  let pair;
  try {
    pair = await subtle.generateKey({ name: "Ed25519" }, true, ["sign", "verify"]);
  } catch {
    throw new Error("ed25519_unavailable");
  }
  const [raw, pkcs8] = await Promise.all([subtle.exportKey("raw", pair.publicKey), subtle.exportKey("pkcs8", pair.privateKey)]);
  return { publicKey: openSSHPublicKey(raw), privateKey: `-----BEGIN PRIVATE KEY-----\n${b64(pkcs8).match(/.{1,64}/g).join("\n")}\n-----END PRIVATE KEY-----` };
}

// One temporary credential per (token, cluster), reused until shortly before its certificate expires.
const credentials = new Map();

export async function temporaryCredential(username, clusterId) {
  const token = getSession()?.token;
  if (!token) throw new Error("session_changed");
  const key = `${token}\u0000${clusterId}`;
  const cached = credentials.get(key);
  if (cached && (await cached).expiresAt > Date.now() + TICKET_SLACK_MS) return cached;
  const pending = (async () => {
    const pair = await generateKeyPair();
    const res = await post("/api/v2/web-access/ssh-certificate", { clusterId, purpose: "access", publicKey: pair.publicKey });
    if (getSession()?.token !== token) throw new Error("session_changed");
    const expiresAt = Date.parse(res.expiresAt);
    if (!res.certificate || res.username !== username || !(expiresAt > Date.now() + TICKET_SLACK_MS)) throw new Error("invalid_temporary_certificate");
    return { privateKey: pair.privateKey, certificate: res.certificate, expiresAt };
  })();
  credentials.set(key, pending);
  pending.catch(() => credentials.delete(key));
  return pending;
}

export function forgetCredentials() {
  credentials.clear();
}

export class GatewaySession {
  constructor(cluster, username, purpose) {
    this.cluster = cluster;
    this.username = username;
    this.purpose = purpose;
    this.token = getSession()?.token;
    this.base = gatewayBase(cluster.gatewayUrl);
    this.sessionId = null;
    this.ticket = null;
    this.ticketScopes = [];
    this.closed = false;
    this.sockets = new Set();
    this.controllers = new Set();
    this.timers = new Map();
  }

  current() {
    if (this.closed || this.token !== getSession()?.token) throw new Error("session_changed");
  }

  scopes() {
    return this.purpose === "terminal" ? ["session:create", "terminal"] : ["session:create", "sftp"];
  }

  async authorization(scopes = this.scopes(), renew = false) {
    this.current();
    const t = this.ticket;
    if (renew || !t || Date.parse(t.expiresAt) <= Date.now() + TICKET_SLACK_MS || scopes.some((s) => !this.ticketScopes.includes(s))) {
      const d = await post("/api/v2/web-access/ticket", { clusterId: this.cluster.id, scopes });
      this.current();
      if (!d?.ticket || d.username !== this.username || d.gatewayId !== this.cluster.gatewayId || gatewayBase(d.gatewayUrl) !== this.base || !(Date.parse(d.expiresAt) > Date.now())) {
        throw new Error("invalid_gateway_ticket");
      }
      this.ticket = d;
      this.ticketScopes = scopes;
    }
    return this.ticket.ticket;
  }

  // The gateway's cookie lets native downloads and WebSockets authenticate without a header.
  async cookie(ticket) {
    const res = await this.fetch("/v1/auth/session", { method: "POST", credentials: "include", headers: { authorization: `Bearer ${ticket}`, "content-type": "application/json" }, body: "{}" });
    if (!res.ok) throw Object.assign(new Error("gateway_cookie_failed"), { status: res.status });
  }

  async fetch(path, init, timeoutMs = 30000) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), timeoutMs);
    this.controllers.add(controller);
    try {
      return await fetch(this.base + path, { cache: "no-store", credentials: "omit", ...init, signal: controller.signal });
    } finally {
      clearTimeout(timer);
      this.controllers.delete(controller);
    }
  }

  async request(path, { method = "POST", data, scopes, cookie = false } = {}) {
    const ticket = await this.authorization(scopes);
    if (cookie) await this.cookie(ticket);
    this.current();
    const res = await this.fetch(path, {
      method,
      headers: { authorization: `Bearer ${ticket}`, ...(data !== undefined ? { "content-type": "application/json" } : {}) },
      body: data !== undefined ? JSON.stringify(data) : undefined,
    });
    this.current();
    let body;
    try {
      body = await res.json();
    } catch {
      throw new Error("gateway_invalid_response");
    }
    if (!res.ok || ![0, 200].includes(body?.code)) {
      if (res.status === 401) this.ticket = null;
      throw Object.assign(new Error(body?.message || body?.error || "gateway_request_failed"), { status: res.status, code: typeof body?.error === "string" ? body.error : null });
    }
    return body.data;
  }

  async open() {
    const cred = await temporaryCredential(this.username, this.cluster.id);
    this.current();
    const d = await this.request("/v1/sessions", { data: { cluster_id: this.cluster.id, private_key: cred.privateKey, certificate: cred.certificate } });
    if (!d?.session_id || !d.home) throw new Error("invalid_gateway_session");
    this.sessionId = d.session_id;
    this.home = d.home;
    this.fileRoot = d.file_root || FILE_ROOT;
    return this;
  }

  sessionPath(path = "") {
    if (!this.sessionId) throw new Error("gateway_session_missing");
    return `/v1/sessions/${encodeURIComponent(this.sessionId)}${path}`;
  }

  list(path) {
    return this.request(this.sessionPath("/files/list"), { data: { path } });
  }

  async download(paths) {
    const d = await this.request(this.sessionPath("/files/download-ticket"), { data: { paths }, cookie: true });
    const url = new URL(d.url, `${this.base}/`);
    if (url.origin !== new URL(this.base).origin || !url.pathname.startsWith("/v1/") || !(Date.parse(d.expires_at) > Date.now())) throw new Error("invalid_download_ticket");
    // Native download streams straight from the gateway; nothing is buffered in the page.
    const a = document.createElement("a");
    a.href = url.href;
    a.rel = "noopener";
    a.setAttribute("download", "");
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  // socket opens a gateway WebSocket and renews its ticket in-band before the old one expires.
  async socket(path, frame, scopes) {
    let ticket = await this.authorization(scopes);
    await this.cookie(ticket);
    this.current();
    const ws = new WebSocket(socketURL(this.base, path));
    ws.binaryType = "arraybuffer";
    this.sockets.add(ws);
    const renewLater = () => {
      clearTimeout(this.timers.get(ws));
      const due = Date.parse(this.ticket.expiresAt) - Date.now() - 30000;
      this.timers.set(ws, setTimeout(async () => {
        try {
          ticket = await this.authorization(scopes, true);
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: "auth", ticket }));
            renewLater();
          }
        } catch {
          if (ws.readyState === WebSocket.OPEN) ws.close(4001, "authorization renewal failed");
        }
      }, Math.max(1000, due)));
    };
    ws.addEventListener("open", () => {
      if (this.closed) return ws.close();
      ws.send(JSON.stringify({ ticket, ...frame }));
      renewLater();
    });
    ws.addEventListener("close", () => {
      clearTimeout(this.timers.get(ws));
      this.timers.delete(ws);
      this.sockets.delete(ws);
    });
    return ws;
  }

  terminal(cols, rows) {
    return this.socket(this.sessionPath("/terminal"), { cols, rows });
  }

  async transfer(destination, sourcePaths, destinationPath) {
    const cred = await temporaryCredential(this.username, destination.id);
    return this.request(this.sessionPath("/transfers"), {
      scopes: ["transfer"],
      data: { destination_cluster_id: destination.id, source_paths: sourcePaths, destination_path: destinationPath, private_key: cred.privateKey, certificate: cred.certificate },
    });
  }

  transferStatus(id) {
    return this.request(`/v1/transfers/${encodeURIComponent(id)}`, { method: "GET", scopes: ["transfer:read"] });
  }

  transferLogs(id) {
    return this.socket(`/v1/transfers/${encodeURIComponent(id)}/logs`, {}, ["transfer:read"]);
  }

  async close() {
    if (this.closed) return;
    for (const c of this.controllers) c.abort();
    for (const t of this.timers.values()) clearTimeout(t);
    for (const s of this.sockets) s.close();
    const id = this.sessionId;
    this.sessionId = null;
    try {
      if (id && this.token === getSession()?.token) await this.request(`/v1/sessions/${encodeURIComponent(id)}`, { method: "DELETE", scopes: ["session:create"] });
    } catch {
      /* the gateway also expires abandoned sessions */
    } finally {
      this.closed = true;
      this.ticket = null;
    }
  }
}
