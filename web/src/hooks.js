// Data hooks shared by views.
import { useCallback, useEffect, useRef, useState } from "react";
import { App } from "antd";
import { get, errorText } from "./api/client.js";
import { t } from "./i18n.js";

// usePoll loads now and every intervalMs while mounted, the tab is visible and not paused.
export function usePoll(load, intervalMs) {
  const { message } = App.useApp();
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [paused, setPaused] = useState(false);
  const loadRef = useRef(load);
  loadRef.current = load;
  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setData(await loadRef.current());
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setLoading(false);
    }
  }, [message]);
  useEffect(() => {
    refresh();
  }, [refresh]);
  useEffect(() => {
    if (!intervalMs || paused) return undefined;
    const timer = setInterval(() => document.visibilityState === "visible" && refresh(), intervalMs);
    return () => clearInterval(timer);
  }, [intervalMs, paused, refresh]);
  return { data, setData, loading, refresh, paused, setPaused };
}

// useLoad runs load once (and on reload()); errors become a message.
export const useLoad = (load) => usePoll(load, 0);

export const list = (v) => (Array.isArray(v) ? v : v?.items || []);

// useWebAccessClusters lists the clusters and feature switches the caller may use for web access.
export function useWebAccessClusters() {
  const [state, setState] = useState({ clusters: [], capabilities: {}, loaded: false, error: "" });
  useEffect(() => {
    let live = true;
    get("/api/v2/web-access/clusters")
      .then((d) => live && setState({ clusters: d.clusters || [], capabilities: d.capabilities || {}, loaded: true, error: "" }))
      .catch((e) => live && setState((s) => ({ ...s, loaded: true, error: errorText(e) })));
    return () => {
      live = false;
    };
  }, []);
  return state;
}

export function gatewayError(e) {
  return {
    ed25519_unavailable: t("浏览器不支持 Ed25519，请使用最新版 Chrome / Edge / Firefox / Safari"),
    session_changed: t("登录状态已变化，请重新连接"),
    invalid_gateway_ticket: t("网关票据无效"),
    session_unavailable: t("网关会话已失效，请重新连接"),
  }[e?.code || e?.message] || errorText(e);
}

// useConfirm wraps modal.confirm in a promise: resolves true on OK.
export function useConfirm() {
  const { modal } = App.useApp();
  return useCallback(
    (title, content, danger = true) =>
      new Promise((resolve) => modal.confirm({ title, content, okButtonProps: { danger }, okText: t("确定"), cancelText: t("取消"), onOk: () => resolve(true), onCancel: () => resolve(false) })),
    [modal],
  );
}

export function download(text, filename, type = "application/x-pem-file") {
  const a = document.createElement("a");
  a.href = URL.createObjectURL(new Blob([text], { type }));
  a.download = filename;
  a.click();
  URL.revokeObjectURL(a.href);
}
