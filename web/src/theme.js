// Theme mode (system / light / dark), remembered per browser. html.dark switches the console tokens.
import { useSyncExternalStore } from "react";
import { t } from "./i18n.js";

const KEY = "gla.theme";
const media = typeof window !== "undefined" && window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;
const listeners = new Set();

function read() {
  try {
    const v = localStorage.getItem(KEY);
    return ["light", "dark", "system"].includes(v) ? v : "system";
  } catch {
    return "system";
  }
}

let snapshot = { mode: read(), dark: false };

function apply() {
  const dark = snapshot.mode === "dark" || (snapshot.mode === "system" && !!media?.matches);
  snapshot = { ...snapshot, dark };
  document.documentElement.classList.toggle("dark", dark);
  listeners.forEach((l) => l());
}

export function setThemeMode(mode) {
  try {
    localStorage.setItem(KEY, mode);
  } catch {
    /* per-browser preference only */
  }
  snapshot = { ...snapshot, mode };
  apply();
}

export const cycleTheme = () => setThemeMode({ system: "light", light: "dark", dark: "system" }[snapshot.mode]);
export const themeLabel = (mode) => ({ system: t("主题：跟随系统"), light: t("主题：浅色"), dark: t("主题：深色") })[mode];
media?.addEventListener?.("change", apply);
apply();

export const useTheme = () => useSyncExternalStore((l) => (listeners.add(l), () => listeners.delete(l)), () => snapshot);
