import assert from "node:assert/strict";
import { test } from "node:test";

let revision = 0;
async function loadConfig(baseUrl) {
  const previous = process.env.BASE_URL;
  if (baseUrl === undefined) delete process.env.BASE_URL;
  else process.env.BASE_URL = baseUrl;
  try {
    const url = new URL("../docusaurus.config.js", import.meta.url);
    url.searchParams.set("test", String(revision++));
    return (await import(url.href)).default;
  } finally {
    if (previous === undefined) delete process.env.BASE_URL;
    else process.env.BASE_URL = previous;
  }
}

test("keeps the existing GitHub Pages URL when BASE_URL is unset", async () => {
  const config = await loadConfig();
  assert.equal(config.url, "https://ting-hiuyu.github.io");
  assert.equal(config.baseUrl, "/SlurmPortal/");
});

test("builds under the public parent URL with or without a trailing slash", async () => {
  for (const parent of ["https://thy.icu/sth", "https://thy.icu/sth/"]) {
    const config = await loadConfig(parent);
    assert.equal(config.url, "https://thy.icu");
    assert.equal(config.baseUrl, "/sth/SlurmPortal/");
  }
});

test("supports a domain root, nested paths and local proxy ports", async () => {
  for (const [parent, origin, baseUrl] of [
    ["https://thy.icu", "https://thy.icu", "/SlurmPortal/"],
    ["https://thy.icu/a/b/", "https://thy.icu", "/a/b/SlurmPortal/"],
    ["http://localhost:8788/sth/", "http://localhost:8788", "/sth/SlurmPortal/"],
  ]) {
    const config = await loadConfig(parent);
    assert.equal(config.url, origin);
    assert.equal(config.baseUrl, baseUrl);
  }
});

test("rejects malformed or ambiguous deployment URLs", async () => {
  for (const parent of [
    "", "/sth/", "thy.icu/sth/", "ftp://thy.icu/sth/",
    "https://user:password@thy.icu/", "https://thy.icu/?query=1", "https://thy.icu/#fragment",
  ]) {
    await assert.rejects(loadConfig(parent), /BASE_URL/);
  }
});
