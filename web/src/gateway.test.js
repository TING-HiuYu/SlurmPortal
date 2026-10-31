import { describe, expect, it } from "vitest";
import { webcrypto } from "node:crypto";
import { gatewayBase, socketURL, parentDirectory, openSSHPublicKey, generateKeyPair, formatBytes } from "./gateway.js";

describe("gateway client", () => {
  it("encodes Ed25519 public keys in OpenSSH wire format", () => {
    expect(openSSHPublicKey(new Uint8Array(32))).toBe("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA");
    expect(() => openSSHPublicKey(new Uint8Array(31))).toThrow();
  });
  it("generates a PKCS#8 private key and matching OpenSSH public key", async () => {
    const pair = await generateKeyPair(webcrypto.subtle);
    expect(pair.publicKey).toMatch(/^ssh-ed25519 [A-Za-z0-9+/]{68}$/);
    expect(pair.privateKey).toMatch(/^-----BEGIN PRIVATE KEY-----\n[\s\S]+\n-----END PRIVATE KEY-----$/);
  });
  it("only accepts plain HTTPS gateway origins", () => {
    expect(gatewayBase("https://cluster1.example.org:19933/")).toBe("https://cluster1.example.org:19933");
    for (const bad of ["http://cluster1.example.org", "https://u:p@cluster1.example.org", "https://cluster1.example.org/?x=1"]) expect(() => gatewayBase(bad)).toThrow();
    expect(socketURL("https://cluster1.example.org:19933", "/v1/x")).toBe("wss://cluster1.example.org:19933/v1/x");
  });
  it("walks directories and formats sizes", () => {
    expect(parentDirectory("/HOME/alice/data/")).toBe("/HOME/alice");
    expect(parentDirectory("/HOME")).toBe("/");
    expect(formatBytes(1536)).toBe("1.5 KiB");
    expect(formatBytes(null)).toBe("—");
  });
});
