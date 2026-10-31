import { useEffect, useMemo, useRef, useState } from "react";
import { Link, Outlet, useLocation, useNavigate } from "react-router";
import { Avatar, Breadcrumb, Button, Drawer, Dropdown, Flex, Input, Layout as AntLayout, Menu, Modal, Segmented, Tooltip, Typography, Grid } from "antd";
import {
  DashboardOutlined, DesktopOutlined, InfoCircleOutlined, KeyOutlined, LogoutOutlined, MenuFoldOutlined, MenuUnfoldOutlined, MoonOutlined,
  SearchOutlined, SunOutlined, UserOutlined,
} from "@ant-design/icons";
import BrandIdentity from "./BrandIdentity.jsx";
import LocaleSwitch from "./LocaleSwitch.jsx";
import { areas, areaHome, canManage, pageFor, visibleGroups } from "../routes.jsx";
import { logout, useSession } from "../session.js";
import { cycleTheme, themeLabel, useTheme } from "../theme.js";
import { t } from "../i18n.js";

const { Sider, Header, Content } = AntLayout;
const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

export default function Layout() {
  const session = useSession();
  const { mode } = useTheme();
  const location = useLocation();
  const navigate = useNavigate();
  const screens = Grid.useBreakpoint();
  const mobile = screens.md === false;
  const [collapsed, setCollapsed] = useState(false);
  const [drawer, setDrawer] = useState(false);
  const [search, setSearch] = useState(false);
  const [query, setQuery] = useState("");
  const searchInput = useRef(null);

  const manage = canManage();
  const page = pageFor(location.pathname);
  const area = manage ? page?.area || "workspace" : "workspace";
  const groups = visibleGroups(area);
  const roleText = session.roles.includes("admin") ? t("管理员") : manage ? t("管理成员") : t("成员");
  const name = session.user?.nickname || session.user?.username || "";

  const menuItems = groups.map((g) => ({
    type: "group",
    key: g.title,
    label: g.title,
    children: g.items.map((r) => ({ key: r.path, icon: <r.icon />, label: r.title })),
  }));

  const allPages = useMemo(
    () => areas.flatMap((a) => (a.key === "admin" && !manage ? [] : visibleGroups(a.key).flatMap((g) => g.items.map((r) => ({ ...r, areaTitle: a.title, group: g.title }))))),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [manage, session.permissions, session.plugins],
  );
  const results = allPages.filter((p) => !query.trim() || p.title.includes(query.trim()) || p.group.includes(query.trim()) || p.path.includes(query.trim().toLowerCase())).slice(0, 12);

  useEffect(() => {
    const onKey = (e) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setSearch(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const go = (path) => {
    setDrawer(false);
    if (location.pathname !== path) navigate(path);
  };
  const pick = (p) => {
    if (!p) return;
    setSearch(false);
    setQuery("");
    go(p.path);
  };

  const sider = (
    <Flex vertical style={{ height: "100%" }}>
      <Link to="/" style={{ display: "flex", alignItems: "center", height: 82, padding: collapsed && !mobile ? "0 19px" : "0 24px", flexShrink: 0 }}>
        <BrandIdentity compact={collapsed && !mobile} />
      </Link>
      {manage && (!collapsed || mobile) && (
        <Segmented block value={area} style={{ margin: "0 17px 14px" }} onChange={(v) => go(areaHome(v))}
          options={[{ label: t("管理控制台"), value: "admin" }, { label: t("工作空间"), value: "workspace" }]} />
      )}
      <div style={{ flex: 1, overflowY: "auto", overflowX: "hidden" }}>
        <Menu mode="inline" inlineCollapsed={collapsed && !mobile} selectedKeys={[location.pathname]} items={menuItems} onClick={({ key }) => go(key)}
          style={{ borderInlineEnd: 0, padding: "0 6px" }} />
      </div>
      {(!collapsed || mobile) && (
        <Typography.Text type="secondary" style={{ margin: "0 22px", padding: "16px 0 18px", borderTop: "1px solid var(--console-border)", fontSize: 12 }}>
          <InfoCircleOutlined /> {t("版本 v2")}
        </Typography.Text>
      )}
    </Flex>
  );

  const userMenu = {
    items: [
      { key: "/profile", icon: <UserOutlined />, label: t("我的账户") },
      { key: "/profile/certificate", icon: <KeyOutlined />, label: t("证书与凭据") },
      ...(manage ? [{ type: "divider" }, { key: "/dashboard", icon: <DashboardOutlined />, label: t("管理控制台") }] : []),
      { type: "divider" },
      { key: "logout", icon: <LogoutOutlined />, label: t("退出登录"), danger: true },
    ],
    onClick: async ({ key }) => {
      if (key !== "logout") return go(key);
      await logout();
      navigate("/login");
    },
  };

  return (
    // Only the content area scrolls; the header and sider stay in place.
    <AntLayout style={{ height: "100%", overflow: "hidden" }}>
      {mobile ? (
        <Drawer open={drawer} placement="left" size={260} closable={false} onClose={() => setDrawer(false)} styles={{ body: { padding: 0 } }}>{sider}</Drawer>
      ) : (
        <Sider width={240} collapsedWidth={72} collapsed={collapsed} theme="light" style={{ borderRight: "1px solid var(--console-border)", overflowY: "auto" }}>{sider}</Sider>
      )}
      <AntLayout style={{ minWidth: 0, overflow: "hidden" }}>
        <Header style={{ height: 64, lineHeight: "normal", display: "flex", alignItems: "center", gap: 14, padding: mobile ? "0 12px 0 8px" : "0 28px 0 18px",
          background: "var(--console-surface)", borderBottom: "1px solid var(--console-border)" }}>
          <Button type="text" aria-label={t("切换导航")} icon={collapsed || mobile ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
            onClick={() => (mobile ? setDrawer(true) : setCollapsed(!collapsed))} />
          {!mobile && page && (
            <Breadcrumb style={{ flex: 1, minWidth: 0 }}
              items={[{ title: page.areaTitle }, ...(page.group !== page.areaTitle ? [{ title: page.group }] : []), { title: page.title }]} />
          )}
          <Flex align="center" gap={mobile ? 6 : 16} style={{ marginLeft: "auto" }}>
            <Button type="text" icon={<SearchOutlined />} onClick={() => setSearch(true)} style={{ color: "var(--console-muted)" }}>
              {!mobile && <>{t("查找页面")} <Typography.Text keyboard style={{ fontSize: 10 }}>{isMac ? "⌘" : "Ctrl"} K</Typography.Text></>}
            </Button>
            <LocaleSwitch compact={mobile} />
            <Tooltip title={themeLabel(mode)}>
              <Button type="text" aria-label={themeLabel(mode)} onClick={cycleTheme}
                icon={mode === "dark" ? <MoonOutlined /> : mode === "light" ? <SunOutlined /> : <DesktopOutlined />} />
            </Tooltip>
            <Dropdown menu={userMenu} trigger={["click"]} placement="bottomRight">
              <Button type="text" style={{ height: 44, padding: "0 6px" }}>
                <Flex align="center" gap={10}>
                  <Avatar style={{ background: "var(--console-accent-soft)", color: "var(--console-accent)", fontWeight: 650 }}>{Array.from(name || "U")[0].toUpperCase()}</Avatar>
                  {!mobile && (
                    <Flex vertical align="flex-start" gap={2}>
                      <strong style={{ fontSize: 13, lineHeight: 1.2 }}>{name}</strong>
                      <Typography.Text type="secondary" style={{ fontSize: 11, lineHeight: 1.2 }}>{roleText}</Typography.Text>
                    </Flex>
                  )}
                </Flex>
              </Button>
            </Dropdown>
          </Flex>
        </Header>
        <Content style={{ overflow: "auto", minHeight: 0 }}>
          <div className="console-page"><Outlet /></div>
        </Content>
      </AntLayout>

      <Modal open={search} footer={null} closable={false} width={560} onCancel={() => setSearch(false)} afterOpenChange={(o) => o && searchInput.current?.focus()}>
        <Input ref={searchInput} size="large" allowClear prefix={<SearchOutlined />} placeholder={t("输入页面名称…")} value={query}
          onChange={(e) => setQuery(e.target.value)} onPressEnter={() => pick(results[0])} />
        <Menu style={{ marginTop: 12, borderInlineEnd: 0, maxHeight: 380, overflowY: "auto" }} selectable={false} onClick={({ key }) => pick(results.find((r) => r.path === key))}
          items={results.length ? results.map((r) => ({ key: r.path, icon: <r.icon />, label: <Flex justify="space-between"><span>{r.title}</span><Typography.Text type="secondary" style={{ fontSize: 12 }}>{r.areaTitle} / {r.group}</Typography.Text></Flex> })) : [{ key: "none", disabled: true, label: t("没有匹配的页面") }]} />
      </Modal>
    </AntLayout>
  );
}
