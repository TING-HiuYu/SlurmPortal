import { lazy } from "react";
import {
  ApiOutlined, AppstoreOutlined, BarChartOutlined, BellOutlined, CodeOutlined, DashboardOutlined, DesktopOutlined, FileSearchOutlined,
  FolderOpenOutlined, KeyOutlined, LinkOutlined, LockOutlined, MailOutlined, MedicineBoxOutlined, SafetyCertificateOutlined, SendOutlined, TeamOutlined, UserOutlined,
} from "@ant-design/icons";
import { getState, allowed } from "./session.js";
import { t } from "./i18n.js";

const view = {
  dashboard: lazy(() => import("./views/Dashboard.jsx")),
  resources: lazy(() => import("./views/status/Queue.jsx")),
  clusters: lazy(() => import("./views/status/Nodes.jsx")),
  usage: lazy(() => import("./views/status/Usage.jsx")),
  terminal: lazy(() => import("./views/workspace/Terminal.jsx")),
  files: lazy(() => import("./views/workspace/Files.jsx")),
  profile: lazy(() => import("./views/Profile.jsx")),
};

// Two areas like the v1 console: the management console and the personal workspace. Items carry
// meta.perm = [object, action] and/or meta.plugin, which gate both navigation and the menu.
export const areas = [
  {
    key: "admin",
    title: t("管理控制台"),
    groups: [
      {
        title: t("总览"),
        items: [
          { path: "/dashboard", title: t("控制台总览"), icon: DashboardOutlined, meta: { admin: true }, element: view.dashboard },
          { path: "/admin/usage", title: t("近期资源汇总"), icon: BarChartOutlined, meta: { plugin: "resource-monitor", perm: ["resource-monitor", "read"], scope: "all" }, element: view.usage },
          { path: "/admin/resources", title: t("当前资源概览"), icon: DesktopOutlined, meta: { plugin: "resource-status", admin: true }, element: view.resources },
          { path: "/admin/clusters", title: t("集群状态"), icon: AppstoreOutlined, meta: { plugin: "cluster-status", admin: true }, element: view.clusters },
        ],
      },
      {
        title: t("身份与组织"),
        items: [
          { path: "/admin/users", title: t("用户管理"), icon: UserOutlined, meta: { perm: ["user", "read"], desc: t("管理用户身份、所属组织与访问资格") }, element: lazy(() => import("./views/admin/Users.jsx")) },
          { path: "/admin/groups", title: t("用户组"), icon: TeamOutlined, meta: { perm: ["group", "read"], desc: t("维护组织结构与 POSIX 用户组") }, element: lazy(() => import("./views/admin/Groups.jsx")) },
        ],
      },
      {
        title: t("访问与安全"),
        items: [
          { path: "/admin/roles", title: t("角色与授权"), icon: LockOutlined, meta: { perm: ["role", "read"], desc: t("定义角色与可执行的操作") }, element: lazy(() => import("./views/admin/Roles.jsx")) },
          { path: "/admin/oauth", title: t("OAuth 连接器"), icon: ApiOutlined, meta: { plugin: "oauth", perm: ["oauth", "read"], desc: t("管理外部身份连接与登录方式") }, element: lazy(() => import("./views/admin/OAuth.jsx")) },
          { path: "/admin/pki", title: t("证书颁发机构"), icon: SafetyCertificateOutlined, meta: { perm: ["pki", "read"], desc: t("管理 SSH 与 X.509 证书颁发机构") }, element: lazy(() => import("./views/admin/Pki.jsx")) },
          { path: "/admin/web-access", title: t("集群网页访问"), icon: LinkOutlined, meta: { plugin: "web-access", perm: ["web-access", "read"], desc: t("网页终端、文件与传输网关") }, element: lazy(() => import("./views/admin/WebAccess.jsx")) },
        ],
      },
      {
        title: t("集成与通知"),
        items: [
          { path: "/admin/mail", title: t("邮件服务"), icon: MailOutlined, meta: { perm: ["mail", "read"], desc: t("邮件服务商、邮件池与发送统计") }, element: lazy(() => import("./views/admin/Mail.jsx")) },
          { path: "/admin/notification", title: t("作业通知"), icon: BellOutlined, meta: { plugin: "job-notification", perm: ["notification", "read"], desc: t("配置作业结束通知和邮件模板") }, element: lazy(() => import("./views/admin/Notification.jsx")) },
          { path: "/admin/password-mail", title: t("密码验证码"), icon: SafetyCertificateOutlined, meta: { perm: ["notification", "read"], desc: t("配置找回与修改密码的邮箱验证码和邮件模板") }, element: lazy(() => import("./views/admin/PasswordMail.jsx")) },
          { path: "/admin/transfer-notification", title: t("传输通知"), icon: SendOutlined, meta: { plugin: "web-access", perm: ["web-access", "read"], desc: t("配置文件传输完成通知和邮件模板") }, element: lazy(() => import("./views/admin/TransferNotification.jsx")) },
        ],
      },
      {
        title: t("系统"),
        items: [
          { path: "/admin/diagnostics", title: t("运行诊断"), icon: MedicineBoxOutlined, meta: { plugin: "diagnostics", perm: ["diagnostics", "read"], desc: t("数据库、集群上报、网关与邮件的健康状况") }, element: lazy(() => import("./views/admin/Diagnostics.jsx")) },
          { path: "/admin/audit", title: t("操作日志"), icon: FileSearchOutlined, meta: { perm: ["audit", "read"], desc: t("所有变更的防篡改审计记录") }, element: lazy(() => import("./views/admin/Audit.jsx")) },
        ],
      },
    ],
  },
  {
    key: "workspace",
    title: t("工作空间"),
    groups: [
      {
        title: t("工作空间"),
        items: [
          { path: "/workspace/resources", title: t("当前资源概览"), icon: DesktopOutlined, meta: { plugin: "resource-status" }, element: view.resources },
          { path: "/workspace/clusters", title: t("集群状态"), icon: AppstoreOutlined, meta: { plugin: "cluster-status" }, element: view.clusters },
          { path: "/workspace/usage", title: t("我的资源统计"), icon: BarChartOutlined, meta: { plugin: "resource-monitor", scope: "mine" }, element: view.usage },
          { path: "/workspace/terminal", title: t("网页终端"), icon: CodeOutlined, meta: { plugin: "web-access" }, element: view.terminal },
          { path: "/workspace/files", title: t("文件浏览器"), icon: FolderOpenOutlined, meta: { plugin: "web-access" }, element: view.files },
        ],
      },
      {
        title: t("个人中心"),
        items: [
          { path: "/profile", title: t("个人资料"), icon: UserOutlined, meta: { tab: "profile" }, element: view.profile },
          { path: "/profile/security", title: t("登录安全"), icon: LockOutlined, meta: { tab: "security" }, element: view.profile },
          { path: "/profile/certificate", title: t("证书与凭据"), icon: KeyOutlined, meta: { tab: "certificate" }, element: view.profile },
        ],
      },
    ],
  },
];

const ADMIN_PERMS = ["user", "group", "role", "pki", "oauth", "web-access", "mail", "notification", "diagnostics", "audit", "resource-monitor"];

// canManage: the management console is shown to anyone holding any management read permission.
export const canManage = () => ADMIN_PERMS.some((o) => getState().permissions.has(`${o}:read`));

export const itemAllowed = (item) => !(item.meta?.admin && !canManage()) && allowed(item.meta);

export function visibleGroups(areaKey) {
  const area = areas.find((a) => a.key === areaKey);
  return area.groups.map((g) => ({ ...g, items: g.items.filter(itemAllowed) })).filter((g) => g.items.length);
}

export const areaHome = (areaKey) => visibleGroups(areaKey)[0]?.items[0]?.path || "/profile";

// Every page with its area and group, for routing, breadcrumbs and page search.
export const pages = areas.flatMap((a) => a.groups.flatMap((g) => g.items.map((r) => ({ ...r, area: a.key, areaTitle: a.title, group: g.title }))));
export const pageFor = (path) => pages.find((p) => p.path === path);
