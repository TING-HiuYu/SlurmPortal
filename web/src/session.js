// Signed-in user state. Navigation is built from /api/v2/me/access; the server re-checks every call.
import { useSyncExternalStore } from "react";
import { get, post, getSession, setSession, api } from "./api/client.js";
import { t } from "./i18n.js";

const empty = { user: null, roles: [], plugins: [], permissions: new Set(), loaded: false };
let state = empty;
const listeners = new Set();

function update(patch) {
  state = { ...state, ...patch };
  listeners.forEach((l) => l());
}

export const getState = () => state;
const subscribe = (l) => (listeners.add(l), () => listeners.delete(l));
export const useSession = () => useSyncExternalStore(subscribe, getState);

export const signedIn = () => !!getSession();
export const invalidate = () => update({ loaded: false });

export async function load() {
  const [me, access] = await Promise.all([get("/api/v2/me"), get("/api/v2/me/access")]);
  update({
    user: me.user,
    roles: me.roles || [],
    plugins: access.plugins || [],
    permissions: new Set((access.permissions || []).map((p) => `${p.object}:${p.action}`)),
    loaded: true,
  });
}

export const can = (object, action = "read") => state.permissions.has(`${object}:${action}`);
export const hasPlugin = (id) => state.plugins.includes(id);

// allowed evaluates a route's meta: {perm: [object, action]} and/or {plugin: id}.
export function allowed(meta = {}) {
  if (meta.plugin && !hasPlugin(meta.plugin)) return false;
  if (meta.perm && !can(...meta.perm)) return false;
  return true;
}

function begin(token, expiresAt) {
  setSession(token, expiresAt);
  update({ loaded: false });
}

export async function logout() {
  try {
    await post("/api/v2/auth/logout");
  } catch {
    /* the token may already be gone */
  }
  setSession(null);
  state = empty;
  listeners.forEach((l) => l());
}

export async function passwordLogin(username, password) {
  const d = await api("POST", "/api/v2/auth/login", { body: { username, password }, token: null });
  begin(d.token, d.expiresAt);
}

// accessLogin exchanges the Cloudflare Access cookie (sent automatically, same origin) for a token.
export async function accessLogin() {
  const d = await api("POST", "/api/v2/auth/access/session", { body: {}, token: null });
  begin(d.token, d.expiresAt);
}

// oauthLogin opens the provider in a popup; the callback page posts the result back to this origin.
export function oauthLogin(connectorId) {
  return new Promise((resolve, reject) => {
    const popup = window.open(`/api/v2/oauth/authorize/${connectorId}`, "gla-oauth", "width=520,height=720");
    if (!popup) return reject(new Error(t("浏览器阻止了弹出窗口")));
    const timer = setInterval(() => {
      if (popup.closed) {
        cleanup();
        reject(new Error(t("登录窗口已关闭")));
      }
    }, 500);
    function cleanup() {
      clearInterval(timer);
      window.removeEventListener("message", onMessage);
    }
    function onMessage(ev) {
      if (ev.origin !== window.location.origin || ev.data?.type !== "go-ldap-admin:oauth") return;
      cleanup();
      if (ev.data.error) return reject(new Error(ev.data.error.message));
      begin(ev.data.token, ev.data.expiresAt);
      resolve({ initialPassword: ev.data.initialPassword });
    }
    window.addEventListener("message", onMessage);
  });
}
