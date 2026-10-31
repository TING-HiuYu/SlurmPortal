import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError, setSession, getSession, setUnauthorizedHandler, errorText } from "./client.js";

const reply = (status, body) => Promise.resolve(new Response(body === undefined ? "" : JSON.stringify(body), { status }));

afterEach(() => {
  vi.unstubAllGlobals();
  setSession(null);
});

describe("api client", () => {
  it("unwraps data and sends the bearer token", async () => {
    setSession("tok", new Date(Date.now() + 86400e3).toISOString());
    const fetch = vi.fn(() => reply(200, { data: { ok: true } }));
    vi.stubGlobal("fetch", fetch);
    expect(await api("GET", "/api/v2/me", { params: { q: "a b", empty: "" } })).toEqual({ ok: true });
    expect(fetch.mock.calls[0][0]).toBe("/api/v2/me?q=a+b");
    expect(fetch.mock.calls[0][1].headers.authorization).toBe("Bearer tok");
  });
  it("turns error bodies into ApiError with the request id", async () => {
    vi.stubGlobal("fetch", () => reply(403, { error: { code: "forbidden", message: "permission denied", requestId: "r1" } }));
    const e = await api("GET", "/api/v2/users").catch((x) => x);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.status).toBe(403);
    expect(errorText(e)).toBe("permission denied (request r1)");
  });
  it("drops the session and notifies on 401", async () => {
    setSession("tok", new Date(Date.now() + 86400e3).toISOString());
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    vi.stubGlobal("fetch", () => reply(401, { error: { code: "unauthorized", message: "login required" } }));
    await api("GET", "/api/v2/me").catch(() => {});
    expect(getSession()).toBeNull();
    expect(handler).toHaveBeenCalledOnce();
  });
});
