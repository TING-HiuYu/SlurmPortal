import { useEffect, useMemo, useState } from "react";
import { App, Button, Card, Col, Flex, Form, Input, Modal, Radio, Row, Select, Space, Switch, Table, Tag, Typography } from "antd";
import { ArrowDownOutlined, ArrowRightOutlined, ArrowUpOutlined, DeleteOutlined, EditOutlined, PlusOutlined, ReloadOutlined, SearchOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get, post, put, del, errorText } from "../../api/client.js";
import { can } from "../../session.js";
import { list, useConfirm } from "../../hooks.js";
import { t } from "../../i18n.js";

const matches = { any: t("所有用户"), email: t("邮箱匹配"), username: t("用户名匹配"), org: t("GitHub 组织成员") };
const placeholders = { email: t("如 *@example.com，回车添加"), username: t("GitHub 用户名，回车添加"), org: t("组织名，回车添加") };
const providerName = (p) => ({ github: "GitHub" })[p] || p;
const callback = (p) => `${location.origin}/api/v2/oauth/callback/${p}`;
const describe = (r) => (r.match === "any" ? t("任何已验证账号") : `${matches[r.match] || r.match}：${r.values.join("、")}`);

export default function OAuth() {
  const { message } = App.useApp();
  const confirm = useConfirm();
  const [items, setItems] = useState([]);
  const [cas, setCas] = useState([]);
  const [roles, setRoles] = useState([]);
  const [groups, setGroups] = useState([]);
  const [loading, setLoading] = useState(false);
  const [filter, setFilter] = useState({ q: "", provider: undefined, enabled: undefined });
  const [dialog, setDialog] = useState({ open: false, id: 0, saving: false });
  const [form] = Form.useForm();
  const rules = Form.useWatch("rules", form) || [];
  const sshCAs = cas.filter((c) => c.kind === "ssh");
  const isPosix = (id) => groups.find((g) => g.id === id)?.groupClass === "posixGroup";
  const posixGroups = groups.filter((g) => g.groupClass === "posixGroup");
  const otherGroups = groups.filter((g) => g.groupClass !== "posixGroup" && g.groupType !== "ou");
  const caName = (id) => cas.find((c) => c.id === id)?.name || `#${id}`;
  const groupName = (id) => groups.find((g) => g.id === id)?.groupName || `#${id}`;
  const shown = useMemo(() => items.filter((k) => (!filter.q || `${k.name} ${k.description}`.toLowerCase().includes(filter.q.toLowerCase())) &&
    (!filter.provider || k.provider === filter.provider) && (filter.enabled === undefined || k.enabled === filter.enabled)), [items, filter]);

  const load = async () => {
    setLoading(true);
    try {
      setItems(list(await get("/api/v2/oauth/connectors")));
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    load();
    Promise.allSettled([get("/api/v2/pki/cas"), get("/api/v2/roles"), get("/api/v2/groups")]).then(([c, r, g]) => {
      if (c.status === "fulfilled") setCas(list(c.value));
      if (r.status === "fulfilled") setRoles(list(r.value));
      if (g.status === "fulfilled") setGroups(list(g.value));
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const openEdit = (row, provider = "github") => {
    form.resetFields();
    form.setFieldsValue(row
      ? { ...row, clientSecret: "", defaultRoleIds: [...(row.defaultRoleIds || [])],
          rules: row.rules.map((r) => ({ ...r, values: [...r.values], mainGroup: r.mainGroupId || r.groupIds.find(isPosix), extraGroups: r.groupIds.filter((g) => !isPosix(g)) })) }
      : { name: providerName(provider), provider, clientId: "", clientSecret: "", redirectUri: callback(provider), caId: sshCAs[0]?.id, defaultRoleIds: [], defaultStatus: 1,
          enabled: true, description: "", rules: [{ name: t("所有用户"), match: "any", values: [], mainGroup: undefined, extraGroups: [] }] });
    setDialog({ open: true, id: row?.id || 0, saving: false });
  };
  const save = async () => {
    try {
      await form.validateFields();
    } catch (e) {
      message.error(e.errorFields?.[0]?.errors?.[0] || t("请检查表单"));
      return;
    }
    const f = form.getFieldsValue(true);
    const body = { name: f.name, provider: f.provider, caId: f.caId, enabled: f.enabled, defaultRoleIds: f.defaultRoleIds, defaultStatus: f.defaultStatus,
      description: f.description || "", clientId: f.clientId, redirectUri: f.redirectUri,
      rules: f.rules.map((r) => ({ name: r.name || "", match: r.match, values: r.match === "any" ? [] : r.values || [],
        mainGroupId: r.mainGroup || 0, groupIds: r.extraGroups || [] })) };
    setDialog((d) => ({ ...d, saving: true }));
    try {
      if (dialog.id) await put(`/api/v2/oauth/connectors/${dialog.id}`, f.clientSecret ? { ...body, clientSecret: f.clientSecret } : body);
      else await post("/api/v2/oauth/connectors", { ...body, clientSecret: f.clientSecret });
      setDialog({ open: false });
      message.success(t("已保存"));
      load();
    } catch (e) {
      message.error(errorText(e));
      setDialog((d) => ({ ...d, saving: false }));
    }
  };
  const remove = async (row) => {
    if (!(await confirm(t("删除连接器"), t("删除连接器 {0}？已通过它登录的账号不受影响。", row.name)))) return;
    try {
      await del(`/api/v2/oauth/connectors/${row.id}`);
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };

  return (
    <>
      <PageHead title={t("OAuth 连接器")}>
        <Button icon={<ReloadOutlined />} onClick={load}>{t("刷新")}</Button>
        {can("oauth", "create") && <Button type="primary" icon={<PlusOutlined />} onClick={() => openEdit()}>{t("添加连接器")}</Button>}
      </PageHead>
      <Card>
        <Form layout="inline" colon={false} className="filters">
          <Form.Item label={t("关键字")}><Input allowClear prefix={<SearchOutlined />} placeholder={t("名称 / 描述")} style={{ width: 200 }} value={filter.q} onChange={(e) => setFilter({ ...filter, q: e.target.value })} /></Form.Item>
          <Form.Item label={t("提供方")}>
            <Select allowClear placeholder={t("全部")} style={{ width: 150 }} value={filter.provider} onChange={(v) => setFilter({ ...filter, provider: v })}
              options={[...new Set(items.map((k) => k.provider))].map((p) => ({ value: p, label: providerName(p) }))} />
          </Form.Item>
          <Form.Item label={t("状态")}>
            <Select allowClear placeholder={t("全部")} style={{ width: 130 }} value={filter.enabled} onChange={(v) => setFilter({ ...filter, enabled: v })}
              options={[{ value: true, label: t("启用") }, { value: false, label: t("停用") }]} />
          </Form.Item>
          <Form.Item><Button icon={<ReloadOutlined />} onClick={() => setFilter({ q: "", provider: undefined, enabled: undefined })}>{t("重置")}</Button></Form.Item>
        </Form>
        <Table rowKey="id" loading={loading} dataSource={shown} scroll={{ x: 1000 }} locale={{ emptyText: t("还没有 OAuth 连接器") }}
          pagination={{ pageSize: 10, hideOnSinglePage: true, showTotal: (n) => t("共 {0} 条", n) }}
          expandable={{ expandedRowRender: (k) => (
            <Flex vertical gap={9} style={{ padding: "4px 12px" }}>
              {k.rules.map((r, i) => (
                <Flex key={i} align="center" gap={10} wrap>
                  <span className="order">{i + 1}</span>
                  <Typography.Text strong>{r.name || matches[r.match]}</Typography.Text>
                  <Typography.Text type="secondary">{describe(r)}</Typography.Text>
                  <ArrowRightOutlined style={{ color: "var(--console-placeholder)" }} />
                  {(r.mainGroupId || r.groupIds.find(isPosix))
                    ? <Tag color="blue">{t("主组")} {groupName(r.mainGroupId || r.groupIds.find(isPosix))}</Tag>
                    : <Tag color="error">{t("未设置主组")}</Tag>}
                  {r.groupIds.filter((g) => g !== r.mainGroupId && !isPosix(g)).map((g) => <Tag key={g}>{groupName(g)}</Tag>)}
                </Flex>
              ))}
              <Flex align="center" gap={10}><span className="order">—</span><Typography.Text type="secondary">{t("其余用户：拒绝登录")}</Typography.Text></Flex>
            </Flex>
          ) }}
          columns={[
            { title: t("名称"), dataIndex: "name", ellipsis: true, render: (v, k) => <Flex vertical><Typography.Text strong>{v}</Typography.Text><Typography.Text type="secondary" style={{ fontSize: 12 }}>{k.description || "—"}</Typography.Text></Flex> },
            { title: t("提供方"), dataIndex: "provider", width: 100, render: providerName },
            { title: t("签发 CA"), width: 180, ellipsis: true, render: (_, k) => `#${k.caId} ${caName(k.caId)}` },
            { title: t("启用"), width: 80, render: (_, k) => <Tag color={k.enabled ? "success" : "default"}>{k.enabled ? t("是") : t("否")}</Tag> },
            { title: t("默认角色"), width: 150, render: (_, k) => (k.defaultRoleIds || []).map((id) => <Tag key={id} color="blue" bordered={false}>{roles.find((r) => r.id === id)?.name || `#${id}`}</Tag>) },
            { title: t("规则"), width: 180, render: (_, k) => t("{0} 条 · {1}", k.rules.length, k.rules[0] ? describe(k.rules[0]) : "—") },
            { title: t("操作"), width: 150, fixed: "right", render: (_, k) => (
              <Space size={4}>
                {can("oauth", "update") && <Button type="link" size="small" icon={<EditOutlined />} onClick={() => openEdit(k)}>{t("编辑")}</Button>}
                {can("oauth", "delete") && <Button type="link" size="small" danger icon={<DeleteOutlined />} onClick={() => remove(k)}>{t("删除")}</Button>}
              </Space>
            ) },
          ]} />
      </Card>

      <Modal open={dialog.open} title={dialog.id ? t("编辑连接器 {0}", form.getFieldValue("name") || "") : t("新建连接器")} width={860} maskClosable={false}
        onCancel={() => setDialog({ open: false })} onOk={save} okText={t("保存")} cancelText={t("取消")} confirmLoading={dialog.saving} forceRender>
        <Form form={form} layout="vertical">
          <p className="form-section">{t("基本设置")}</p>
          <Row gutter={20}>
            <Col xs={24} sm={12}><Form.Item label={t("名称")} name="name" rules={[{ required: true }]}><Input placeholder={t("如 GitHub · 实验室")} /></Form.Item></Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("登录方式")} name="provider">
                <Select disabled={!!dialog.id} onChange={(v) => form.setFieldValue("redirectUri", callback(v))} options={[{ value: "github", label: "GitHub" }]} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}><Form.Item label="Client ID" name="clientId" rules={[{ required: true }]}><Input className="mono" /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label="Client Secret" name="clientSecret"><Input.Password placeholder={dialog.id ? t("留空则保持不变") : t("Secret 或 env:变量名")} /></Form.Item></Col>
            <Col span={24}><Form.Item label={t("回调地址")} name="redirectUri"><Input className="mono" /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("签发证书 CA")} name="caId"><Select options={sshCAs.map((c) => ({ value: c.id, label: c.name }))} /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("说明")} name="description"><Input /></Form.Item></Col>
            <Col span={24}><Form.Item label={t("启用")} name="enabled" valuePropName="checked"><Switch /></Form.Item></Col>
          </Row>
          <p className="form-section">{t("新用户默认设置")}</p>
          <Row gutter={20}>
            <Col xs={24} sm={12}><Form.Item label={t("角色")} name="defaultRoleIds"><Select mode="multiple" options={roles.map((r) => ({ value: r.id, label: r.name }))} /></Form.Item></Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("账号状态")} name="defaultStatus"><Radio.Group options={[{ value: 1, label: t("直接启用") }, { value: 2, label: t("待管理员审核") }]} /></Form.Item>
            </Col>
          </Row>
          <p className="form-section">{t("筛选与分配规则")}</p>
          <Typography.Paragraph type="secondary" style={{ fontSize: 12 }}>{t("按顺序匹配，第一条命中的规则把新用户放入它的主组和附加组；没有规则命中的账号不能通过此连接器登录。每条规则必须选择主组（POSIX 组，决定 GID），新用户的 HOME 与 Shell 取自主组预设（在 用户组 中设置）。值支持通配符 *，例如 *@example.com。")}</Typography.Paragraph>
          <Form.List name="rules">
            {(fields, { add, remove, move }) => (
              <Flex vertical gap={10} align="flex-start">
                {fields.map((field, i) => (
                  <Flex key={field.key} align="center" gap={8} wrap style={{ width: "100%", padding: "10px 12px", border: "1px solid var(--console-border-soft)", borderRadius: 8, background: "var(--console-surface-soft)" }}>
                    <span className="order">{i + 1}</span>
                    <Form.Item noStyle name={[field.name, "name"]}><Input placeholder={t("规则名称")} style={{ width: 130 }} /></Form.Item>
                    <Form.Item noStyle name={[field.name, "match"]}><Select style={{ width: 150 }} options={Object.entries(matches).map(([value, label]) => ({ value, label }))} /></Form.Item>
                    {rules[i]?.match !== "any"
                      ? <Form.Item noStyle name={[field.name, "values"]}><Select mode="tags" open={false} tokenSeparators={[",", " "]} placeholder={placeholders[rules[i]?.match]} style={{ flex: 1, minWidth: 180 }} /></Form.Item>
                      : <Typography.Text type="secondary" style={{ flex: 1, minWidth: 180 }}>{t("匹配所有已验证账号")}</Typography.Text>}
                    <Form.Item noStyle name={[field.name, "mainGroup"]} rules={[{ required: true, message: t("规则 {0} 需要选择主组", i + 1) }]}>
                      <Select showSearch optionFilterProp="label" placeholder={t("主组（必选）")} style={{ width: 200 }}
                        status={rules[i] && !rules[i].mainGroup ? "warning" : undefined}
                        options={posixGroups.map((g) => ({ value: g.id, label: `${g.groupName} (GID ${g.gidNumber})`, title: g.homeRoot ? t("HOME {0}/<用户名>", g.homeRoot) : t("未设置 HOME 根目录") }))} />
                    </Form.Item>
                    <Form.Item noStyle name={[field.name, "extraGroups"]}>
                      <Select mode="multiple" maxTagCount="responsive" optionFilterProp="label" placeholder={t("附加组（可留空）")} style={{ width: 180 }}
                        options={otherGroups.map((g) => ({ value: g.id, label: g.groupName }))} />
                    </Form.Item>
                    <Space.Compact>
                      <Button icon={<ArrowUpOutlined />} disabled={i === 0} onClick={() => move(i, i - 1)} />
                      <Button icon={<ArrowDownOutlined />} disabled={i === fields.length - 1} onClick={() => move(i, i + 1)} />
                      <Button danger icon={<DeleteOutlined />} disabled={fields.length === 1} onClick={() => remove(field.name)} />
                    </Space.Compact>
                  </Flex>
                ))}
                <Button type="dashed" icon={<PlusOutlined />} onClick={() => add({ name: "", match: "email", values: [], mainGroup: undefined, extraGroups: [] })}>{t("添加规则")}</Button>
              </Flex>
            )}
          </Form.List>
        </Form>
      </Modal>
    </>
  );
}
