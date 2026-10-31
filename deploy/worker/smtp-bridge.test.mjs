import assert from "node:assert/strict";
import test from "node:test";
import { createSMTPBridge } from "./smtp-bridge.js";

const enc = new TextEncoder();
const dec = new TextDecoder();
const b64 = (s) => btoa(String.fromCharCode(...enc.encode(s)));

// fakeServer scripts an SMTP peer behind fake cloudflare:sockets streams.
function fakeServer({ starttls = true, mech = "PLAIN", password = "pw", greet = true } = {}) {
  const state = { log: [], connects: [], data: null, tlsUpgrades: 0 };
  function makeSocket(secure) {
    let controller;
    const readable = new ReadableStream({ start(c) { controller = c; } });
    const reply = (...lines) => controller.enqueue(enc.encode(lines.map((l) => `${l}\r\n`).join("")));
    let buf = "", inData = false, body = "", loginStep = 0;
    const writable = new WritableStream({
      write(chunk) {
        buf += dec.decode(chunk);
        let i;
        while ((i = buf.indexOf("\r\n")) >= 0) {
          const line = buf.slice(0, i);
          buf = buf.slice(i + 2);
          if (inData) {
            if (line === ".") { inData = false; state.data = body; reply("250 queued"); } else body += `${line}\r\n`;
            continue;
          }
          if (loginStep === 1) { loginStep = 2; reply("334 UGFzc3dvcmQ6"); continue; }
          if (loginStep === 2) { loginStep = 0; reply(line === b64(password) ? "235 ok" : "535 no"); continue; }
          const cmd = line.split(" ")[0].toUpperCase();
          state.log.push(cmd);
          if (cmd === "EHLO") reply(`250-smtp.example.com`, ...(!secure && starttls ? ["250-STARTTLS"] : []), `250 AUTH ${mech}`);
          else if (cmd === "STARTTLS") reply("220 go ahead");
          else if (cmd === "AUTH" && line.startsWith("AUTH PLAIN ")) reply(line.slice(11) === b64(`\0user\0${password}`) ? "235 ok" : "535 bad credentials");
          else if (cmd === "AUTH") { loginStep = 1; reply("334 VXNlcm5hbWU6"); }
          else if (cmd === "MAIL") reply("250 ok");
          else if (cmd === "RCPT") reply(line.includes("reject@") ? "550 no such user" : "250 ok");
          else if (cmd === "DATA") { inData = true; body = ""; reply("354 go"); }
          else if (cmd === "QUIT") reply("221 bye");
          else reply("502 unknown");
        }
      },
    });
    return {
      opened: Promise.resolve(), closed: new Promise(() => {}), readable, writable,
      close: async () => {},
      startTls: () => { state.tlsUpgrades++; return makeSocket(true); },
      greet: () => greet && reply("220 smtp.example.com ESMTP"),
    };
  }
  state.connect = (address, options) => {
    state.connects.push({ address, options });
    const s = makeSocket(options.secureTransport === "on");
    s.greet();
    return s;
  };
  return state;
}

const request = (over = {}) => JSON.stringify({
  host: "smtp.example.com", port: 465, security: "tls", username: "user", password: "pw",
  from: "bot@example.com", to: ["u@example.com"], timeoutMs: 2000,
  message: b64("Subject: hi\r\n\r\nline\r\n.leading dot\r\n"), ...over,
});

const send = async (server, over) => JSON.parse(await createSMTPBridge(server.connect)(request(over)));

test("implicit TLS on 465 delivers with dot-stuffing", async () => {
  const server = fakeServer();
  assert.deepEqual(await send(server), { ok: true });
  assert.deepEqual(server.connects[0], { address: { hostname: "smtp.example.com", port: 465 }, options: { secureTransport: "on" } });
  assert.deepEqual(server.log.slice(0, 5), ["EHLO", "AUTH", "MAIL", "RCPT", "DATA"]);
  assert.equal(server.data, "Subject: hi\r\n\r\nline\r\n..leading dot\r\n");
  assert.equal(server.tlsUpgrades, 0);
});

test("STARTTLS on 587 upgrades before AUTH (LOGIN)", async () => {
  const server = fakeServer({ mech: "LOGIN" });
  assert.deepEqual(await send(server, { port: 587, security: "starttls" }), { ok: true });
  assert.equal(server.connects[0].options.secureTransport, "starttls");
  assert.equal(server.tlsUpgrades, 1);
  assert.deepEqual(server.log.slice(0, 4), ["EHLO", "STARTTLS", "EHLO", "AUTH"]);
});

test("missing STARTTLS is refused before credentials are sent", async () => {
  const server = fakeServer({ starttls: false });
  assert.deepEqual(await send(server, { port: 587, security: "starttls" }), { ok: false, kind: "smtp_starttls_required", permanent: false });
  assert.ok(!server.log.includes("AUTH"));
});

test("AUTH failure stops the dialogue", async () => {
  const server = fakeServer({ password: "other" });
  const res = await send(server);
  assert.equal(res.ok, false);
  assert.equal(res.kind, "smtp_authentication_failed");
  assert.ok(!server.log.includes("MAIL"));
});

test("recipient 5xx is permanent", async () => {
  const res = await send(fakeServer(), { to: ["reject@example.com"] });
  assert.deepEqual(res, { ok: false, kind: "smtp_recipient_rejected", permanent: true });
});

test("CRLF injection is rejected without connecting", async () => {
  for (const over of [{ to: ["u@example.com>\r\nRCPT TO:<x@example.com"] }, { from: "a@example.com\r\nDATA" },
    { username: "user\r\nQUIT" }, { host: "smtp.example.com\r\n" }]) {
    const server = fakeServer();
    const res = await send(server, over);
    assert.equal(res.ok, false);
    assert.equal(res.permanent, true);
    assert.equal(server.connects.length, 0);
  }
});

test("port/security mismatch is refused", async () => {
  const server = fakeServer();
  assert.equal((await send(server, { port: 25 })).kind, "invalid_smtp_endpoint");
  assert.equal((await send(server, { port: 587 })).kind, "invalid_smtp_endpoint");
  assert.equal(server.connects.length, 0);
});

test("silent server times out", async () => {
  const res = await send(fakeServer({ greet: false }), { timeoutMs: 50 });
  assert.deepEqual(res, { ok: false, kind: "smtp_timeout", permanent: false });
});
