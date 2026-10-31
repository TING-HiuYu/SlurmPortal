import test from "node:test";
import assert from "node:assert/strict";
import { allowed, verifiedPeer } from "./edge.js";

const fp = "a".repeat(64);
const env = { SYNC_HOSTNAME: "sync.hpc.example.org", SYNC_CLIENT_FINGERPRINTS: fp };
const req = (url, cf) => ({ url, cf });

test("sync host serves only machine endpoints", () => {
  assert.equal(allowed(new URL("https://sync.hpc.example.org/sync/v2/snapshot"), env), true);
  assert.equal(allowed(new URL("https://sync.hpc.example.org/healthz"), env), true);
  assert.equal(allowed(new URL("https://sync.hpc.example.org/api/v2/users"), env), false);
  assert.equal(allowed(new URL("https://sync.hpc.example.org/"), env), false);
});

test("browser host never serves machine endpoints", () => {
  assert.equal(allowed(new URL("https://hpc.example.org/api/v2/me"), env), true);
  assert.equal(allowed(new URL("https://hpc.example.org/sync/v2/snapshot"), env), false);
  assert.equal(allowed(new URL("https://hpc.example.org/syncing"), env), true);
});

test("no split without SYNC_HOSTNAME", () => {
  assert.equal(allowed(new URL("https://any.example.org/sync/v2/snapshot"), {}), true);
});

test("client certificates count only on the sync host and when verified and allowlisted", () => {
  const ok = { tlsClientAuth: { certVerified: "SUCCESS", certRevoked: "0", certFingerprintSHA256: fp.toUpperCase() } };
  assert.equal(verifiedPeer(req("https://sync.hpc.example.org/sync/v2/snapshot", ok), env), fp);
  assert.equal(verifiedPeer(req("https://hpc.example.org/sync/v2/snapshot", ok), env), "");
  assert.equal(verifiedPeer(req("https://sync.hpc.example.org/x", { tlsClientAuth: { ...ok.tlsClientAuth, certRevoked: "1" } }), env), "");
  assert.equal(verifiedPeer(req("https://sync.hpc.example.org/x", { tlsClientAuth: { ...ok.tlsClientAuth, certVerified: "FAILED:x" } }), env), "");
  assert.equal(verifiedPeer(req("https://sync.hpc.example.org/x", { tlsClientAuth: { ...ok.tlsClientAuth, certFingerprintSHA256: "b".repeat(64) } }), env), "");
});
