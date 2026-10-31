import { useEffect, useState } from "react";
import { App, Badge, Button, Card, Col, Flex, Form, Input, InputNumber, Modal, Row, Select, Space, Switch, Table, Tag, Tooltip, Typography } from "antd";
import { CodeOutlined, EditOutlined, FolderOpenOutlined, ReloadOutlined, SwapOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get, put, errorText } from "../../api/client.js";
import { can } from "../../session.js";
import { list } from "../../hooks.js";
import { ago, time } from "../../format.js";
import { t } from "../../i18n.js";

const featureTags = [
  ["terminal", t("网页终端"), CodeOutlined],
  ["files", t("文件浏览"), FolderOpenOutlined],
  ["transfer", t("跨集群传输"), SwapOutlined],
];

// Each bound gateway is one cluster. Features switch on by themselves when its gateway registers
// and reports them; administrators only maintain the connection details.
function status(c) {
  if (!c.enabled) return ["default", t("已停用")];
  if (!c.registration) return ["default", t("网关未注册")];
  if (!c.registration.fresh) return ["error", t("网关离线")];
  if (!c.gatewayUrl) return ["warning", t("待填写网关地址")];
  return ["success", t("可用")];
}

const strip = ({ registration, features, ready, ...c }) => c;

export default function WebAccess() {
  const { message } = App.useApp();
  const [form] = Form.useForm();
  const [view, setView] = useState(null);
  const [rows, setRows] = useState([]);
  const [editing, setEditing] = useState(-1);
  const [cas, setCas] = useState([]);
  const [roles, setRoles] = useState([]);
  const [keys, setKeys] = useState([]);
  const [saving, setSaving] = useState(false);
  const editable = can("web-access", "update");

  const show = (v) => {
    setView(v);
    setRows(v.clusters.map(strip));
  };
  const load = () =>
    Promise.allSettled([get("/api/v2/web-access/config"), get("/api/v2/pki/cas"), get("/api/v2/roles"), get("/api/v2/web-access/keys")]).then(([c, a, r, k]) => {
      if (c.status === "fulfilled") show(c.value);
      else message.error(errorText(c.reason));
      if (a.status === "fulfilled") setCas(list(a.value).filter((x) => x.kind === "ssh" && x.enabled));
      if (r.status === "fulfilled") setRoles(list(r.value));
      if (k.status === "fulfilled") setKeys(k.value.keys || []);
    });
  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const save = async (next = rows) => {
    setSaving(true);
    try {
      show(await put("/api/v2/web-access/config", { clusters: next }));
      message.success(t("已保存"));
      return true;
    } catch (e) {
      message.error(errorText(e));
      return false;
    } finally {
      setSaving(false);
    }
  };
  const openEdit = (i) => {
    form.setFieldsValue(rows[i]);
    setEditing(i);
  };
  const saveEdit = async () => {
    const v = await form.validateFields();
    const next = rows.map((r, i) => (i === editing ? { ...r, ...v } : r));
    if (await save(next)) setEditing(-1);
  };

  const data = (view?.clusters || []).map((c, i) => ({ ...c, ...rows[i], registration: c.registration, features: c.features, index: i }));
  return (
    <>
      <PageHead title={t("集群网页访问")}>
        <Button icon={<ReloadOutlined />} onClick={load}>{t("刷新")}</Button>
        {editable && <Button type="primary" loading={saving} disabled={!view} onClick={() => save()}>{t("保存配置")}</Button>}
      </PageHead>
      <Card className="surface" title={t("集群配置")} loading={!view}>
        <Table rowKey="id" dataSource={data} pagination={false} scroll={{ x: 1000 }}
          locale={{ emptyText: t("没有绑定网关。网关通过客户端证书绑定到集群（部署配置 web-access.gateways）。") }}
          columns={[
            { title: t("集群"), render: (_, c) => <Flex vertical><Typography.Text strong>{c.name}</Typography.Text><span className="muted mono">{c.id}</span></Flex> },
            { title: t("公共登录地址"), render: (_, c) => <span className="mono">{c.address ? `${c.address}:${c.sshPort}` : "—"}</span> },
            { title: t("网关"), width: 230, render: (_, c) => {
              const [badge, label] = status(c);
              return (
                <Flex vertical gap={2}>
                  <span className="mono">{c.gatewayId}</span>
                  <Tooltip title={c.registration ? t("最近注册 {0}（{1}）", time(c.registration.registeredAt), c.registration.instanceId) : t("从未注册")}>
                    <Badge status={badge} text={<span style={{ fontSize: 12 }}>{label}{c.registration ? ` · ${ago(c.registration.registeredAt)}` : ""}</span>} />
                  </Tooltip>
                </Flex>
              );
            } },
            { title: t("网关前端地址"), ellipsis: true, render: (_, c) => (c.gatewayUrl ? <span className="mono">{c.gatewayUrl}</span> : <Typography.Text type="warning">{t("未填写")}</Typography.Text>) },
            { title: t("功能（网关报告）"), width: 250, render: (_, c) => (
              <Flex gap={4} wrap>{featureTags.map(([k, text, Icon]) => <Tag key={k} icon={<Icon />} color={c.features?.[k] ? "success" : "default"}>{text}</Tag>)}</Flex>
            ) },
            { title: t("启用"), width: 80, render: (_, c) => (
              <Switch size="small" checked={c.enabled} disabled={!editable} onChange={(v) => setRows(rows.map((r, i) => (i === c.index ? { ...r, enabled: v } : r)))} />
            ) },
            { title: t("操作"), width: 90, fixed: "right", render: (_, c) => editable && <Button type="link" size="small" icon={<EditOutlined />} onClick={() => openEdit(c.index)}>{t("编辑")}</Button> },
          ]} />
      </Card>
      {keys.length > 0 && (
        <Card className="surface" title={t("网关验票公钥")}>
          {keys.map((k) => <Typography.Paragraph key={k.kid} copyable={{ text: k.x }} className="secret mono">{k.kid}: {k.x}</Typography.Paragraph>)}
        </Card>
      )}

      <Modal open={editing >= 0} title={t("编辑集群 {0}", rows[editing]?.name || "")} width={720} maskClosable={false} onCancel={() => setEditing(-1)}
        onOk={saveEdit} okText={t("保存")} cancelText={t("取消")} confirmLoading={saving} forceRender>
        <Form form={form} layout="vertical">
          <Row gutter={20}>
            <Col xs={24} md={10}><Form.Item label={t("显示名称")} name="name" rules={[{ required: true }]}><Input /></Form.Item></Col>
            <Col xs={24} md={14}>
              <Form.Item label={t("网关前端地址")} name="gatewayUrl" tooltip={t("浏览器连接网关所用的 HTTPS 地址（含端口），留空则暂不对用户开放")}>
                <Input placeholder="https://cluster1-login.example.org:19933" className="mono" />
              </Form.Item>
            </Col>
            <Col xs={24} md={16}><Form.Item label={t("公开 SSH 地址")} name="address" tooltip={t("其他集群传输到此集群时连接的地址")}><Input placeholder="cluster1-login.example.org" className="mono" /></Form.Item></Col>
            <Col xs={24} md={8}><Form.Item label={t("公开 SSH 端口")} name="sshPort"><InputNumber min={1} max={65535} style={{ width: "100%" }} /></Form.Item></Col>
            <Col xs={24} md={16}><Form.Item label={t("网关本地 SSH 主机")} name="sshHost" tooltip={t("网关在登录节点上连接 sshd 的地址")}><Input placeholder="127.0.0.1" className="mono" /></Form.Item></Col>
            <Col xs={24} md={8}><Form.Item label={t("网关本地 SSH 端口")} name="sshLocalPort"><InputNumber min={1} max={65535} style={{ width: "100%" }} /></Form.Item></Col>
            <Col xs={24} md={12}>
              <Form.Item label={t("签发 CA")} name="sshCaId">
                <Select options={[{ value: 0, label: t("自动：匹配登录节点信任的 CA") }, ...cas.map((x) => ({ value: x.id, label: x.name }))]} />
              </Form.Item>
            </Col>
            <Col xs={24} md={12}>
              <Form.Item label={t("允许使用的角色")} name="allowedRoles">
                <Select mode="tags" options={roles.map((r) => ({ value: r.keyword, label: `${r.name} (${r.keyword})` }))} />
              </Form.Item>
            </Col>
            <Col span={24}><Form.Item label={t("启用")} name="enabled" valuePropName="checked"><Switch /></Form.Item></Col>
          </Row>
          <Space><span className="muted">{t("网关：")}</span><span className="mono">{rows[editing]?.gatewayId}</span><span className="muted">{t("（由客户端证书绑定，不可修改）")}</span></Space>
        </Form>
      </Modal>
    </>
  );
}
