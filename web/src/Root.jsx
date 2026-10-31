import { Suspense, useEffect, useState } from "react";
import { createBrowserRouter, Navigate, Outlet, RouterProvider, useLocation } from "react-router";
import { App as AntApp, Button, ConfigProvider, Result, Spin, theme as antTheme } from "antd";
import zhCN from "antd/locale/zh_CN";
import enUS from "antd/locale/en_US";
import Layout from "./components/Layout.jsx";
import Login from "./views/Login.jsx";
import { pages, areaHome, canManage, itemAllowed } from "./routes.jsx";
import { load, logout, signedIn, useSession } from "./session.js";
import { errorText, setUnauthorizedHandler } from "./api/client.js";
import { invalidate } from "./session.js";
import { useTheme } from "./theme.js";
import { locale, t } from "./i18n.js";

// Console design tokens carried over from the v1 portal.
const light = { colorPrimary: "#3f63e9", colorSuccess: "#17836b", colorWarning: "#a36b19", colorError: "#c44254", colorInfo: "#3f63e9",
  colorBgLayout: "#f5f7fb", colorBorder: "#e6ebf2", colorBorderSecondary: "#edf0f5", colorText: "#1c2940", colorTextSecondary: "#526079",
  colorTextTertiary: "#7d8ba1", colorTextPlaceholder: "#929fb4" };
const dark = { colorPrimary: "#4c6bd5", colorSuccess: "#258974", colorWarning: "#9a6c2c", colorError: "#bf5265", colorInfo: "#4c6bd5",
  colorBgLayout: "#10151e", colorBgContainer: "#19212d", colorBgElevated: "#202a39", colorBorder: "#344158", colorBorderSecondary: "#29364a",
  colorText: "#e4eaf4", colorTextSecondary: "#b4c1d5", colorTextTertiary: "#9aaac1", colorTextPlaceholder: "#7e90ad" };
const font = '-apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif';

const loading = <div className="page-loading"><Spin size="large" /></div>;

const SESSION_TIMEOUT_MS = 15000;

// loadWithin rejects when the session cannot be loaded in time, so a stuck request never leaves the
// user on an endless spinner.
function loadWithin(ms) {
  let timer;
  return Promise.race([load(), new Promise((_, reject) => (timer = setTimeout(() => reject(new Error(t("连接主服务超时"))), ms)))]).finally(() => clearTimeout(timer));
}

// RequireSession loads the signed-in user, then gates each page by its permission/plugin.
function RequireSession() {
  const s = useSession();
  const location = useLocation();
  const [error, setError] = useState(null);
  const [attempt, setAttempt] = useState(0);
  const live = signedIn();
  useEffect(() => {
    if (!live || s.loaded) return undefined;
    let current = true;
    setError(null);
    loadWithin(SESSION_TIMEOUT_MS).catch((e) => current && setError(e));
    return () => {
      current = false;
    };
  }, [live, s.loaded, attempt]);
  const toLogin = <Navigate to={`/login${location.pathname === "/" ? "" : `?next=${encodeURIComponent(location.pathname + location.search)}`}`} replace />;
  if (!live || error?.status === 401) return toLogin;
  if (error) {
    return (
      <Result status="warning" title={t("加载失败")} subTitle={t("无法读取登录信息：{0}", errorText(error))}
        extra={[
          <Button key="retry" type="primary" onClick={() => setAttempt((n) => n + 1)}>{t("重试")}</Button>,
          <Button key="login" onClick={async () => { await logout(); window.location.assign("/login"); }}>{t("重新登录")}</Button>,
        ]} />
    );
  }
  if (!s.loaded) return loading;
  const page = pages.find((p) => p.path === location.pathname);
  if (location.pathname === "/") return <Navigate to={areaHome(canManage() ? "admin" : "workspace")} replace />;
  if (page && !itemAllowed(page)) return <Navigate to="/" replace />;
  return <Outlet />;
}

const router = createBrowserRouter([
  { path: "/login", element: <Login /> },
  {
    element: <RequireSession />,
    children: [
      {
        element: <Layout />,
        children: [
          { path: "/", element: null },
          ...pages.map((p) => ({ path: p.path, element: <Suspense fallback={loading}><p.element page={p} /></Suspense> })),
        ],
      },
    ],
  },
  // v1 and earlier v2 paths.
  { path: "/terminal", element: <Navigate to="/workspace/terminal" replace /> },
  { path: "/files", element: <Navigate to="/workspace/files" replace /> },
  { path: "/status/nodes", element: <Navigate to="/workspace/clusters" replace /> },
  { path: "/status/queue", element: <Navigate to="/workspace/resources" replace /> },
  { path: "/status/usage", element: <Navigate to="/workspace/usage" replace /> },
  { path: "*", element: <Navigate to="/" replace /> },
]);

setUnauthorizedHandler(() => {
  invalidate();
  const { pathname, search } = window.location;
  if (pathname !== "/login") router.navigate(`/login?next=${encodeURIComponent(pathname + search)}`);
});

export default function Root() {
  const { dark: isDark } = useTheme();
  return (
    <ConfigProvider
      locale={locale === "en" ? enUS : zhCN}
      theme={{
        algorithm: isDark ? antTheme.darkAlgorithm : antTheme.defaultAlgorithm,
        token: { ...(isDark ? dark : light), fontFamily: font, borderRadius: 6, borderRadiusLG: 12 },
        components: {
          Table: { headerBg: isDark ? "#202a39" : "#f3f5fa", headerColor: isDark ? "#9aaac1" : "#7d8ba1", rowHoverBg: isDark ? "#29395e" : "#eef2ff", cellPaddingBlock: 13 },
          Menu: { itemHeight: 44, itemBorderRadius: 8, itemSelectedBg: isDark ? "#29395e" : "#eef2ff", itemSelectedColor: isDark ? "#96afff" : "#3f63e9", groupTitleFontSize: 11 },
          Card: { headerFontSize: 15 },
        },
      }}
    >
      <AntApp style={{ height: "100%" }}>
        <RouterProvider router={router} />
      </AntApp>
    </ConfigProvider>
  );
}
