// Host split for the Worker edge. The sync hostname (mutual TLS, not behind Cloudflare Access) serves
// only machine endpoints and is the only place a client certificate counts; every other hostname
// (behind Access) serves the browser API and never the machine endpoints.

export function isSyncPath(pathname) {
  return pathname === "/sync" || pathname.startsWith("/sync/");
}

// allowed reports whether this host may serve this path. Without SYNC_HOSTNAME nothing is split.
export function allowed(url, env) {
  const sync = (env.SYNC_HOSTNAME || "").toLowerCase();
  if (!sync) return true;
  if (url.hostname.toLowerCase() === sync) return isSyncPath(url.pathname) || url.pathname === "/healthz";
  return !isSyncPath(url.pathname);
}

// verifiedPeer returns the client certificate fingerprint only when Cloudflare verified it, it is not
// revoked, it is explicitly allowlisted and (when split) the request arrived on the sync hostname.
export function verifiedPeer(request, env) {
  const sync = (env.SYNC_HOSTNAME || "").toLowerCase();
  if (sync && new URL(request.url).hostname.toLowerCase() !== sync) return "";
  const auth = request.cf?.tlsClientAuth;
  const allowedFps = (env.SYNC_CLIENT_FINGERPRINTS || "").toLowerCase().split(",").map((v) => v.trim()).filter((v) => /^[a-f0-9]{64}$/.test(v));
  const fp = (auth?.certFingerprintSHA256 || "").toLowerCase();
  if (auth?.certVerified === "SUCCESS" && auth?.certRevoked === "0" && allowedFps.includes(fp)) return fp;
  return "";
}
