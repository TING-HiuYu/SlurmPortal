import { dateLocale, t } from "./i18n.js";
// Display helpers shared by views.
export const nodeStatus = {
  idle: [t("空闲"), "success"], full: [t("满载"), "danger"], running: [t("运行中"), "primary"], offline: [t("离线"), "info"],
  drain: [t("排空"), "warning"], maintenance: [t("维护"), "warning"], unknown: [t("未知"), "info"],
};
export const clusterStatus = {
  normal: [t("正常"), "success"], partial_maintenance: [t("部分维护"), "warning"], abnormal: [t("异常"), "danger"],
  offline: [t("离线"), "info"], unavailable: [t("无数据"), "info"],
};
export const checkStatus = { ok: [t("正常"), "success"], stale: [t("过期"), "warning"], missing: [t("无数据"), "info"], error: [t("错误"), "danger"], disabled: [t("未启用"), "info"] };

export function label(map, key) {
  return (map[key] || [key, "info"])[0];
}
export function tag(map, key) {
  return (map[key] || [key, "info"])[1];
}

export function time(v) {
  if (!v) return "—";
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString(dateLocale, { hour12: false });
}

export function ago(v) {
  if (!v) return "—";
  const s = Math.round((Date.now() - Date.parse(v)) / 1000);
  if (s < 60) return t("{0} 秒前", Math.max(s, 0));
  if (s < 3600) return t("{0} 分钟前", Math.round(s / 60));
  if (s < 86400) return t("{0} 小时前", Math.round(s / 3600));
  return t("{0} 天前", Math.round(s / 86400));
}

export function duration(sec) {
  if (sec === null || sec === undefined) return "—";
  const n = Math.floor(sec);
  const d = Math.floor(n / 86400);
  const hms = [Math.floor(n / 3600) % 24, Math.floor(n / 60) % 60, n % 60].map((x) => String(x).padStart(2, "0")).join(":");
  return d ? t("{0}天 {1}", d, hms) : hms;
}

export function mib(v) {
  if (v === null || v === undefined) return "—";
  return v >= 1024 * 1024 ? `${(v / 1024 / 1024).toFixed(1)} TiB` : v >= 1024 ? `${(v / 1024).toFixed(1)} GiB` : `${v} MiB`;
}

export function percent(used, total) {
  return total > 0 ? Math.round((used / total) * 100) : 0;
}

// antColor maps a status tone (success/warning/danger/primary/info) to an antd Tag/Badge colour.
export const antColor = (tone) => ({ success: "success", warning: "warning", danger: "error", primary: "processing", info: "default" })[tone] || "default";
