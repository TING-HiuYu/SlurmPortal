import { describe, expect, it } from "vitest";
import {
  buildCertificateBundle,
  certificateBundleFilename,
  certificateFilename,
  crc32,
  privateKeyFilename,
} from "./certificate.js";

const decoder = new TextDecoder();

function parseZip(bytes) {
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  const eocdOffset = bytes.length - 22;
  expect(view.getUint32(eocdOffset, true)).toBe(0x06054b50);
  const count = view.getUint16(eocdOffset + 10, true);
  const cdSize = view.getUint32(eocdOffset + 12, true);
  const cdOffset = view.getUint32(eocdOffset + 16, true);
  expect(cdOffset + cdSize).toBe(eocdOffset);

  const entries = [];
  let cursor = cdOffset;
  for (let i = 0; i < count; i++) {
    expect(view.getUint32(cursor, true)).toBe(0x02014b50);
    const method = view.getUint16(cursor + 10, true);
    const cdCrc = view.getUint32(cursor + 16, true);
    const compSize = view.getUint32(cursor + 20, true);
    const uncompSize = view.getUint32(cursor + 24, true);
    const nameLen = view.getUint16(cursor + 28, true);
    const extraLen = view.getUint16(cursor + 30, true);
    const commentLen = view.getUint16(cursor + 32, true);
    const externalAttr = view.getUint32(cursor + 38, true);
    const localOffset = view.getUint32(cursor + 42, true);
    const name = decoder.decode(bytes.subarray(cursor + 46, cursor + 46 + nameLen));

    expect(view.getUint32(localOffset, true)).toBe(0x04034b50);
    const localNameLen = view.getUint16(localOffset + 26, true);
    const localExtraLen = view.getUint16(localOffset + 28, true);
    const localName = decoder.decode(bytes.subarray(localOffset + 30, localOffset + 30 + localNameLen));
    expect(localName).toBe(name);

    const dataStart = localOffset + 30 + localNameLen + localExtraLen;
    const content = bytes.subarray(dataStart, dataStart + compSize);
    expect(method).toBe(0);
    expect(compSize).toBe(uncompSize);
    expect(crc32(content)).toBe(cdCrc);
    expect(view.getUint32(localOffset + 14, true)).toBe(cdCrc);

    entries.push({
      name,
      mode: (externalAttr >>> 16) & 0o777,
      content: decoder.decode(content),
    });
    cursor += 46 + nameLen + extraLen + commentLen;
  }
  expect(cursor).toBe(cdOffset + cdSize);
  return entries;
}

describe("certificate bundle", () => {
  it("uses v1 filenames with empty username fallback", () => {
    expect(certificateFilename("alice", "ssh")).toBe("alice-cert.pub");
    expect(privateKeyFilename("alice", "ssh")).toBe("alice");
    expect(certificateFilename("alice", "x509")).toBe("alice.crt.pem");
    expect(privateKeyFilename("alice", "x509")).toBe("alice.key.pem");
    expect(certificateBundleFilename("alice")).toBe("alice-certificate.zip");

    expect(certificateFilename("", "ssh")).toBe("user-cert.pub");
    expect(privateKeyFilename("  ", "ssh")).toBe("user");
    expect(certificateBundleFilename(null)).toBe("user-certificate.zip");
  });

  it("throws when certificate or private key is missing", () => {
    expect(() => buildCertificateBundle("alice", null)).toThrow();
    expect(() => buildCertificateBundle("alice", { certificatePem: "cert", privateKeyPem: "" })).toThrow();
    expect(() => buildCertificateBundle("alice", { certificatePem: "", privateKeyPem: "key" })).toThrow();
  });

  it("builds a valid v1 SSH zip archive", () => {
    const certPem = "ssh-ed25519-cert-v01@openssh.com AAAATEST\n";
    const keyPem = "-----BEGIN OPENSSH PRIVATE KEY-----\ntest\n-----END OPENSSH PRIVATE KEY-----\n";
    const bundle = buildCertificateBundle("ting-hiuyu", {
      kind: "ssh",
      certificatePem: certPem,
      privateKeyPem: keyPem,
    }, new Date(2026, 10, 28, 1, 30, 0));

    expect(bundle.filename).toBe("ting-hiuyu-certificate.zip");
    const entries = parseZip(bundle.data);
    expect(entries).toEqual([
      { name: "ting-hiuyu", mode: 0o600, content: keyPem },
      { name: "ting-hiuyu-cert.pub", mode: 0o644, content: certPem },
    ]);
  });

  it("builds a valid v1 X.509 zip archive", () => {
    const certPem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n";
    const keyPem = "-----BEGIN EC PRIVATE KEY-----\nMHcC\n-----END EC PRIVATE KEY-----\n";
    const bundle = buildCertificateBundle("alice", {
      kind: "x509",
      certificatePem: certPem,
      privateKeyPem: keyPem,
    }, new Date(2026, 10, 28, 1, 30, 0));

    expect(bundle.filename).toBe("alice-certificate.zip");
    const entries = parseZip(bundle.data);
    expect(entries).toEqual([
      { name: "alice.key.pem", mode: 0o600, content: keyPem },
      { name: "alice.crt.pem", mode: 0o644, content: certPem },
    ]);
  });
});
