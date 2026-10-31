import { StrictMode } from "react";
import { locale } from "./i18n.js";
import { createRoot } from "react-dom/client";
import "./style.css";
import "./theme.js";
import Root from "./Root.jsx";

// A dialog's onOk that fails antd form validation rejects with { errorFields }; the form already
// shows the errors inline, so that rejection is not an application error.
window.addEventListener("unhandledrejection", (event) => {
  if (event.reason && Array.isArray(event.reason.errorFields)) event.preventDefault();
});

document.documentElement.lang = locale === "en" ? "en" : "zh-CN";

// After a deploy, a tab still running the previous build asks for chunks that no longer exist.
// Reload once to pick up the new build; the timestamp guard prevents a reload loop.
window.addEventListener("vite:preloadError", (event) => {
  try {
    const last = Number(sessionStorage.getItem("gla.chunk-reload") || 0);
    if (Date.now() - last < 30000) return;
    sessionStorage.setItem("gla.chunk-reload", String(Date.now()));
  } catch {
    /* storage unavailable: still try one reload */
  }
  event.preventDefault();
  window.location.reload();
});

createRoot(document.getElementById("app")).render(
  <StrictMode>
    <Root />
  </StrictMode>,
);
