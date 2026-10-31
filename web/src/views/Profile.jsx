import { useEffect, useState } from "react";
import { useNavigate } from "react-router";
import { Alert, App, Avatar, Button, Card, Descriptions, Divider, Empty, Flex, Form, Input, Segmented, Space, Tabs, Tag, Typography } from "antd";
import { DownloadOutlined, ReloadOutlined, UserOutlined } from "@ant-design/icons";
import PageHead from "../components/PageHead.jsx";
import { get, post, put, errorText, ApiError } from "../api/client.js";
import { load, useSession } from "../session.js";
import { download, useConfirm } from "../hooks.js";
import { time } from "../format.js";
import { t } from "../i18n.js";

const paths = { profile: "/profile", security: "/profile/security", certificate: "/profile/certificate" };

export default function Profile({ page }) {
  const { message } = App.useApp();
  const confirm = useConfirm();
  const navigate = useNavigate();
  const { user = {}, roles } = useSession();
  const tab = page.meta.tab;
  const [shell, setShell] = useState(user.loginShell || "");
  const [cert, setCert] = useState(null);
  const [issued, setIssued] = useState(null);
  const [pwForm] = Form.useForm();
  const method = Form.useWatch("method", pwForm) || "current";
  const [sending, setSending] = useState(false);
  const [cooldown, setCooldown] = useState(0);
  const [codesEnabled, setCodesEnabled] = useState(false);
  useEffect(() => {
    get("/api/v2/auth/password-reset").then((d) => setCodesEnabled(!!d?.enabled)).catch(() => setCodesEnabled(false));
  }, []);
  useEffect(() => {
    if (cooldown <= 0) return undefined;
    const timer = setTimeout(() => setCooldown((c) => c - 1), 1000);
    return () => clearTimeout(timer);
  }, [cooldown]);
  // Mails a 6-digit code to the user's own address; the server allows one per minute.
  const sendCode = async () => {
    setSending(true);
    try {
      await post("/api/v2/me/password/code");
      message.success(t("验证码已发送到 {0}", user.mail));
      setCooldown(60);
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setSending(false);
    }
  };
  const certDays = cert?.expiresAt ? Math.floor((Date.parse(cert.expiresAt) - Date.now()) / 86400000) : null;

  const loadCert = async () => {
    try {
      setCert(await get("/api/v2/me/certificate"));
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 404)) message.error(errorText(e));
    }
  };
  useEffect(() => {
    loadCert();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const act = async (fn, ok) => {
    try {
      await fn();
      if (ok) message.success(ok);
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const issue = async () => {
    if (cert && !(await confirm(t("重新签发证书"), t("重新签发会让旧证书失效，确定继续？")))) return;
    act(async () => {
      const out = await post("/api/v2/me/certificate");
      setIssued(out);
      setCert(out);
    });
  };

  return (
    <>
      <PageHead title={t("我的账户")}>
        <Button icon={<ReloadOutlined />} onClick={async () => { await Promise.allSettled([load(), loadCert()]); }}>{t("刷新")}</Button>
      </PageHead>
      <Card>
        <Tabs activeKey={tab} onChange={(k) => navigate(paths[k])}
          items={[{ key: "profile", label: t("个人信息") }, { key: "security", label: t("登录安全") }, { key: "certificate", label: t("访问证书") }]} />

        {tab === "profile" && (
          <>
            <Flex align="center" gap={18} style={{ padding: "8px 0 22px" }}>
              <Avatar shape="square" size={62} icon={<UserOutlined />} style={{ background: "var(--console-accent-soft)", color: "#5a73a8", borderRadius: 14 }} />
              <Flex vertical gap={6} style={{ flex: 1 }}>
                <Typography.Title level={4} style={{ margin: 0 }}>{user.nickname || user.username}</Typography.Title>
                <Typography.Text type="secondary">{user.username}</Typography.Text>
              </Flex>
              <Tag color={user.status === 1 ? "success" : "error"}>{user.status === 1 ? t("启用") : t("停用")}</Tag>
            </Flex>
            <Divider style={{ margin: "0 0 22px" }} />
            <Descriptions column={{ xs: 1, md: 2, xl: 3 }} layout="vertical" colon={false}
              items={[
                { label: t("用户名"), children: user.username },
                { label: t("邮箱"), children: user.mail || "—" },
                { label: t("角色"), children: roles.join("、") || "—" },
                { label: t("手机"), children: user.mobile || "—" },
                { label: t("身份来源"), children: user.source || "—" },
              ]} />
            <Divider />
            <h3 className="section-title">{t("目录身份")}</h3>
            <p className="section-desc">{t("数字身份与主目录由管理员统一管理。")}</p>
            <Descriptions column={{ xs: 1, md: 3 }} layout="vertical" colon={false}
              items={[
                { label: "UID", children: <span className="mono">{user.uidNumber || "—"}</span> },
                { label: "GID", children: <span className="mono">{user.gidNumber || "—"}</span> },
                { label: "HOME", children: <span className="mono">{user.homeDirectory || "—"}</span> },
              ]} />
            <Divider />
            <h3 className="section-title">{t("终端设置")}</h3>
            <p className="section-desc">{t("修改默认 Shell，不会更改已有的数字身份。")}</p>
            <Space.Compact style={{ maxWidth: 560, width: "100%" }}>
              <Input value={shell} onChange={(e) => setShell(e.target.value)} placeholder="/bin/bash" className="mono" />
              <Button type="primary" disabled={!shell || shell === user.loginShell} onClick={() => act(async () => { await put("/api/v2/me/shell", { loginShell: shell }); await load(); }, t("已保存"))}>{t("保存")}</Button>
            </Space.Compact>
          </>
        )}

        {tab === "security" && (
          <>
            <h3 className="section-title">{t("修改密码")}</h3>
            <Form form={pwForm} layout="vertical" style={{ maxWidth: 420 }} initialValues={{ method: "current" }}
              onFinish={(v) => act(async () => {
                await put("/api/v2/me/password", v.method === "email" ? { code: v.code.trim(), newPassword: v.next } : { currentPassword: v.current, newPassword: v.next });
                pwForm.resetFields();
                setCooldown(0);
              }, t("密码已修改"))}>
              <Form.Item label={t("验证方式")} name="method">
                <Segmented options={[{ value: "current", label: t("当前密码") }, { value: "email", label: t("邮箱验证码"), disabled: !user.mail || !codesEnabled }]} />
              </Form.Item>
              {method === "email" ? (
                <Form.Item label={t("验证码")} required extra={t("验证码将发送到 {0}，10 分钟内有效。", user.mail)}>
                  <Space.Compact style={{ width: "100%" }}>
                    <Form.Item noStyle name="code" rules={[{ required: true, pattern: /^\s*\d{6}\s*$/, message: t("请输入 6 位数字验证码") }]}>
                      <Input inputMode="numeric" maxLength={6} autoComplete="one-time-code" placeholder={t("6 位数字")} />
                    </Form.Item>
                    <Button loading={sending} disabled={cooldown > 0} onClick={sendCode}>{cooldown > 0 ? t("{0} 秒后重发", cooldown) : t("发送验证码")}</Button>
                  </Space.Compact>
                </Form.Item>
              ) : (
                <Form.Item label={t("当前密码")} name="current" rules={[{ required: true }]}><Input.Password autoComplete="current-password" /></Form.Item>
              )}
              <Form.Item label={t("新密码")} name="next" rules={[{ required: true }]}><Input.Password autoComplete="new-password" /></Form.Item>
              <Form.Item label={t("确认新密码")} name="confirm" dependencies={["next"]}
                rules={[{ required: true }, ({ getFieldValue }) => ({ validator: (_, v) => (!v || v === getFieldValue("next") ? Promise.resolve() : Promise.reject(new Error(t("两次输入不一致")))) })]}>
                <Input.Password autoComplete="new-password" />
              </Form.Item>
              <Button type="primary" htmlType="submit">{t("修改密码")}</Button>
            </Form>
          </>
        )}

        {tab === "certificate" && (
          <>
            <h3 className="section-title">{t("SSH 访问证书")}</h3>
            {certDays !== null && certDays < 14 && (
              <Alert showIcon type={certDays < 0 ? "error" : "warning"} style={{ marginBottom: 18 }} title={certDays < 0 ? t("证书已过期，请重新签发") : t("证书将在 {0} 天后到期", certDays)} />
            )}
            {cert ? (
              <>
                <Descriptions column={{ xs: 1, md: 3 }} layout="vertical" colon={false}
                  items={[{ label: t("类型"), children: cert.kind }, { label: t("序列号"), children: <span className="mono">{cert.serialHex}</span> }, { label: t("到期时间"), children: time(cert.expiresAt) }]} />
                <pre className="secret mono">{cert.certificatePem}</pre>
              </>
            ) : <Empty description={t("尚未签发证书")} />}
            {issued && <Alert type="warning" showIcon style={{ marginTop: 18 }} title={t("私钥只显示这一次，请立即下载并妥善保存")} />}
            <Space style={{ marginTop: 18 }}>
              {cert && <Button icon={<DownloadOutlined />} onClick={() => download(cert.certificatePem, `${user.username}-certificate.pem`)}>{t("下载证书")}</Button>}
              {issued
                ? <Button type="primary" icon={<DownloadOutlined />} onClick={() => download(issued.privateKeyPem, `${user.username}-private-key.pem`)}>{t("下载私钥")}</Button>
                : <Button type="primary" onClick={issue}>{cert ? t("重新签发") : t("签发证书")}</Button>}
            </Space>
          </>
        )}
      </Card>
    </>
  );
}
