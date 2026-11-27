import { download } from "./hooks.js";

const encoder = new TextEncoder();

const CRC_TABLE = (() => {
  const table = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let bit = 0; bit < 8; bit++) {
      c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    }
    table[i] = c >>> 0;
  }
  return table;
})();

export function crc32(bytes) {
  let crc = 0xffffffff;
  for (let i = 0; i < bytes.length; i++) {
    crc = CRC_TABLE[(crc ^ bytes[i]) & 0xff] ^ (crc >>> 8);
  }
  return (crc ^ 0xffffffff) >>> 0;
}

export function normalizeUsername(username) {
  const base = String(username || "").trim();
  return base || "user";
}

export function certificateFilename(username, kind) {
  const base = normalizeUsername(username);
  return kind === "ssh" ? `${base}-cert.pub` : `${base}.crt.pem`;
}

export function privateKeyFilename(username, kind) {
  const base = normalizeUsername(username);
  return kind === "ssh" ? base : `${base}.key.pem`;
}

export function certificateBundleFilename(username) {
  return `${normalizeUsername(username)}-certificate.zip`;
}

function dosTimestamp(date) {
  const d = date instanceof Date && !Number.isNaN(date.getTime()) ? date : new Date();
  const year = Math.min(127, Math.max(0, d.getFullYear() - 1980));
  const dosTime = ((d.getHours() & 0x1f) << 11) | ((d.getMinutes() & 0x3f) << 5) | ((d.getSeconds() >> 1) & 0x1f);
  const dosDate = (year << 9) | (((d.getMonth() + 1) & 0x0f) << 5) | (d.getDate() & 0x1f);
  return { dosTime, dosDate };
}

function toBytes(content) {
  if (content instanceof Uint8Array) return content;
  return encoder.encode(typeof content === "string" ? content : String(content ?? ""));
}

// buildZip writes a standard PKWARE STORE ZIP archive with UTF-8 filenames and UNIX permissions.
export function buildZip(entries, now = new Date()) {
  const { dosTime, dosDate } = dosTimestamp(now);
  const localChunks = [];
  const centralChunks = [];
  let offset = 0;

  for (const entry of entries) {
    const nameBytes = encoder.encode(entry.name);
    const data = toBytes(entry.content);
    const crc = crc32(data);
    const mode = entry.mode ?? 0o644;
    const externalAttr = ((0o100000 | (mode & 0o777)) << 16) >>> 0;

    const local = new ArrayBuffer(30 + nameBytes.length);
    const lv = new DataView(local);
    lv.setUint32(0, 0x04034b50, true);
    lv.setUint16(4, 20, true);
    lv.setUint16(6, 0x0800, true);
    lv.setUint16(8, 0, true);
    lv.setUint16(10, dosTime, true);
    lv.setUint16(12, dosDate, true);
    lv.setUint32(14, crc, true);
    lv.setUint32(18, data.length, true);
    lv.setUint32(22, data.length, true);
    lv.setUint16(26, nameBytes.length, true);
    lv.setUint16(28, 0, true);
    new Uint8Array(local, 30).set(nameBytes);

    localChunks.push(new Uint8Array(local), data);

    const central = new ArrayBuffer(46 + nameBytes.length);
    const cv = new DataView(central);
    cv.setUint32(0, 0x02014b50, true);
    cv.setUint16(4, 0x0314, true);
    cv.setUint16(6, 20, true);
    cv.setUint16(8, 0x0800, true);
    cv.setUint16(10, 0, true);
    cv.setUint16(12, dosTime, true);
    cv.setUint16(14, dosDate, true);
    cv.setUint32(16, crc, true);
    cv.setUint32(20, data.length, true);
    cv.setUint32(24, data.length, true);
    cv.setUint16(28, nameBytes.length, true);
    cv.setUint16(30, 0, true);
    cv.setUint16(32, 0, true);
    cv.setUint16(34, 0, true);
    cv.setUint16(36, 0, true);
    cv.setUint32(38, externalAttr, true);
    cv.setUint32(42, offset, true);
    new Uint8Array(central, 46).set(nameBytes);

    centralChunks.push(new Uint8Array(central));
    offset += 30 + nameBytes.length + data.length;
  }

  let centralSize = 0;
  for (const chunk of centralChunks) centralSize += chunk.length;

  const eocd = new ArrayBuffer(22);
  const ev = new DataView(eocd);
  ev.setUint32(0, 0x06054b50, true);
  ev.setUint16(4, 0, true);
  ev.setUint16(6, 0, true);
  ev.setUint16(8, entries.length, true);
  ev.setUint16(10, entries.length, true);
  ev.setUint32(12, centralSize, true);
  ev.setUint32(16, offset, true);
  ev.setUint16(20, 0, true);

  const total = offset + centralSize + 22;
  const out = new Uint8Array(total);
  let pos = 0;
  for (const chunk of localChunks) {
    out.set(chunk, pos);
    pos += chunk.length;
  }
  for (const chunk of centralChunks) {
    out.set(chunk, pos);
    pos += chunk.length;
  }
  out.set(new Uint8Array(eocd), pos);
  return out;
}

export function buildCertificateBundle(username, issued, now = new Date()) {
  if (!issued?.certificatePem || !issued?.privateKeyPem) {
    throw new Error("missing certificate or private key");
  }
  return {
    filename: certificateBundleFilename(username),
    data: buildZip(
      [
        { name: privateKeyFilename(username, issued.kind), content: issued.privateKeyPem, mode: 0o600 },
        { name: certificateFilename(username, issued.kind), content: issued.certificatePem, mode: 0o644 },
      ],
      now,
    ),
  };
}

export function downloadCertificate(username, cert) {
  download(cert.certificatePem, certificateFilename(username, cert.kind), "text/plain");
}

export function downloadCertificateBundle(username, issued) {
  const { filename, data } = buildCertificateBundle(username, issued);
  download(data, filename, "application/zip");
}
