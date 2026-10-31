import { describe, expect, test } from "vitest";
import { besidePoint, besideRect } from "./placement.js";

const overlaps = (a, b) => a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;

describe("besidePoint", () => {
  const viewport = { width: 1200, height: 800 };
  const size = { width: 300, height: 200 };
  test("goes right of the pointer with a gap", () => {
    const p = besidePoint({ x: 100, y: 300 }, size, viewport);
    expect(p.side).toBe("right");
    expect(p.left).toBe(118);
  });
  test("flips left near the right edge and never covers the pointer", () => {
    for (const x of [900, 1000, 1190]) {
      const p = besidePoint({ x, y: 400 }, size, viewport);
      expect(p.side).toBe("left");
      expect(overlaps({ x: p.left, y: p.top, w: 300, h: 200 }, { x: x - 1, y: 399, w: 2, h: 2 })).toBe(false);
    }
  });
  test("falls back below on narrow screens", () => {
    const p = besidePoint({ x: 180, y: 100 }, size, { width: 360, height: 800 });
    expect(p.side).toBe("bottom");
    expect(p.top).toBeGreaterThan(100);
  });
});

describe("besideRect", () => {
  const box = { width: 1000, height: 600 };
  const size = { width: 280, height: 220 };
  test("never overlaps the selected rectangle", () => {
    for (const rect of [{ x: 20, y: 20 }, { x: 760, y: 20 }, { x: 760, y: 500 }, { x: 400, y: 260 }].map((r) => ({ ...r, width: 210, height: 58 }))) {
      const p = besideRect(rect, size, box);
      expect(overlaps({ x: p.left, y: p.top, w: size.width, h: size.height }, { x: rect.x, y: rect.y, w: rect.width, h: rect.height })).toBe(false);
    }
  });
  test("a node at the top right gets the panel on its left", () => {
    expect(besideRect({ x: 760, y: 20, width: 210, height: 58 }, size, box).side).toBe("left");
  });
  test("prefers a side that leaves other nodes uncovered", () => {
    const rect = { x: 300, y: 200, width: 210, height: 58 };
    const neighbour = { x: 560, y: 180, width: 210, height: 120 };
    const p = besideRect(rect, size, box, 16, 12, [neighbour]);
    expect(overlaps({ x: p.left, y: p.top, w: size.width, h: size.height }, { x: neighbour.x, y: neighbour.y, w: neighbour.width, h: neighbour.height })).toBe(false);
    expect(overlaps({ x: p.left, y: p.top, w: size.width, h: size.height }, { x: rect.x, y: rect.y, w: rect.width, h: rect.height })).toBe(false);
  });
});
