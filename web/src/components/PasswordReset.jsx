import { useState } from "react";
import { Alert, App, Button, Form, Input, Modal, Steps } from "antd";
import { LockOutlined, MailOutlined, SafetyOutlined } from "@ant-design/icons";
import { post, errorText } from "../api/client.js";
import { t } from "../i18n.js";

// Forgot-password dialog: the user names the account (username or e-mail), receives a 6-digit
// code by e-mail and sets a new password with it. The server answers the same for unknown accounts.
export default function PasswordReset({ open, onClose }) {
  const { message } = App.useApp();
  const [form] = Form.useForm();
  const [step, setStep] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [login, setLogin] = useState("");

  const close = () => {
    form.resetFields();
    setStep(0);
    setError("");
    onClose();
  };
  const send = async () => {
    const v = await form.validateFields(["login"]).catch(() => null);
    if (!v) return;
    setBusy(true);
    setError("");
    try {
      await post("/api/v2/auth/password-reset/request", { login: v.login.trim() });
      setLogin(v.login.trim());
      setStep(1);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };
  const confirm = async () => {
    const v = await form.validateFields(["code", "password", "confirm"]).catch(() => null);
    if (!v) return;
    setBusy(true);
    setError("");
    try {
      await post("/api/v2/auth/password-reset/confirm", { login, code: v.code.trim(), newPassword: v.password });
      message.success(t("密码已重置，请使用新密码登录"));
      close();
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal open={open} title={t("重置密码")} width={460} maskClosable={false} onCancel={close} destroyOnHidden
      footer={step === 0
        ? <Button type="primary" loading={busy} onClick={send}>{t("发送验证码")}</Button>
        : <>
          <Button disabled={busy} onClick={() => { setStep(0); setError(""); }}>{t("上一步")}</Button>
          <Button type="primary" loading={busy} onClick={confirm}>{t("重置密码")}</Button>
        </>}>
      <Steps size="small" current={step} style={{ margin: "8px 0 20px" }} items={[{ title: t("验证邮箱") }, { title: t("设置新密码") }]} />
      {error && <Alert type="error" showIcon title={error} style={{ marginBottom: 16 }} />}
      <Form form={form} layout="vertical" requiredMark={false}>
        {step === 0 ? (
          <Form.Item name="login" label={t("用户名或邮箱")} extra={t("验证码会发送到账号绑定的邮箱，10 分钟内有效。")}
            rules={[{ required: true, message: t("请输入用户名或邮箱") }]}>
            <Input prefix={<MailOutlined />} autoComplete="username" onPressEnter={send} />
          </Form.Item>
        ) : (
          <>
            <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t("如果账号存在且绑定了邮箱，验证码已发送。")} />
            <Form.Item name="code" label={t("验证码")} rules={[{ required: true, pattern: /^\s*\d{6}\s*$/, message: t("请输入 6 位数字验证码") }]}>
              <Input prefix={<SafetyOutlined />} inputMode="numeric" maxLength={6} autoComplete="one-time-code" />
            </Form.Item>
            <Form.Item name="password" label={t("新密码")} rules={[{ required: true, message: t("请输入新密码") }]}>
              <Input.Password prefix={<LockOutlined />} autoComplete="new-password" />
            </Form.Item>
            <Form.Item name="confirm" label={t("确认新密码")} dependencies={["password"]}
              rules={[{ required: true, message: t("请再次输入新密码") }, ({ getFieldValue }) => ({ validator: (_, v) => (!v || v === getFieldValue("password") ? Promise.resolve() : Promise.reject(new Error(t("两次输入不一致")))) })]}>
              <Input.Password prefix={<LockOutlined />} autoComplete="new-password" onPressEnter={confirm} />
            </Form.Item>
          </>
        )}
      </Form>
    </Modal>
  );
}
