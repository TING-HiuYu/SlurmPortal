import { useEffect, useMemo, useState } from "react";
import { Alert, App, Button, Card, Collapse, Flex, Form, Input, Select, Spin, Switch, Table, Tabs, Tag, Tooltip, Typography } from "antd";
import { CheckOutlined, CopyOutlined, EyeOutlined, ReloadOutlined, SendOutlined } from "@ant-design/icons";
import PageHead from "./PageHead.jsx";
import { get, post, put, errorText } from "../api/client.js";
import { can } from "../session.js";
import { time } from "../format.js";
import { t } from "../i18n.js";

async function copyText(message, text, what) {
  try {
    await navigator.clipboard.writeText(text);
    message.success(t("已复制{0}", what));
  } catch {
    message.error(t("复制失败，请手动选择文本"));
  }
}

// TemplateWorkbench edits one mail notification: switch, provider, time zone and a {{ placeholder }}
// template, with preview, test mail, the placeholder list and a copy of real (or example) data.
// configUrl (default base) serves GET/PUT {enabled, provider, timeZone, template}; base serves POST
// base/preview, base/test and GET base/sample.
export default function TemplateWorkbench({ title, desc, base, configUrl = base, perm, enableLabel }) {
  const { message, modal } = App.useApp();
  const [form] = Form.useForm();
  const [loaded, setLoaded] = useState(false);
  const [providers, setProviders] = useState([]);
  const [rendered, setRendered] = useState(null);
  const [sample, setSample] = useState(null);
  // busy names the request in flight (save, preview, test-me, test-other) so its button spins.
  const [busy, setBusy] = useState("");
  const editable = can(perm, "update");
  const defaultName = useMemo(() => providers.find((p) => p.default)?.name || "", [providers]);

  const fill = (c) => form.setFieldsValue({ ...c, provider: c.provider || "", constants: JSON.stringify(c.template?.constants || {}, null, 2) });
  const reload = () =>
    Promise.all([get(configUrl), can("mail") ? get("/api/v2/mail/providers").catch(() => []) : [], get(`${base}/sample`).catch(() => null)])
      .then(([c, p, s]) => {
        fill(c);
        setProviders(p || []);
        setSample(s);
        setLoaded(true);
      })
      .catch((e) => message.error(errorText(e)));
  useEffect(() => {
    reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [base]);

  const template = () => {
    const v = form.getFieldsValue(true);
    let constants;
    try {
      constants = JSON.parse(v.constants || "{}");
    } catch {
      throw new Error(t("常量不是有效的 JSON"));
    }
    return { ...v.template, constants };
  };
  const run = async (fn, key = "") => {
    setBusy(key);
    try {
      await fn();
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setBusy("");
    }
  };
  const save = () => run(async () => {
    const v = form.getFieldsValue(true);
    fill(await put(configUrl, { enabled: v.enabled, timeZone: v.timeZone, provider: v.provider || "", template: template() }));
    message.success(t("已保存"));
  }, "save");
  const preview = () => run(async () => setRendered(await post(`${base}/preview`, { template: template(), timeZone: form.getFieldValue("timeZone") })), "preview");
  const insert = (name) => {
    const html = form.getFieldValue(["template", "html"]) || "";
    form.setFieldValue(["template", "html"], `${html}{{ ${name} }}`);
  };

  const enabled = Form.useWatch("enabled", form);
  // Sending waits for the mail provider to accept the message, which can take several seconds.
  const sendTest = (to, key) => run(async () => {
    await post(`${base}/test`, to ? { to } : {});
    message.success(t("邮件服务商已接受测试邮件"));
  }, key);
  const testOther = () => {
    let to = "";
    modal.confirm({
      title: t("发送测试邮件到其他地址"), okText: t("发送"), cancelText: t("取消"),
      content: <Input placeholder={t("收件地址")} onChange={(e) => (to = e.target.value)} style={{ marginTop: 8 }} />,
      onOk: () => sendTest(to, "test-other"),
    });
  };

  const settingsTab = (
    <div className="grid-2">
      <div>
        <Form.Item label={enableLabel} name="enabled" valuePropName="checked"><Switch /></Form.Item>
        <Form.Item label={t("邮件服务商")} name="provider">
          <Select options={[{ value: "", label: defaultName ? t("默认服务商（{0}）", defaultName) : t("默认服务商（未设置）") },
            ...providers.map((p) => ({ value: p.name, label: `${p.name} · ${p.kind === "mail-pool" ? t("邮件池") : p.kind.toUpperCase()}${p.enabled ? "" : t("（已停用）")}` }))]} />
        </Form.Item>
        <Form.Item label={t("时区")} name="timeZone" extra={t("模板中 *_local 时间按此时区显示")}><Input placeholder="Asia/Shanghai" /></Form.Item>
      </div>
      <div>
        <Card size="small" style={{ background: "var(--console-surface-soft)", marginTop: 30 }} title={t("测试邮件")}>
          <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
            {t("使用已保存的模板和邮件服务商，并填入“变量与数据”中的数据，发送一封测试邮件；通知停用时也可测试。保存或预览不会发信。")}
          </Typography.Paragraph>
          <Flex gap={8} wrap>
            <Button icon={<SendOutlined />} disabled={!editable || (busy && busy !== "test-me")} loading={busy === "test-me"} style={{ background: "#a36b19", borderColor: "#a36b19", color: "#fff" }} onClick={() => sendTest("", "test-me")}>{t("向我的邮箱发送测试邮件")}</Button>
            <Button disabled={!editable || !!busy} loading={busy === "test-other"} onClick={testOther}>{t("发送到其他地址")}</Button>
          </Flex>
        </Card>
      </div>
    </div>
  );
  const templateTab = (
    <>
      <Form.Item label={t("主题")} name={["template", "subject"]}><Input /></Form.Item>
      <Form.Item label={t("HTML 正文")} name={["template", "html"]}>
        <Input.TextArea rows={14} className="mono" />
      </Form.Item>
      <Form.Item label={t("常量 (JSON)")} name="constants" extra={t("模板中用 {{ constants.名称 }} 引用")}><Input.TextArea rows={3} className="mono" /></Form.Item>
      <Button icon={<EyeOutlined />} loading={busy === "preview"} onClick={preview}>{t("预览")}</Button>
      {rendered && (
        <Card size="small" title={t("预览：{0}", rendered.subject)} style={{ marginTop: 16 }}>
          <iframe title={t("邮件预览")} srcDoc={rendered.html} sandbox="" style={{ width: "100%", height: 420, border: "1px solid var(--console-border)", borderRadius: 8, background: "#fff" }} />
          <Collapse style={{ marginTop: 12 }} items={[{ key: "t", label: t("纯文本部分"), children: <pre className="secret mono">{rendered.text}</pre> }]} />
        </Card>
      )}
    </>
  );
  const dataTab = (
    <div className="grid-2">
      <div>
        <Flex justify="space-between" align="center" style={{ marginBottom: 10 }}>
          <Typography.Text strong>{t("可用变量")}</Typography.Text><span className="muted">{sample?.variables?.length || 0} {t("个")}</span>
        </Flex>
        <Table rowKey="name" size="small" pagination={false} scroll={{ y: 460 }} dataSource={sample?.variables || []}
          columns={[
            { title: t("变量"), dataIndex: "name", width: "50%", render: (v) => (
              <Flex gap={4} align="center">
                <Tooltip title={t("插入到正文末尾")}><Typography.Link className="mono" disabled={!editable || v.includes("<")} onClick={() => insert(v)}>{`{{ ${v} }}`}</Typography.Link></Tooltip>
                <Button type="text" size="small" icon={<CopyOutlined />} aria-label={t("复制")} onClick={() => copyText(message, `{{ ${v} }}`, t("变量"))} />
              </Flex>
            ) },
            { title: t("说明"), dataIndex: "description" },
          ]} />
      </div>
      <div>
        <Flex justify="space-between" align="center" style={{ marginBottom: 10 }}>
          <Typography.Text strong>{t("数据")}</Typography.Text>
          <Button size="small" icon={<CopyOutlined />} disabled={!sample} onClick={() => copyText(message, JSON.stringify(sample.context, null, 2), "JSON")}>{t("复制 JSON")}</Button>
        </Flex>
        {sample && (
          <Alert type={sample.source === "latest" ? "success" : "info"} showIcon style={{ marginBottom: 12 }}
            title={sample.source === "latest" ? t("最近一次真实事件的数据（{0}）", time(sample.updatedAt)) : t("还没有真实事件，以下为示例数据")}
            description={t("变量路径即 JSON 中的字段路径，例如 user.nickname；数组用序号，例如 xxx.0。")} />
        )}
        <pre className="secret mono" style={{ maxHeight: 400 }}>{sample ? JSON.stringify(sample.context, null, 2) : "—"}</pre>
      </div>
    </div>
  );

  return (
    <Spin spinning={!loaded}>
      <PageHead title={title} desc={desc}>
        <Button icon={<ReloadOutlined />} onClick={reload}>{t("重新加载")}</Button>
        {editable && <Button type="primary" icon={<CheckOutlined />} loading={busy === "save"} onClick={save}>{t("保存设置")}</Button>}
      </PageHead>
      <Card>
        <Tag color={enabled ? "success" : "default"} style={{ marginBottom: 12 }}>{enabled ? t("已启用") : t("未启用")}</Tag>
        <Form form={form} layout="vertical" disabled={!editable}>
          <Tabs items={[
            { key: "settings", label: t("发送设置"), forceRender: true, children: settingsTab },
            { key: "template", label: t("邮件模板"), forceRender: true, children: templateTab },
            { key: "data", label: t("变量与数据"), forceRender: true, children: dataTab },
          ]} />
        </Form>
      </Card>
    </Spin>
  );
}
