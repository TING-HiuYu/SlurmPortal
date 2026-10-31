import { forwardRef, useEffect, useImperativeHandle, useRef } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

const XTerm = forwardRef(function XTerm({ readonly = false, label = "Terminal", onInput, onResize, height }, ref) {
  const el = useRef(null);
  const term = useRef(null);
  const fit = useRef(null);
  const handlers = useRef({ onInput, onResize });
  handlers.current = { onInput, onResize };

  useEffect(() => {
    const t = new Terminal({
      fontFamily: "SFMono-Regular, Consolas, 'Liberation Mono', monospace", fontSize: 13, lineHeight: 1.3, scrollback: 3000,
      cursorBlink: !readonly, disableStdin: readonly, convertEol: readonly,
      theme: { background: "#101827", foreground: "#d7e1f3", cursor: "#829bff", selectionBackground: "#425c96" },
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(el.current);
    term.current = t;
    fit.current = f;
    const subs = [t.onData((d) => handlers.current.onInput?.(new TextEncoder().encode(d))), t.onResize((s) => handlers.current.onResize?.(s))];
    const refit = () => el.current?.clientWidth && el.current?.clientHeight && f.fit();
    const o = new ResizeObserver(refit);
    o.observe(el.current);
    refit();
    return () => {
      o.disconnect();
      subs.forEach((s) => s.dispose());
      t.dispose();
      term.current = null;
    };
  }, [readonly]);

  useImperativeHandle(ref, () => ({
    write: (v) => term.current?.write(v instanceof ArrayBuffer ? new Uint8Array(v) : v),
    clear: () => term.current?.reset(),
    focus: () => term.current?.focus(),
    size: () => (term.current ? { cols: term.current.cols, rows: term.current.rows } : { cols: 100, rows: 28 }),
  }));

  return (
    <div style={{ boxSizing: "border-box", height: height || (readonly ? 340 : "calc(100vh - 280px)"), minHeight: readonly ? 220 : 320, background: "#101827", padding: "12px 6px 10px 12px", borderRadius: 8, overflow: "hidden" }}>
      <div ref={el} aria-label={label} style={{ height: "100%", width: "100%" }} />
    </div>
  );
});

export default XTerm;
