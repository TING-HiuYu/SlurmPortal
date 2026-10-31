import { useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router";
import { Alert, Button, Divider, Flex, Form, Grid, Input, Modal, Tooltip, Typography } from "antd";
import { DesktopOutlined, LockOutlined, MoonOutlined, SunOutlined, UserOutlined } from "@ant-design/icons";
import BrandIdentity from "../components/BrandIdentity.jsx";
import AuthClusterScene from "../components/AuthClusterScene.jsx";
import { get, errorText } from "../api/client.js";
import { passwordLogin, accessLogin, oauthLogin } from "../session.js";
import { cycleTheme, themeLabel, useTheme } from "../theme.js";
import LocaleSwitch from "../components/LocaleSwitch.jsx";
import PasswordReset from "../components/PasswordReset.jsx";
import "./Login.css";
import { t } from "../i18n.js";

const GitHubMark = () => (
  <svg className="sso-icon" viewBox="0 0 16 16" aria-hidden="true"><path fill="currentColor" d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z" /></svg>
);
const CloudflareMark = () => (
  <svg className="sso-icon" viewBox="0 0 24 24" aria-hidden="true">
    <path fill="#F38020" d="M16.5 15.6l.3-1c.2-.7.1-1.3-.2-1.7-.3-.4-.8-.6-1.3-.6l-8.6-.1c-.1 0-.1 0-.2-.1v-.2c0-.1.1-.1.2-.2l8.7-.1c1-.1 2.1-.9 2.5-1.9l.5-1.3v-.2C17.8 5.7 15.6 4 13.1 4 10.7 4 8.7 5.5 8 7.6c-.5-.3-1.1-.5-1.7-.5-1.1.1-2 1-2.1 2.1 0 .3 0 .5.1.8C2.5 10.1 1 11.6 1 13.5c0 .2 0 .3.1.5 0 .1.1.1.2.1h15c.1 0 .2-.1.2-.2z" />
    <path fill="#FAAE40" d="M19.1 9.7h-.2c-.1 0-.1.1-.2.2l-.3 1.1c-.2.7-.1 1.3.2 1.7.3.4.8.6 1.3.6l1.8.1c.1 0 .1 0 .2.1v.2c0 .1-.1.1-.2.2l-1.9.1c-1 .1-2.1.9-2.5 1.9l-.1.4c0 .1 0 .2.2.2h6.4c.1 0 .2-.1.2-.2.1-.4.2-.8.2-1.2-.1-2.4-2.1-4.4-4.6-4.4z" />
  </svg>
);

export default function Login() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const { mode } = useTheme();
  const narrow = Grid.useBreakpoint().lg === false;
  const [busy, setBusy] = useState(null);
  const [error, setError] = useState("");
  const [initial, setInitial] = useState("");
  const [access, setAccess] = useState({ available: false, email: "" });
  const [connectors, setConnectors] = useState([]);
  const [resetOpen, setResetOpen] = useState(false);
  const [resetEnabled, setResetEnabled] = useState(false);
  useEffect(() => {
    get("/api/v2/auth/password-reset").then((d) => setResetEnabled(!!d?.enabled)).catch(() => setResetEnabled(false));
  }, []);

  useEffect(() => {
    Promise.allSettled([get("/api/v2/auth/access"), get("/api/v2/oauth/connectors/public")]).then(([a, list]) => {
      if (a.status === "fulfilled" && a.value?.available) setAccess({ available: true, email: a.value.email || "" });
      if (list.status === "fulfilled") setConnectors(list.value || []);
    });
  }, []);

  const proceed = () => {
    setInitial("");
    const next = params.get("next");
    navigate(next && next.startsWith("/") && !next.startsWith("//") ? next : "/", { replace: true });
  };
  const run = async (kind, fn) => {
    setBusy(kind);
    setError("");
    try {
      const out = await fn();
      if (out?.initialPassword) return setInitial(out.initialPassword);
      proceed();
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="login-shell">
      <aside className="auth-brand">
        <div className="auth-logo"><BrandIdentity light={!narrow} /></div>
        <div className="brand-copy">
          <p className="brand-eyebrow">SLURM PORTAL / HPC SERVICES</p>
          <h1>{t("集群帐户中心")}</h1>
          <AuthClusterScene className="auth-scene" />
        </div>
      </aside>
      <main className="auth-main">
        <div className="auth-tools">
          <LocaleSwitch />
          <Tooltip title={themeLabel(mode)}>
            <Button type="text" aria-label={themeLabel(mode)} onClick={cycleTheme} icon={mode === "dark" ? <MoonOutlined /> : mode === "light" ? <SunOutlined /> : <DesktopOutlined />} />
          </Tooltip>
        </div>
        <section className="auth-card">
          <h2>{t("欢迎回来")}</h2>
          <Typography.Paragraph type="secondary" style={{ marginBottom: 28 }}>{t("登录集群帐户中心")}</Typography.Paragraph>
          {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 20 }} />}

          {(access.available || connectors.length > 0) && (
            <Flex vertical gap={12}>
              {access.available && (
                <Button block className="sso-btn" icon={<CloudflareMark />} loading={busy === "access"} disabled={!!busy && busy !== "access"} onClick={() => run("access", accessLogin)}>
                  {access.email ? t("以 {0} 登录", access.email) : t("使用 Cloudflare Access 登录")}
                </Button>
              )}
              {connectors.map((c) => (
                <Button key={c.id} block className="sso-btn" loading={busy === c.id} disabled={!!busy && busy !== c.id} onClick={() => run(c.id, () => oauthLogin(c.id))}
                  icon={c.provider === "github" ? <GitHubMark /> : <span className="chip blue" style={{ width: 22, height: 22, fontSize: 12 }}>{(c.provider || c.name).slice(0, 1).toUpperCase()}</span>}>
                  {t("使用 {0} 继续", c.name)}
                </Button>
              ))}
              <Divider plain style={{ margin: "10px 0 14px" }}><Typography.Text type="secondary" style={{ fontSize: 12, letterSpacing: "0.12em" }}>{t("或")}</Typography.Text></Divider>
            </Flex>
          )}

          <Form layout="vertical" requiredMark={false} disabled={!!busy && busy !== "password"} onFinish={(v) => run("password", () => passwordLogin(v.username.trim(), v.password))}>
            <Form.Item label={t("用户名")} name="username" rules={[{ required: true, message: t("请输入用户名") }]}>
              <Input size="large" prefix={<UserOutlined />} placeholder={t("请输入用户名")} autoComplete="username" />
            </Form.Item>
            <Form.Item label={t("密码")} name="password" rules={[{ required: true, message: t("请输入密码") }]}>
              <Input.Password size="large" prefix={<LockOutlined />} placeholder={t("密码")} autoComplete="current-password" />
            </Form.Item>
            <Button type="primary" htmlType="submit" size="large" block loading={busy === "password"} style={{ marginTop: 6 }}>{t("使用密码登录")}</Button>
            {resetEnabled && (
              <Flex justify="flex-end" style={{ marginTop: 10 }}>
                <Typography.Link onClick={() => setResetOpen(true)}>{t("忘记密码？")}</Typography.Link>
              </Flex>
            )}
          </Form>
        </section>
      </main>

      <PasswordReset open={resetOpen} onClose={() => setResetOpen(false)} />
      <Modal open={!!initial} title={t("请记录初始密码")} width={440} closable={false} maskClosable={false} footer={<Button type="primary" onClick={proceed}>{t("我已记录，继续")}</Button>}>
        <Typography.Paragraph type="secondary">{t("已为你创建集群账号。初始密码只显示这一次，可在个人中心修改。")}</Typography.Paragraph>
        <Typography.Paragraph copyable={{ text: initial }} className="secret mono" style={{ fontSize: 18, textAlign: "center", letterSpacing: 2 }}>{initial}</Typography.Paragraph>
      </Modal>
    </div>
  );
}
