import { useEffect, useMemo, useRef, useState } from "react";
import FloatCard from "./FloatCard.jsx";

const HEIGHT = 220;
const PAD = { t: 14, r: 8, b: 30, l: 44 };
const PLOT = HEIGHT - PAD.t - PAD.b;

// Single-series bar chart: recessive grid, thin bars with rounded data ends, and a hover card
// (FloatCard) that sits beside the pointer instead of on top of the bars.
export default function BarChart({ data = [], title, unit, color = "var(--console-chart-1)", digits = 0, details }) {
  const root = useRef(null);
  const [width, setWidth] = useState(0);
  const [hover, setHover] = useState(null);
  useEffect(() => {
    const o = new ResizeObserver(([e]) => setWidth(Math.floor(e.contentRect.width)));
    o.observe(root.current);
    return () => o.disconnect();
  }, []);
  const top = useMemo(() => {
    const m = Math.max(0, ...data.map((d) => d.value));
    if (m <= 0) return 4;
    const step = 10 ** Math.floor(Math.log10(m / 4));
    return [1, 2, 2.5, 5, 10].map((k) => k * step).find((s) => s * 4 >= m) * 4;
  }, [data]);
  const band = (width - PAD.l - PAD.r) / Math.max(data.length, 1);
  const x = (i) => PAD.l + i * band;
  const y = (v) => PAD.t + PLOT - (v / top) * PLOT;
  const fmt = (v) => (digits === 0 && Number.isInteger(v) ? String(v) : v.toFixed(digits || 1));
  const bar = (i, v) => {
    const w = Math.min(Math.max(band * 0.42, 6), 34), bx = x(i) + (band - w) / 2, by = y(v), base = PAD.t + PLOT;
    const r = Math.min(4, w / 2, (base - by) / 2);
    return `M${bx},${base}V${by + r}Q${bx},${by} ${bx + r},${by}H${bx + w - r}Q${bx + w},${by} ${bx + w},${by + r}V${base}Z`;
  };
  const show = (i, e) => setHover({ i, point: { x: e.clientX, y: e.clientY } });
  const d = hover && data[hover.i];
  return (
    <div ref={root} style={{ position: "relative", width: "100%", minHeight: HEIGHT }} onMouseLeave={() => setHover(null)}>
      {width > 0 && (
        <svg width={width} height={HEIGHT} role="img" aria-label={title} style={{ display: "block", overflow: "visible" }}>
          {[0, 1, 2, 3, 4].map((k) => {
            const t = (top / 4) * k;
            return (
              <g key={k}>
                <line x1={PAD.l} x2={width - PAD.r} y1={y(t)} y2={y(t)} stroke="var(--console-chart-grid)" />
                <text x={PAD.l - 8} y={y(t) + 4} textAnchor="end" fontSize="11" fill="var(--console-muted)">{fmt(t)}</text>
              </g>
            );
          })}
          {data.map((p, i) => (
            <g key={p.label}>
              <rect x={x(i)} y={PAD.t} width={band} height={PLOT} fill="transparent" onMouseEnter={(e) => show(i, e)} onMouseMove={(e) => show(i, e)} />
              {p.value > 0 && <path d={bar(i, p.value)} fill={color} opacity={!hover || hover.i === i ? 1 : 0.5} pointerEvents="none" />}
              <text x={x(i) + band / 2} y={HEIGHT - 8} textAnchor="middle" fontSize="11" fill="var(--console-muted)">{p.label}</text>
            </g>
          ))}
        </svg>
      )}
      <FloatCard point={hover?.point}>
        {d && (
          <>
            <div style={{ fontSize: 11, letterSpacing: "0.06em" }}>{d.full || d.label}</div>
            <div style={{ display: "flex", justifyContent: "space-between", marginTop: 6, paddingBottom: details ? 8 : 0, borderBottom: details ? "1px solid var(--console-border-soft)" : 0 }}>
              <span>{title}</span><strong style={{ color: "var(--console-text)", fontVariantNumeric: "tabular-nums" }}>{fmt(d.value)} {unit}</strong>
            </div>
            {details?.(d)}
          </>
        )}
      </FloatCard>
    </div>
  );
}
