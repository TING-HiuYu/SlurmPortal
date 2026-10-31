import { useEffect, useState } from "react";
import { App, Button, Card, Divider, Empty, Flex, Form, Input, InputNumber, Modal, Progress, Radio, Select, Space, Switch, Table, Tag, Typography } from "antd";
import { ArrowDownOutlined, ArrowUpOutlined, CopyOutlined, DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined, SendOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get, put, post, del, errorText } from "../../api/client.js";
import { can, hasPlugin } from "../../session.js";
import { useConfirm } from "../../hooks.js";
import { t } from "../../i18n.js";

const strategies = { priority: t("优先级"), "round-robin": t("轮询"), weighted: t("按权重") };
const periods = { day: t("天"), month: t("月"), total: t("总计") };
const strategyHelp = {
  priority: t("按顺序使用：排在前面的服务商用完本周期额度后，才轮到下一个。"),
  "round-robin": t("在仍有额度的成员之间依次轮流发送。"),
  weighted: t("按权重比例分配，例如权重 3:1 时约 75% 的邮件走第一个服务商。"),
};
const kindLabel = (id) => ({ smtp: "SMTP", rest: "REST", "mail-pool": t("邮件池") })[id] || id;
const blankSMTP = { host: "", port: 465, security: "tls", username: "", fromAddress: "", fromName: "", replyTo: "", timeoutMs: 15000 };
const purposes = {
  job: t("作业通知"), "job-test": t("作业通知测试"), transfer: t("传输通知"), "transfer-test": t("传输通知测试"),
  password: t("密码验证码"), "password-test": t("密码验证码测试"), "provider-test": t("服务商测试"), other: t("其他"),
};
// deliveryRows folds the daily counts into one row per provider and purpose: today, 7 and 30 days (UTC).
function deliveryRows(rows) {
  const day = (offset) => new Date(Date.now() - offset * 864e5).toISOString().slice(0, 10);
  const today = day(0), week = day(6);
  const out = new Map();
  for (const r of rows) {
    const key = `${r.provider}/${r.purpose}`;
    if (!out.has(key)) out.set(key, { provider: r.provider, purpose: r.purpose, today: { sent: 0, failed: 0 }, week: { sent: 0, failed: 0 }, month: { sent: 0, failed: 0 } });
    const e = out.get(key);
    for (const [k, ok] of [["today", r.day === today], ["week", r.day >= week], ["month", true]]) {
      if (ok) {
        e[k].sent += r.sent;
        e[k].failed += r.failed;
      }
    }
  }
  return [...out.values()].sort((a, b) => a.provider.localeCompare(b.provider) || a.purpose.localeCompare(b.purpose));
}
const blankREST = { request: "", fromAddress: "", fromName: "", timeoutMs: 15000, secretNames: [] };
const restExample = `curl -X POST "https://api.cloudflare.com/client/v4/accounts/<ACCOUNT_ID>/email/sending/send" \\
  -H "Authorization: Bearer {{ secrets.API_TOKEN }}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "to": {{ to }},
    "from": {"address": {{ from.address }}, "name": {{ from.name }}},
    "subject": {{ subject }},
    "html": {{ html }},
    "text": {{ text }}
  }'`;
async function copy(message, text) {
  try {
    await navigator.clipboard.writeText(text);
    message.success(t("已复制 {0}", text));
  } catch {
    message.error(t("复制失败，请手动选择文本"));
  }
}
const blankPool = { strategy: "priority", period: "day", failover: true, members: [] };

