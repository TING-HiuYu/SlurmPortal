import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";
import { t } from "./i18n.js";

const files = (dir) => fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
  const p = path.join(dir, e.name);
  return e.isDirectory() ? files(p) : /\.jsx?$/.test(e.name) ? [p] : [];
});

describe("i18n", () => {
  it("fills numbered placeholders", () => {
    expect(t("共 {0} 条", 5)).toBe("共 5 条");
  });

  // A parameter or variable named t shadows the translation function: t("…") then calls a value.
  it("no file that imports t also declares a local t", () => {
    const shadow = /\(\s*t\s*[,)]|[,(]\s*t\s*\)\s*=>|(^|[^\w.])t\s*=>|\b(const|let|var)\s+t\b|\bfor\s*\(\s*(const|let)\s+t\b/m;
    const bad = files(path.dirname(new URL(import.meta.url).pathname))
      .filter((f) => !f.endsWith("i18n.js") && !f.endsWith(".test.js"))
      .filter((f) => {
        const src = fs.readFileSync(f, "utf8");
        return /import\s*\{[^}]*\bt\b[^}]*\}\s*from\s*["'][^"']*i18n\.js["']/.test(src) && shadow.test(src);
      });
    expect(bad).toEqual([]);
  });
});
