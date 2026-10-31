import { useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { besidePoint } from "../placement.js";

// A hover card pinned beside the pointer (right, else left, else below/above, v1 behaviour). It is
// portalled to <body>, ignores the pointer and never covers the pointer or the mark under it.
export default function FloatCard({ point, children, width = 260 }) {
  const ref = useRef(null);
  const [pos, setPos] = useState(null);
  useLayoutEffect(() => {
    if (!point || !ref.current) return;
    const viewport = { width: document.documentElement.clientWidth || window.innerWidth, height: window.innerHeight };
    setPos(besidePoint(point, { width: ref.current.offsetWidth, height: ref.current.offsetHeight }, viewport));
  }, [point, children]);
  if (!point) return null;
  return createPortal(
    <div ref={ref} className="float-card" role="tooltip" data-side={pos?.side}
      style={{ width, left: pos?.left ?? -9999, top: pos?.top ?? -9999, maxWidth: pos?.maxWidth, maxHeight: pos?.maxHeight }}>
      {children}
    </div>,
    document.body,
  );
}
