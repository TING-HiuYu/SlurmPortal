import { useEffect, useId, useState } from "react";
import "./AuthClusterScene.css";

// Decorative login illustration from the v1 portal; ids are namespaced so two instances never collide.
export default function AuthClusterScene({ className = "" }) {
  const ns = `auth-cluster-scene-${useId().replace(/[^a-zA-Z0-9]/g, "")}`;
  const [paused, setPaused] = useState(false);
  const id = (name) => `${ns}-${name}`;
  const paint = (name) => `url(#${id(name)})`;
  useEffect(() => {
    const sync = () => setPaused(document.hidden);
    sync();
    document.addEventListener("visibilitychange", sync);
    return () => document.removeEventListener("visibilitychange", sync);
  }, []);
  return (
    <div className={`auth-cluster-scene ${paused ? "is-paused" : ""} ${className}`} aria-hidden="true">
      <svg viewBox="0 0 720 480" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false" role="presentation" preserveAspectRatio="xMidYMid meet">
      <defs>
        <linearGradient id={id('top')} x1="0" y1="0" x2="1" y2="1"><stop stopColor="#D5E2FF" stopOpacity=".27" /><stop offset=".5" stopColor="#889DF4" stopOpacity=".12" /><stop offset="1" stopColor="#536CE0" stopOpacity=".32" /></linearGradient>
        <linearGradient id={id('left')} x1="0" y1="0" x2="1" y2="1"><stop stopColor="#A5BAFF" stopOpacity=".18" /><stop offset="1" stopColor="#354DAF" stopOpacity=".08" /></linearGradient>
        <linearGradient id={id('right')} x1="0" y1="0" x2="1" y2="1"><stop stopColor="#7F9AF6" stopOpacity=".27" /><stop offset="1" stopColor="#5267CA" stopOpacity=".1" /></linearGradient>
        <linearGradient id={id('edge')} x1="0" y1="0" x2="1" y2="1"><stop stopColor="#E3ECFF" stopOpacity=".78" /><stop offset=".5" stopColor="#9EB7FF" stopOpacity=".34" /><stop offset="1" stopColor="#637EEE" stopOpacity=".48" /></linearGradient>
        <linearGradient id={id('slot')} x1="0" y1="0" x2="1" y2="1"><stop stopColor="#D0DEFF" stopOpacity=".13" /><stop offset="1" stopColor="#8BA6FF" stopOpacity=".03" /></linearGradient>
        <linearGradient id={id('network')} x1="0" y1="0" x2="1" y2="0"><stop stopColor="#5B75E6" stopOpacity=".08" /><stop offset=".35" stopColor="#91ACFF" stopOpacity=".85" /><stop offset=".7" stopColor="#93B7FF" stopOpacity=".72" /><stop offset="1" stopColor="#5B75E6" stopOpacity=".12" /></linearGradient>
        <radialGradient id={id('haze')}><stop stopColor="#5B75E6" stopOpacity=".32" /><stop offset=".55" stopColor="#6279E2" stopOpacity=".12" /><stop offset="1" stopColor="#5B75E6" stopOpacity="0" /></radialGradient>
        <radialGradient id={id('groundLight')}><stop stopColor="#8CA8FF" stopOpacity=".22" /><stop offset="1" stopColor="#5B75E6" stopOpacity="0" /></radialGradient>
        <filter id={id('soft')} x="-50%" y="-80%" width="200%" height="260%" colorInterpolationFilters="sRGB"><feGaussianBlur stdDeviation="22" /></filter>
        <filter id={id('glow')} x="-150%" y="-150%" width="400%" height="400%" colorInterpolationFilters="sRGB"><feGaussianBlur stdDeviation="3.2" result="blur" /><feMerge><feMergeNode in="blur" /><feMergeNode in="SourceGraphic" /></feMerge></filter>
        <pattern id={id('grid')} width="36" height="36" patternUnits="userSpaceOnUse" patternTransform="matrix(1 .56 -1 .56 360 130)"><path d="M36 0H0V36" fill="none" stroke="#8DA4F4" strokeOpacity=".16" strokeWidth=".8" /></pattern>
        <clipPath id={id('floor')}><path d="M67 307 349 150 668 330 386 487Z" /></clipPath>
        <g id={id('server')} strokeLinejoin="round">
          
          <path d="M0-40V84L-70 124M0 84 70 124" fill="none" stroke="#94ABFC" strokeOpacity=".12" strokeWidth=".8" strokeDasharray="3 5" />
          <path d="M-70 0 0 40V164L-70 124Z" fill={paint('left')} stroke={paint('edge')} strokeWidth=".9" />
          <path d="M0 40 70 0V124L0 164Z" fill={paint('right')} stroke={paint('edge')} strokeWidth=".9" />
          <path d="M-70 0 0-40 70 0 0 40Z" fill={paint('top')} stroke={paint('edge')} strokeWidth="1" />
          <path d="M-57 24-13 49V66L-57 41ZM-57 56-13 81V98L-57 73ZM-57 88-13 113V130L-57 105Z" fill={paint('slot')} stroke="#A3B9FF" strokeOpacity=".19" strokeWidth=".6" />
          <path d="M13 49 57 24V41L13 66ZM13 81 57 56V73L13 98ZM13 113 57 88V105L13 130Z" fill={paint('slot')} stroke="#B4C7FF" strokeOpacity=".18" strokeWidth=".6" />
          <path d="m-48 34 24 14m-24 18 24 14m-24 18 24 14" fill="none" stroke="#D6E4FF" strokeOpacity=".38" strokeWidth="1.15" strokeLinecap="round" />
          <path d="m23 51 23-13m-23 45 23-13m-23 45 23-13" fill="none" stroke="#B7CCFF" strokeOpacity=".65" strokeWidth="1.1" strokeLinecap="round" />
          <g className="server-lights" fill="#BFD7FF" filter={paint('glow')}><circle cx="52" cy="35" r="1.55" /><circle cx="52" cy="67" r="1.55" /><circle cx="52" cy="99" r="1.55" /></g>
          <path d="M-44 0 0-25 44 0 0 25Z" fill="#C8D9FF" fillOpacity=".06" stroke="#B0C5FF" strokeOpacity=".29" strokeWidth=".7" />
          <path d="M-27 0 0-15 27 0 0 15Z" fill="#6685F0" fillOpacity=".15" stroke="#AAC4FF" strokeOpacity=".42" strokeWidth=".7" />
          <path d="M-12 0 0-7 12 0 0 7Z" fill="#A7C0FF" fillOpacity=".62" filter={paint('glow')} />
          <path d="M-70 110 0 150 70 110" fill="none" stroke="#7F9BFF" strokeOpacity=".47" strokeWidth="1.3" />
          <path d="M0 41V157" fill="none" stroke="#D5E5FF" strokeOpacity=".34" strokeWidth=".7" />
        </g>
      </defs>

      <g className="scene-atmosphere">
        <ellipse cx="364" cy="224" rx="262" ry="146" fill={paint('haze')} transform="rotate(-9 364 224)" />
        <path d="M102 252C210 116 383 141 606 239 485 205 418 285 291 292 213 299 157 286 102 252Z" fill="#6F80E7" fillOpacity=".11" filter={paint('soft')} />
        <path d="M232 339C329 225 481 176 629 203 567 213 498 276 447 325 369 370 290 371 232 339Z" fill="#7699ED" fillOpacity=".08" filter={paint('soft')} />
      </g>
      <g className="scene-floor">
        <path d="M67 307 349 150 668 330 386 487Z" fill={paint('groundLight')} opacity=".34" />
        <path d="M67 307 349 150 668 330 386 487Z" fill="none" stroke="#8099EF" strokeOpacity=".08" strokeWidth="1" />
        <path d="M45 135H686V480H45Z" fill={paint('grid')} clipPath={paint('floor')} />
        <g fill="none" stroke="#718DEB" strokeOpacity=".17" strokeWidth=".85"><path d="m93 322 264 149 275-154" /><path d="m137 297 225 127 227-128" /><path d="m186 268 183 104 167-95" /></g>
      </g>
      <g className="scene-network" fill="none" strokeLinecap="round" strokeLinejoin="round">
        <path d="m109 329 104 59 153-86 139 79 115-65M151 285l88 50 125-71 171 97M213 388v-48l-9-5M505 381v-45l24-13M366 302v67l-63 36" stroke={paint('network')} strokeWidth="1" />
        <path d="m109 329 104 59 153-86 139 79 115-65" stroke={paint('network')} strokeWidth="3" opacity=".18" filter={paint('glow')} />
        <path className="network-signal signal-a" d="m109 329 104 59 153-86 139 79 115-65" stroke="#C0D3FF" strokeWidth="1.45" strokeDasharray="18 410" strokeOpacity=".72" />
        <path className="network-signal signal-b" d="m151 285 88 50 125-71 171 97" stroke="#9BBDFB" strokeWidth="1.2" strokeDasharray="10 320" strokeOpacity=".65" />
      </g>
      <g className="network-points" filter={paint('glow')}><path d="m103 329 6-3 6 3-6 3Z" fill="#A3BCFF" fillOpacity=".75" /><path d="m207 388 6-3 6 3-6 3Z" fill="#C0D4FF" fillOpacity=".82" /><path d="m499 381 6-3 6 3-6 3Z" fill="#B0C7FF" fillOpacity=".8" /><path d="m614 316 6-3 6 3-6 3Z" fill="#A3BCFF" fillOpacity=".75" /><circle cx="303" cy="405" r="1.8" fill="#B6CBFF" /><circle cx="151" cy="285" r="1.5" fill="#A2BFFF" /></g>
      <g className="server-ground" fill={paint('groundLight')}><ellipse cx="204" cy="291" rx="98" ry="39" /><ellipse cx="365" cy="305" rx="123" ry="49" /><ellipse cx="529" cy="316" rx="84" ry="34" /></g>

      <g className="server-float float-left"><g transform="translate(204 175) scale(.82)" opacity=".78"><use href={`#${id("server")}`} /></g></g>
      <g className="server-float float-right"><g transform="translate(529 212) scale(.72)" opacity=".7"><use href={`#${id("server")}`} /></g></g>
      <g className="server-float float-main"><g transform="translate(365 151) scale(1.12)"><use href={`#${id("server")}`} /></g></g>

      <g className="scene-particles" fill="#C1D5FF">
        <circle className="spark spark-a" cx="138" cy="190" r="1.3" /><circle className="spark spark-b" cx="262" cy="94" r="1.2" /><circle className="spark spark-c" cx="447" cy="116" r="1.4" /><circle className="spark spark-a" cx="576" cy="155" r="1.1" /><circle className="spark spark-c" cx="622" cy="257" r="1.35" /><circle className="spark spark-b" cx="103" cy="273" r="1" /><circle className="spark spark-b" cx="460" cy="405" r="1.1" /><circle className="spark spark-c" cx="254" cy="433" r=".9" /><circle className="spark spark-a" cx="347" cy="73" r=".85" />
        <g className="spark spark-b" stroke="#C8D9FF" strokeWidth=".8" strokeLinecap="round"><path d="M303 126v7m-3.5-3.5h7M576 366v6m-3-3h6M155 366v5m-2.5-2.5h5" /></g>
      </g>
    </svg>
    </div>
  );
}