export default function Mail() {
  const { message, modal } = App.useApp();
  const confirm = useConfirm();
  const editable = can("mail", "update");
  const [providers, setProviders] = useState([]);
  const [kinds, setKinds] = useState([]);
  const [usage, setUsage] = useState([]);
  const [counts, setCounts] = useState([]);
  const [loading, setLoading] = useState(false);
  const [dialog, setDialog] = useState({ open: false, isNew: true, saving: false, secretSet: false });
  const [form] = Form.useForm();
  const kind = Form.useWatch("kind", form);
  const [testing, setTesting] = useState("");
  const restSecretRows = Form.useWatch("restSecrets", form);
  const strategy = Form.useWatch(["pool", "strategy"], form);
  const name = Form.useWatch("name", form);

  const load = async () => {
    setLoading(true);
    try {
      const [p, k, u, c] = await Promise.all([get("/api/v2/mail/providers"), get("/api/v2/mail/kinds"), hasPlugin("mail-pool") ? get("/api/v2/mail-pool/usage") : [], get("/api/v2/mail/usage")]);
      setProviders(p || []);
      setKinds(k || []);
      setUsage(u || []);
      setCounts(c || []);
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const edit = (row) => {
    form.resetFields();
    form.setFieldsValue({
      name: row?.name || "", kind: row?.kind || "smtp", enabled: row ? row.enabled : true, default: row ? row.default : !providers.some((p) => p.default), secret: "",
      smtp: row?.kind === "smtp" ? { ...blankSMTP, ...row.config } : blankSMTP,
      rest: row?.kind === "rest" ? { ...blankREST, ...row.config } : blankREST,
      restSecrets: row?.kind === "rest" ? (row.config.secretNames || []).map((name) => ({ name, value: "", stored: true })) : [],
      pool: row?.kind === "mail-pool" ? { ...blankPool, ...row.config, members: (row.config.members || []).map((m) => ({ ...m })) } : blankPool,
    });
    setDialog({ open: true, isNew: !row, saving: false, secretSet: !!row?.secretSet });
  };
  const save = async () => {
    const f = await form.validateFields();
    const all = form.getFieldsValue(true);
    const restSecrets = (all.restSecrets || []).filter((x) => x?.name);
    const config = f.kind === "smtp" ? all.smtp : f.kind === "rest" ? { ...all.rest, secretNames: restSecrets.map((x) => x.name) } : f.kind === "mail-pool" ? { ...all.pool, members: (all.pool.members || []).filter((m) => m?.provider) } : {};
    const secret = f.kind === "rest" ? JSON.stringify(Object.fromEntries(restSecrets.filter((x) => x.value).map((x) => [x.name, x.value]))) : f.secret;
    const body = { kind: f.kind, enabled: f.enabled, default: f.default, config, ...(secret ? { secret } : {}) };
    setDialog((d) => ({ ...d, saving: true }));
    try {
      await put(`/api/v2/mail/providers/${encodeURIComponent(f.name)}`, body);
      setDialog({ open: false });
      message.success(t("已保存"));
      load();
    } catch (e) {
      message.error(errorText(e));
      setDialog((d) => ({ ...d, saving: false }));
    }
  };
  const test = (row) => {
    let to = "";
    // The OK button spins while the provider is contacted; the row's button shows it too.
    modal.confirm({
      title: t("通过「{0}」发送测试邮件", row.name), okText: t("发送"), cancelText: t("取消"),
      content: <Input placeholder={t("收件地址，留空则发给自己")} onChange={(e) => (to = e.target.value)} style={{ marginTop: 8 }} />,
      onOk: async () => {
        setTesting(row.name);
        try {
          await post(`/api/v2/mail/providers/${encodeURIComponent(row.name)}/test`, to ? { to } : {});
          message.success(t("服务商已接受测试邮件"));
          load();
        } catch (e) {
          message.error(errorText(e));
          throw e;
        } finally {
          setTesting("");
        }
      },
    });
  };
  const remove = async (row) => {
    if (!(await confirm(t("确认删除"), t("删除邮件服务商「{0}」？使用它的插件将无法发信。", row.name)))) return;
    try {
      await del(`/api/v2/mail/providers/${encodeURIComponent(row.name)}`);
      message.success(t("已删除"));
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };

  return (
    <>
      <PageHead title={t("邮件服务")}>
        <Button icon={<ReloadOutlined />} onClick={load}>{t("刷新")}</Button>
        {editable && <Button type="primary" icon={<PlusOutlined />} onClick={() => edit()}>{t("新建服务商")}</Button>}
      </PageHead>

      <Card className="surface" title={t("邮件服务商")} extra={<span className="muted">{providers.length} {t("个")}</span>}>
        <Table rowKey="name" loading={loading} dataSource={providers} pagination={false} scroll={{ x: 900 }} locale={{ emptyText: t("还没有邮件服务商") }}
          columns={[
            { title: t("名称"), width: 200, render: (_, r) => <Space><span className="mono"><strong>{r.name}</strong></span>{r.default && <Tag color="blue">{t("默认")}</Tag>}</Space> },
            { title: t("类型"), width: 120, render: (_, r) => kindLabel(r.kind) },
            { title: t("配置"), render: (_, r) => r.kind === "smtp" ? (
              <><span className="mono">{r.config.host || "—"}:{r.config.port}</span><div className="muted">{r.config.fromName ? `${r.config.fromName} ` : ""}&lt;{r.config.fromAddress || t("未设置发件地址")}&gt;</div></>
            ) : r.kind === "rest" ? (
              <><span className="mono">{((r.config.request || "").match(/(?:-X|--request)\s+['"]?([A-Za-z]+)/) || [null, /\s(-d|--data)/.test(r.config.request || "") ? "POST" : "GET"])[1].toUpperCase()} {(r.config.request.match(/https?:\/\/[^\s"'\\/]+/) || [t("未设置请求")])[0]}</span>
                <div className="muted">{r.config.fromName ? `${r.config.fromName} ` : ""}&lt;{r.config.fromAddress || t("未设置发件地址")}&gt;{r.config.secretNames?.length ? t(" · {0} 个密钥", r.config.secretNames.length) : ""}</div></>
            ) : r.kind === "mail-pool" ? (
              <>{strategies[r.config.strategy]} {t("· 每")}{periods[r.config.period]}{r.config.failover ? t(" · 失败转移") : ""}
                <div className="muted">{(r.config.members || []).map((m) => m.provider + (m.quota ? `(${m.quota})` : "")).join(" → ")}</div></>
            ) : "—" },
            { title: t("状态"), width: 130, render: (_, r) => (
              <><Tag color={r.enabled ? "success" : "default"}>{r.enabled ? t("已启用") : t("已停用")}</Tag>
                {kinds.find((k) => k.id === r.kind)?.secret && !r.secretSet && <div className="muted" style={{ color: "var(--console-warning)" }}>{r.kind === "rest" ? (r.config.secretNames?.length ? t("未设置密钥") : "") : t("未设置密码")}</div>}</>
            ) },
            ...(editable ? [{ title: t("操作"), width: 220, fixed: "right", render: (_, r) => (
              <Space size={4}>
                <Button type="link" size="small" icon={<EditOutlined />} onClick={() => edit(r)}>{t("编辑")}</Button>
                <Button type="link" size="small" icon={<SendOutlined />} loading={testing === r.name} onClick={() => test(r)}>{t("测试")}</Button>
                <Button type="link" size="small" danger icon={<DeleteOutlined />} onClick={() => remove(r)}>{t("删除")}</Button>
              </Space>
            ) }] : []),
          ]} />
      </Card>

      <Card className="surface" title={t("发送统计")} extra={<span className="muted">{t("按实际发送的服务商计数（UTC 日期）")}</span>}>
        <Table rowKey={(r) => `${r.provider}/${r.purpose}`} size="small" pagination={false} dataSource={deliveryRows(counts)} locale={{ emptyText: t("最近 30 天没有发送记录") }}
          columns={[
            { title: t("服务商"), dataIndex: "provider", render: (v) => <span className="mono">{v}</span> },
            { title: t("用途"), dataIndex: "purpose", render: (v) => purposes[v] || v },
            ...[["today", t("今天")], ["week", t("近 7 天")], ["month", t("近 30 天")]].map(([k, title]) => ({
              title, align: "right", render: (_, r) => <span>{r[k].sent}{r[k].failed > 0 && <Typography.Text type="danger"> · {t("失败 {0}", r[k].failed)}</Typography.Text>}</span>,
            })),
          ]} />
      </Card>

      {hasPlugin("mail-pool") && (
        <Card className="surface" title={t("邮件池用量")} extra={<span className="muted">{t("按 UTC 周期计数")}</span>}>
          {!usage.length && <Empty description={t("还没有邮件池。新建服务商时选择“邮件池”类型，把多个服务商组合起来。")} />}
          {usage.map((p, pi) => (
            <div key={p.pool}>
              {pi > 0 && <Divider />}
              <Space wrap style={{ marginBottom: 14 }}>
                <strong className="mono">{p.pool}</strong>
                <Tag color={p.enabled ? "success" : "default"}>{p.enabled ? t("已启用") : t("已停用")}</Tag>
                <span className="muted">{strategies[p.strategy]} {t("· 周期")} {p.periodKey}{p.failover ? t(" · 失败转移") : ""}</span>
              </Space>
              <Flex vertical gap={10}>
                {p.members.map((m, i) => (
                  <Flex key={m.provider} align="center" gap={14} wrap>
                    <span className="order">{i + 1}</span>
                    <Flex vertical style={{ width: 170 }}>
                      <span className="mono">{m.provider}</span>
                      {m.missing ? <small style={{ color: "var(--console-warning)" }}>{t("服务商不存在")}</small> : !m.enabled ? <small className="muted">{t("已停用")}</small> : p.strategy === "weighted" && <small className="muted">{t("权重")} {m.weight}</small>}
                    </Flex>
                    <Flex vertical style={{ flex: 1, minWidth: 200, maxWidth: 520 }}>
                      <Progress size="small" showInfo={false} percent={m.quota ? Math.min(100, (m.sent / m.quota) * 100) : 0} status={m.quota && m.sent >= m.quota ? "exception" : "normal"} />
                      <span className="muted">{t("已发")} {m.sent}{m.quota ? ` / ${m.quota}` : t("（不限额）")}{m.failed ? t(" · 失败 {0}", m.failed) : ""}</span>
                    </Flex>
                  </Flex>
                ))}
              </Flex>
            </div>
          ))}
        </Card>
      )}

      <Modal open={dialog.open} title={dialog.isNew ? t("新建邮件服务商") : t("编辑 {0}", name)} width={720} maskClosable={false} onCancel={() => setDialog({ open: false })}
        onOk={save} okText={t("保存")} cancelText={t("取消")} confirmLoading={dialog.saving} forceRender>
        <Form form={form} labelCol={{ flex: "110px" }} labelAlign="left">
          <Form.Item label={t("名称")} name="name" rules={[{ required: true, pattern: /^[a-z0-9][a-z0-9_-]{0,63}$/, message: t("小写字母、数字、- _") }]}>
            <Input disabled={!dialog.isNew} placeholder={t("如 smtp1、brevo、pool")} className="mono" />
          </Form.Item>
          <Form.Item label={t("类型")} name="kind">
            <Radio.Group optionType="button" disabled={!dialog.isNew} options={kinds.map((k) => ({ value: k.id, label: kindLabel(k.id) }))} />
          </Form.Item>
          <Form.Item label={t("启用")} name="enabled" valuePropName="checked"><Switch /></Form.Item>
          <Form.Item label={t("默认服务商")} name="default" valuePropName="checked" extra={t("插件未指定服务商时使用")}><Switch /></Form.Item>
          {kind === "smtp" && (
            <>
              <Divider titlePlacement="start">SMTP</Divider>
              <Form.Item label={t("主机")} name={["smtp", "host"]}><Input placeholder="smtp.example.com" /></Form.Item>
              <Form.Item label={t("加密与端口")} name={["smtp", "security"]}>
                <Select style={{ width: 220 }} onChange={(v) => form.setFieldValue(["smtp", "port"], v === "tls" ? 465 : 587)}
                  options={[{ value: "tls", label: "TLS · 465" }, { value: "starttls", label: "STARTTLS · 587" }]} />
              </Form.Item>
              <Form.Item label={t("用户名")} name={["smtp", "username"]}><Input autoComplete="off" /></Form.Item>
              <Form.Item label={t("密码")} name="secret"><Input.Password autoComplete="new-password" placeholder={dialog.secretSet ? t("已设置，留空保持不变") : t("未设置")} /></Form.Item>
              <Form.Item label={t("发件地址")} name={["smtp", "fromAddress"]}><Input placeholder="noreply@example.com" /></Form.Item>
              <Form.Item label={t("发件人名称")} name={["smtp", "fromName"]}><Input /></Form.Item>
              <Form.Item label={t("回复地址")} name={["smtp", "replyTo"]}><Input /></Form.Item>
              <Form.Item label={t("超时 (ms)")} name={["smtp", "timeoutMs"]}><InputNumber min={1000} max={15000} step={1000} /></Form.Item>
            </>
          )}
          {kind === "rest" && (
            <>
              <Divider titlePlacement="start">{t("REST 请求")}</Divider>
              <Form.Item label={t("请求体")} name={["rest", "request"]} layout="vertical" labelCol={{ span: 24 }} wrapperCol={{ span: 24 }}
                extra={<>{t("cURL 格式：")}<span className="mono">-X</span> {t("请求方法（POST、PUT、PATCH、GET；省略时有")} <span className="mono">-d</span> {t("为 POST，否则为 GET）、URL、")}
                  <span className="mono">-H</span> {t("请求头和")} <span className="mono">-d</span> {t("请求内容。 点击下方变量复制。")}<a onClick={() => form.setFieldValue(["rest", "request"], restExample)}>{t("填入 Cloudflare Email Service 示例")}</a></>}>
                <Input.TextArea rows={12} className="mono" spellCheck={false} placeholder={restExample} />
              </Form.Item>
              <Card size="small" style={{ background: "var(--console-surface-soft)", marginBottom: 16 }} title={t("可用变量")}>
                <Flex gap={6} wrap style={{ marginBottom: 8 }}>
                  {(kinds.find((k) => k.id === "rest")?.placeholders || []).map((p) => (
                    <Tag key={p.name} style={{ cursor: "pointer" }} title={p.description} icon={<CopyOutlined />} onClick={() => copy(message, `{{ ${p.name} }}`)}>
                      <span className="mono">{`{{ ${p.name} }}`}</span> {p.description}
                    </Tag>
                  ))}
                  {(restSecretRows || []).filter((x) => x?.name).map((x) => (
                    <Tag key={x.name} color="gold" style={{ cursor: "pointer" }} icon={<CopyOutlined />} onClick={() => copy(message, `{{ secrets.${x.name} }}`)}>
                      <span className="mono">{`{{ secrets.${x.name} }}`}</span>
                    </Tag>
                  ))}
                </Flex>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {t("变量按位置自动编码：JSON 请求体中变成完整的 JSON 字符串（自带引号，不要再加引号）；表单请求体和 URL 中做 URL 编码；请求头中原样填入。 请求体格式由 Content-Type 请求头决定。返回 2xx 视为发送成功。")}
                </Typography.Text>
              </Card>
              <Form.Item label={t("密钥")} extra={t("自定义密钥变量，在请求中用 {{ secrets.名称 }} 引用。值加密保存且不会再显示；编辑时留空表示保持不变。")}>
                <Form.List name="restSecrets">
                  {(fields, { add, remove }) => (
                    <Flex vertical gap={8}>
                      {fields.map((field) => (
                        <Space.Compact key={field.key} style={{ width: "100%" }}>
                          <Form.Item noStyle name={[field.name, "name"]} normalize={(v) => (v || "").toUpperCase().replace(/[^A-Z0-9_]/g, "_")}>
                            <Input placeholder={t("名称，如 API_TOKEN")} className="mono" style={{ width: "40%" }} disabled={form.getFieldValue(["restSecrets", field.name, "stored"])} />
                          </Form.Item>
                          <Form.Item noStyle name={[field.name, "value"]}>
                            <Input.Password autoComplete="new-password" placeholder={form.getFieldValue(["restSecrets", field.name, "stored"]) ? t("已设置，留空保持不变") : t("值")} />
                          </Form.Item>
                          <Button icon={<DeleteOutlined />} onClick={() => remove(field.name)} />
                        </Space.Compact>
                      ))}
                      <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ name: "", value: "", stored: false })}>{t("添加密钥")}</Button>
                    </Flex>
                  )}
                </Form.List>
              </Form.Item>
              <Form.Item label={t("发件地址")} name={["rest", "fromAddress"]}><Input placeholder="no-reply@example.com" /></Form.Item>
              <Form.Item label={t("发件人名称")} name={["rest", "fromName"]} extra={t("{{ from.name }}；与发件地址组成 {{ from }}（名称 <地址>）。服务商要求分开填写时（如 Cloudflare 的 {\"address\", \"name\"}）用 {{ from.address }} 和 {{ from.name }}")}><Input /></Form.Item>
              <Form.Item label={t("超时 (ms)")} name={["rest", "timeoutMs"]}><InputNumber min={1000} max={15000} step={1000} /></Form.Item>
            </>
          )}
          {kind === "mail-pool" && (
            <>
              <Divider titlePlacement="start">{t("分发规则")}</Divider>
              <Form.Item label={t("策略")} name={["pool", "strategy"]} extra={strategyHelp[strategy]}>
                <Radio.Group optionType="button" options={Object.entries(strategies).map(([value, label]) => ({ value, label }))} />
              </Form.Item>
              <Form.Item label={t("额度周期")} name={["pool", "period"]}>
                <Select style={{ width: 160 }} options={[{ value: "day", label: t("每天 (UTC)") }, { value: "month", label: t("每月 (UTC)") }, { value: "total", label: t("总计") }]} />
              </Form.Item>
              <Form.Item label={t("失败转移")} name={["pool", "failover"]} valuePropName="checked" extra={t("一个服务商发送失败时改用下一个")}><Switch /></Form.Item>
              <Form.Item label={t("成员")} extra={t("额度 0 表示不限额。例：smtp1 额度 300、smtp2 额度 500、smtp3 不限额 = 每天前 300 封走 smtp1，接下来 500 封走 smtp2，其余走 smtp3。")}>
                <Form.List name={["pool", "members"]}>
                  {(fields, { add, remove, move }) => (
                    <Flex vertical gap={8}>
                      {fields.map((field, i) => (
                        <Flex key={field.key} align="center" gap={8} wrap>
                          <span className="order">{i + 1}</span>
                          <Form.Item noStyle name={[field.name, "provider"]}>
                            <Select placeholder={t("服务商")} style={{ width: 160 }} options={providers.filter((p) => p.name !== name).map((p) => ({ value: p.name, label: p.name }))} />
                          </Form.Item>
                          <Form.Item noStyle name={[field.name, "quota"]}><InputNumber min={0} max={10000000} step={100} addonAfter={t("封")} style={{ width: 150 }} /></Form.Item>
                          {strategy === "weighted" && <Form.Item noStyle name={[field.name, "weight"]}><InputNumber min={1} max={1000} placeholder={t("权重")} style={{ width: 90 }} /></Form.Item>}
                          <Button type="text" icon={<ArrowUpOutlined />} disabled={i === 0} onClick={() => move(i, i - 1)} />
                          <Button type="text" icon={<ArrowDownOutlined />} disabled={i === fields.length - 1} onClick={() => move(i, i + 1)} />
                          <Button type="text" danger icon={<DeleteOutlined />} onClick={() => remove(field.name)} />
                        </Flex>
                      ))}
                      <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ provider: undefined, quota: 0, weight: 1 })} style={{ width: 140 }}>{t("添加成员")}</Button>
                    </Flex>
                  )}
                </Form.List>
              </Form.Item>
            </>
          )}
        </Form>
      </Modal>
    </>
  );
}
