// Thin fetch wrapper for the v2 API. Responses are {"data": …} or {"error": {code, message, requestId}}.

const TOKEN_KEY = "gla.session";
const REFRESH_BEFORE_MS = 30 * 60 * 1000;

export class ApiError extends Error {
  constructor(status, code, message, requestId) {
    super(message || code || `HTTP ${status}`);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

let session = readSession();
let onUnauthorized = () => {};
let refreshing = null;

function readSession() {
  try {
    const s = JSON.parse(localStorage.getItem(TOKEN_KEY) || "null");
    return s && s.token && Date.parse(s.expiresAt) > Date.now() ? s : null;
  } catch {
    return null;
  }
}

export function getSession() {
  return session;
}

export function setSession(token, expiresAt) {
  session = token ? { token, expiresAt } : null;
  try {
    if (session) localStorage.setItem(TOKEN_KEY, JSON.stringify(session));
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    /* storage unavailable: the session lives only in memory */
  }
}

export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

function query(params) {
  if (!params) return "";
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
}

async function raw(method, path, { body, params, token = session?.token } = {}) {
  const headers = {};
  if (token) headers.authorization = `Bearer ${token}`;
  if (body !== undefined) headers["content-type"] = "application/json";
  const res = await fetch(path + query(params), { method, headers, body: body === undefined ? undefined : JSON.stringify(body), credentials: "same-origin" });
  const text = await res.text();
  let json = null;
  try {
    json = text ? JSON.parse(text) : null;
  } catch {
    /* non-JSON (e.g. an Access login page) */
  }
  if (!res.ok) {
    const e = json?.error || {};
    throw new ApiError(res.status, e.code, e.message || (json ? "" : `HTTP ${res.status}`), e.requestId);
  }
  return json?.data ?? null;
}

// refresh swaps the token for a fresh one shortly before it expires.
async function maybeRefresh() {
  if (!session || Date.parse(session.expiresAt) - Date.now() > REFRESH_BEFORE_MS) return;
  refreshing ??= raw("POST", "/api/v2/auth/refresh")
    .then((d) => setSession(d.token, d.expiresAt))
    .catch(() => {})
    .finally(() => (refreshing = null));
  await refreshing;
}

export async function api(method, path, opts = {}) {
  if (!path.startsWith("/api/v2/auth/")) await maybeRefresh();
  try {
    return await raw(method, path, opts);
  } catch (e) {
    if (e instanceof ApiError && e.status === 401 && session) {
      setSession(null);
      onUnauthorized();
    }
    throw e;
  }
}

export const get = (path, params) => api("GET", path, { params });
export const post = (path, body) => api("POST", path, { body: body ?? {} });
export const put = (path, body) => api("PUT", path, { body });
export const patch = (path, body) => api("PATCH", path, { body });
export const del = (path) => api("DELETE", path);

// errorText renders an error for a message box, including the request id for support.
export function errorText(e) {
  if (e instanceof ApiError) return `${e.message}${e.requestId ? ` (request ${e.requestId})` : ""}`;
  return String(e?.message || e);
}
