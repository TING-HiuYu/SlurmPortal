// SMTP transport for the Go/Wasm master (globalThis.goV2SMTP). Go owns policy, recipients and the
// message; this module only runs one SMTP submission over cloudflare:sockets with protocol safety:
// implicit TLS on 465 or mandatory STARTTLS on 587, AUTH only over TLS, bounded replies, total deadline.
const enc = new TextEncoder();
const MAX_REPLY_BYTES = 65536;
const MAX_LINE = 8192;

class SMTPError extends Error {
  constructor(kind, permanent = false) { super(kind); this.kind = kind; this.permanent = permanent; }
}

const line = (v) => typeof v === "string" && v.length > 0 && v.length <= 4096 && !/[\r\n\0]/.test(v);
const address = (v) => typeof v === "string" && v.length <= 254 && /^[\x21-\x7e]+@[\x21-\x7e]+$/.test(v) && !/[<>]/.test(v);

function bytesToB64(bytes) {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(s);
}

function b64ToBytes(s) {
  const bin = atob(s), out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

// dotStuff doubles leading dots and appends the end-of-data marker.
function dotStuff(bytes) {
  const out = new Uint8Array(bytes.length * 2 + 5);
  let n = 0, start = true;
  for (const b of bytes) {
    if (start && b === 0x2e) out[n++] = 0x2e;
    out[n++] = b;
    start = b === 0x0a;
  }
  if (!(n >= 2 && out[n - 2] === 0x0d && out[n - 1] === 0x0a)) { out[n++] = 0x0d; out[n++] = 0x0a; }
  out.set([0x2e, 0x0d, 0x0a], n);
  return out.subarray(0, n + 3);
}

function checked(req) {
  if (!req || typeof req !== "object") throw new SMTPError("invalid_request", true);
  const endpointOK = typeof req.host === "string" && /^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$/.test(req.host) &&
    ((req.security === "tls" && req.port === 465) || (req.security === "starttls" && req.port === 587));
  if (!endpointOK) throw new SMTPError("invalid_smtp_endpoint", true);
  if (!line(req.username) || typeof req.password !== "string" || !req.password || req.password.length > 4096 || req.password.includes("\0")) {
    throw new SMTPError("smtp_credentials_missing", true);
  }
  if (!address(req.from) || !Array.isArray(req.to) || req.to.length < 1 || req.to.length > 16 || !req.to.every(address)) {
    throw new SMTPError("invalid_mail_address", true);
  }
  if (!Number.isSafeInteger(req.timeoutMs) || req.timeoutMs < 1 || req.timeoutMs > 15000) throw new SMTPError("invalid_request", true);
  let message;
  try { message = b64ToBytes(req.message); } catch { throw new SMTPError("invalid_request", true); }
  if (!message.length || message.length > 512 * 1024) throw new SMTPError("mail_too_large", true);
  return { ...req, message };
}

async function submit(connect, req) {
  let socket, reader, writer, timer, buffer = "", replyBytes = 0;
  const decoder = new TextDecoder();
  const release = () => {
    try { reader?.releaseLock(); } catch { /* closed */ }
    try { writer?.releaseLock(); } catch { /* closed */ }
    reader = writer = undefined;
  };
  const attach = (value) => {
    socket = value;
    socket.closed?.catch(() => {});
    reader = socket.readable.getReader();
    writer = socket.writable.getWriter();
    buffer = "";
  };
  async function readLine() {
    while (!buffer.includes("\r\n")) {
      const part = await reader.read();
      if (part.done) throw new SMTPError("smtp_connection_closed");
      if (!part.value?.byteLength) throw new SMTPError("smtp_invalid_reply");
      replyBytes += part.value.byteLength;
      if (replyBytes > MAX_REPLY_BYTES) throw new SMTPError("smtp_reply_too_large");
      buffer += decoder.decode(part.value, { stream: true });
      if (buffer.length > MAX_LINE && !buffer.includes("\r\n")) throw new SMTPError("smtp_line_too_large");
    }
    const i = buffer.indexOf("\r\n");
    if (i > MAX_LINE) throw new SMTPError("smtp_line_too_large");
    const value = buffer.slice(0, i);
    buffer = buffer.slice(i + 2);
    return value;
  }
  async function readReply() {
    const lines = [];
    let code;
    for (let count = 0; count < 100; count++) {
      const m = /^(\d{3})([- ])(.*)$/.exec(await readLine());
      if (!m || (code && code !== Number(m[1]))) throw new SMTPError("smtp_invalid_reply");
      code = Number(m[1]);
      lines.push(m[3]);
      if (m[2] === " ") return { code, lines };
    }
    throw new SMTPError("smtp_reply_too_large");
  }
  async function expect(codes, kind = "smtp_rejected") {
    const r = await readReply();
    if (!codes.includes(r.code)) throw new SMTPError(kind, r.code >= 500);
    return r.lines;
  }
  const write = (v) => writer.write(typeof v === "string" ? enc.encode(v) : v);
  const command = async (v, codes, kind) => { await write(`${v}\r\n`); return expect(codes, kind); };

  const task = async () => {
    attach(connect({ hostname: req.host, port: req.port }, { secureTransport: req.security === "tls" ? "on" : "starttls" }));
    await socket.opened;
    await expect([220]);
    let caps = await command("EHLO go-ldap-admin.invalid", [250]);
    if (req.security === "starttls") {
      if (!caps.some((v) => /^STARTTLS(?:\s|$)/i.test(v))) throw new SMTPError("smtp_starttls_required");
      await command("STARTTLS", [220], "smtp_tls_failed");
      // Plaintext queued behind the TLS go-ahead would be an injection attempt.
      if (buffer.length) throw new SMTPError("smtp_unexpected_plaintext");
      const insecure = socket;
      release();
      attach(insecure.startTls());
      await socket.opened;
      caps = await command("EHLO go-ldap-admin.invalid", [250]);
    }
    const auth = caps.find((v) => /^AUTH(?:[ =]|$)/i.test(v)) || "";
    if (/\bPLAIN\b/i.test(auth)) {
      const token = bytesToB64(enc.encode(`\0${req.username}\0${req.password}`));
      await write(`AUTH PLAIN ${token}\r\n`);
      const first = await readReply();
      if (first.code === 334) await command(token, [235], "smtp_authentication_failed");
      else if (first.code !== 235) throw new SMTPError("smtp_authentication_failed");
    } else if (/\bLOGIN\b/i.test(auth)) {
      await command("AUTH LOGIN", [334], "smtp_authentication_failed");
      await command(bytesToB64(enc.encode(req.username)), [334], "smtp_authentication_failed");
      await command(bytesToB64(enc.encode(req.password)), [235], "smtp_authentication_failed");
    } else throw new SMTPError("smtp_authentication_unsupported");
    await command(`MAIL FROM:<${req.from}>`, [250], "smtp_sender_rejected");
    for (const to of req.to) await command(`RCPT TO:<${to}>`, [250, 251], "smtp_recipient_rejected");
    await command("DATA", [354]);
    await write(dotStuff(req.message));
    await expect([250], "smtp_data_rejected");
    // DATA acceptance is the delivery boundary; QUIT is best effort.
    write("QUIT\r\n").catch(() => {});
  };
  try {
    await Promise.race([task(), new Promise((_, reject) => {
      timer = setTimeout(() => {
        try { socket?.close()?.catch?.(() => {}); } catch { /* closed */ }
        reject(new SMTPError("smtp_timeout"));
      }, req.timeoutMs);
    })]);
  } finally {
    clearTimeout(timer);
    release();
    try { socket?.close()?.catch?.(() => {}); } catch { /* closed */ }
  }
}

// createSMTPBridge returns goV2SMTP(requestJSON) -> Promise<resultJSON>; it never rejects and never logs payloads.
export function createSMTPBridge(connect) {
  return async function goV2SMTP(requestJSON) {
    try {
      await submit(connect, checked(JSON.parse(requestJSON)));
      return JSON.stringify({ ok: true });
    } catch (error) {
      const e = error instanceof SMTPError ? error : new SMTPError("smtp_unavailable");
      return JSON.stringify({ ok: false, kind: e.kind, permanent: e.permanent });
    }
  };
}
