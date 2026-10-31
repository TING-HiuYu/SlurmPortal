// Floating-card placement that never covers the pointer or the thing it describes.

// besidePoint (v1 hover-overlay): prefer the pointer's right, then left, then below/above, always
// leaving `gap` around the pointer and staying `padding` inside the viewport.
export function besidePoint(point, size, viewport, gap = 18, padding = 12) {
  const width = Math.max(1, viewport.width), height = Math.max(1, viewport.height);
  const x = Math.max(padding, Math.min(point.x, width - padding));
  const y = Math.max(padding, Math.min(point.y, height - padding));
  const spaces = [
    { side: "right", w: width - x - gap - padding, h: height - padding * 2 },
    { side: "left", w: x - gap - padding, h: height - padding * 2 },
    { side: "bottom", w: width - padding * 2, h: height - y - gap - padding },
    { side: "top", w: width - padding * 2, h: y - gap - padding },
  ].map((s) => ({ ...s, w: Math.max(1, s.w), h: Math.max(1, s.h) }));
  const area = (s) => Math.min(size.width, s.w) * Math.min(size.height, s.h);
  const chosen = spaces.find((s) => s.w >= size.width && s.h >= size.height) || spaces.reduce((a, b) => (area(b) > area(a) ? b : a));
  const w = Math.min(size.width, chosen.w), h = Math.min(size.height, chosen.h);
  const clampX = (v) => Math.max(padding, Math.min(v, width - padding - w));
  const clampY = (v) => Math.max(padding, Math.min(v, height - padding - h));
  const left = chosen.side === "right" ? x + gap : chosen.side === "left" ? x - gap - w : clampX(x - w / 2);
  const top = chosen.side === "bottom" ? y + gap : chosen.side === "top" ? y - gap - h : clampY(y - 24);
  return { left, top, maxWidth: chosen.w, maxHeight: chosen.h, side: chosen.side };
}

// besideRect places a panel of `size` next to `rect` inside `box` (all in one coordinate space)
// without overlapping it. Candidates are the four sides, each aligned to the rect's start or centre;
// among those that fit, the one covering the least of `obstacles` (other nodes) wins, then the
// earlier side in right, left, bottom, top order. If none fits, the roomiest side is used.
export function besideRect(rect, size, box, gap = 16, padding = 12, obstacles = []) {
  const clampY = (v) => Math.max(padding, Math.min(v, box.height - padding - size.height));
  const clampX = (v) => Math.max(padding, Math.min(v, box.width - padding - size.width));
  const cy = rect.y + rect.height / 2 - size.height / 2, cx = rect.x + rect.width / 2 - size.width / 2;
  const candidates = [
    ["right", rect.x + rect.width + gap, [rect.y, cy]],
    ["left", rect.x - gap - size.width, [rect.y, cy]],
    ["bottom", rect.y + rect.height + gap, [rect.x, cx]],
    ["top", rect.y - gap - size.height, [rect.x, cx]],
  ].flatMap(([side, master, aligns]) => aligns.map((a) => (side === "right" || side === "left" ? { side, left: master, top: clampY(a) } : { side, left: clampX(a), top: master })));
  const inside = (c) => c.left >= padding - 0.5 && c.top >= padding - 0.5 && c.left + size.width <= box.width - padding + 0.5 && c.top + size.height <= box.height - padding + 0.5;
  const area = (c, o) => Math.max(0, Math.min(c.left + size.width, o.x + o.width) - Math.max(c.left, o.x)) * Math.max(0, Math.min(c.top + size.height, o.y + o.height) - Math.max(c.top, o.y));
  const cost = (c) => obstacles.reduce((a, o) => a + area(c, o), 0);
  const fitting = candidates.filter(inside);
  if (fitting.length) return fitting.reduce((best, c) => (cost(c) < cost(best) ? c : best));
  const room = { right: box.width - padding - (rect.x + rect.width + gap), left: rect.x - gap - padding, bottom: box.height - padding - (rect.y + rect.height + gap), top: rect.y - gap - padding };
  const side = Object.keys(room).reduce((a, b) => (room[b] > room[a] ? b : a));
  return candidates.find((c) => c.side === side);
}
