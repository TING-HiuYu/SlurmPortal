// Live cluster status straight from R2: master signs short GET links, the browser fetches each
// cluster's current.json directly, so status polling never reaches the Worker or D1. When no
// cluster has published yet, callers fall back to the master APIs.
import { get } from "./api/client.js";
import { hasPlugin } from "./session.js";

let links = null;

async function signedLinks() {
  if (!links || Date.parse(links.expiresAt) - Date.now() < 60000) links = await get("/api/v2/current-status/links");
  return links;
}

async function object(link) {
  // Objects are stored with Cache-Control: no-store. Setting cache: "no-store" here would add
  // Cache-Control/Pragma request headers, which turns this into a CORS preflight R2 refuses.
  const res = await fetch(link.url, { credentials: "omit" });
  if (res.status === 404) return null;
  if (!res.ok) throw new Error(`current status ${res.status}`);
  return res.json();
}

// Mirrors clusterstatus.overall on the server.
export function overall(c) {
  const total = Object.values(c).reduce((a, n) => a + n, 0);
  const healthy = (c.idle || 0) + (c.full || 0) + (c.running || 0);
  if (!total) return "unavailable";
  if (c.offline === total) return "offline";
  if (c.offline || c.unknown || !healthy) return "abnormal";
  if (c.drain || c.maintenance) return "partial_maintenance";
  return "normal";
}

// load returns [{id, name, stale, doc}] or null when R2 status is not in use.
async function load() {
  if (!hasPlugin("current-status")) return null;
  const l = await signedLinks();
  if (!l.clusters?.length) return null;
  const docs = await Promise.all(l.clusters.map((c) => object(c).catch(() => null)));
  if (docs.every((d) => !d)) return null;
  return l.clusters.map((c, i) => {
    const doc = docs[i];
    const stale = !doc || Date.now() - Date.parse(doc.observed_at) > (doc.stale_after_seconds || l.staleAfterSeconds) * 1000;
    return { id: c.id, name: c.name, stale, doc };
  });
}

export async function nodesFromR2() {
  const rows = await load();
  if (!rows) return null;
  return {
    updatedAt: new Date().toISOString(),
    clusters: rows.map(({ id, name, stale, doc }) => {
      const s = doc?.cluster_status;
      const base = { id, name, address: s?.address || null, currentIps: [], nodes: [], nodeCounts: {}, nodesTotal: 0, nodesTruncated: false, observedAt: doc?.observed_at };
      if (!s) return { ...base, status: "unavailable", error: "never_observed" };
      if (stale) return { ...base, status: "offline", error: "stale_snapshot" };
      if (s.health !== "ok") return { ...base, status: "abnormal", error: "collection_failed", currentIps: s.current_ips || [] };
      return { ...base, status: overall(s.node_counts), error: null, currentIps: s.current_ips || [], nodes: s.nodes, nodeCounts: s.node_counts, nodesTotal: s.nodes_total, nodesTruncated: s.nodes_truncated };
    }),
  };
}

export async function queueFromR2() {
  const rows = await load();
  if (!rows) return null;
  return {
    updatedAt: new Date().toISOString(),
    clusters: rows.map(({ id, name, stale, doc }) => {
      const r = doc?.resource_status;
      const base = { id, name, jobs: [], jobsTotal: 0, jobsTruncated: false, resources: null, observedAt: doc?.observed_at, receivedAt: doc?.received_at };
      if (!r) return { ...base, health: "unavailable", error: "never_observed" };
      if (stale) return { ...base, health: "stale", error: "stale_snapshot" };
      if (r.health !== "ok") return { ...base, health: "error", error: "collection_failed" };
      return { ...base, health: "ok", error: null, resources: r.resources, jobs: r.jobs, jobsTotal: r.jobs_total, jobsTruncated: r.jobs_truncated };
    }),
  };
}
