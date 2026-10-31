import { useEffect, useState } from "react";
import { App, Avatar, Button, Card, Col, Descriptions, Dropdown, Flex, Form, Input, Modal, Row, Select, Space, Switch, Table, Tag, TreeSelect, Typography } from "antd";
import { CloseCircleOutlined, DeleteOutlined, DownOutlined, KeyOutlined, PlusOutlined, ReloadOutlined, SafetyCertificateOutlined, SearchOutlined, SyncOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import SecretModal from "../../components/SecretModal.jsx";
import { get, post, put, patch, del, errorText } from "../../api/client.js";
import { can } from "../../session.js";
import { list, useConfirm } from "../../hooks.js";
import { time } from "../../format.js";
import { t } from "../../i18n.js";

const blank = { username: "", nickname: "", givenName: "", mail: "", jobNumber: "", mobile: "", uidNumber: "", gidNumber: "", homeDirectory: "", loginShell: "", password: "", introduction: "", groupIds: [], roleIds: [] };

export default function Users() {
  const { message } = App.useApp();
  const confirm = useConfirm();
  const [filter, setFilter] = useState({ q: "", status: 0, groupId: undefined, page: 1, size: 20 });
  const [page, setPage] = useState({ items: [], total: 0 });
  const [loading, setLoading] = useState(false);
  const [selected, setSelected] = useState([]);
  const [saving, setSaving] = useState({});
  const [groups, setGroups] = useState([]);
  const [roles, setRoles] = useState([]);
  const [cas, setCas] = useState([]);
  const [edit, setEdit] = useState({ open: false, id: 0, saving: false });
  const [issueDlg, setIssueDlg] = useState({ open: false, user: null, caId: undefined });
  const [secret, setSecret] = useState({ open: false });
  const [form] = Form.useForm();

  const roleName = (id) => roles.find((r) => r.id === id)?.name || `#${id}`;
  const posix = groups.filter((g) => g.groupClass === "posixGroup");
  const isPosix = (id) => posix.some((g) => g.id === id);
  const orgTree = (parent) => groups.filter((g) => (g.parentId || 0) === parent && g.groupClass !== "posixGroup")
    .map((g) => ({ value: g.id, title: g.groupName, disabled: g.groupType === "ou", children: orgTree(g.id) }));
  const groupName = (id) => groups.find((g) => g.id === id)?.groupName || `#${id}`;
  const mainId = Form.useWatch("mainGroup", form);
  const username = Form.useWatch("username", form);
  const mainPreset = posix.find((g) => g.id === mainId);

  const load = async (f = filter) => {
    setLoading(true);
    try {
      setPage(await get("/api/v2/users", { q: f.q, status: f.status || undefined, groupId: f.groupId || undefined, page: f.page, size: f.size }));
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setLoading(false);
    }
  };
  const apply = (patchF) => {
    const f = { ...filter, page: 1, ...patchF };
    setFilter(f);
    load(f);
  };
  useEffect(() => {
    load();
    Promise.allSettled([get("/api/v2/groups"), get("/api/v2/roles")]).then(([g, r]) => {
      if (g.status === "fulfilled") setGroups(list(g.value));
      if (r.status === "fulfilled") setRoles(list(r.value));
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // A POSIX account has one main group (its GID), chosen on create and fixed afterwards (v1);
  // other POSIX groups are supplementary, everything else is organisation membership.
  const openEdit = (row) => {
    setEdit({ open: true, id: row?.id || 0, saving: false, hadMain: false });
    if (!row) return form.setFieldsValue({ ...blank, mainGroup: undefined, supplementary: [], orgs: [] });
    const main = posix.find((g) => g.gidNumber && g.gidNumber === row.gidNumber)?.id;
    const ids = row.groupIds || [];
    setEdit({ open: true, id: row.id, saving: false, hadMain: ids.includes(main), main });
    form.setFieldsValue({ ...blank, ...row, roleIds: [...(row.roleIds || [])], mainGroup: main,
      supplementary: ids.filter((g) => isPosix(g) && g !== main), orgs: ids.filter((g) => !isPosix(g)) });
  };
  const save = async () => {
    const f = await form.validateFields();
    setEdit((e) => ({ ...e, saving: true }));
    try {
      const main = edit.id ? (edit.hadMain ? [edit.main] : []) : f.mainGroup ? [f.mainGroup] : [];
      const groupIds = [...main, ...(f.supplementary || []), ...(f.orgs || [])];
      const common = { nickname: f.nickname, givenName: f.givenName, mail: f.mail, jobNumber: f.jobNumber, mobile: f.mobile, homeDirectory: f.homeDirectory,
        loginShell: f.loginShell, introduction: f.introduction, groupIds, roleIds: f.roleIds };
      if (edit.id) await patch(`/api/v2/users/${edit.id}`, common);
      else {
        const gid = posix.find((g) => g.id === f.mainGroup)?.gidNumber || 0;
        const out = await post("/api/v2/users", { ...common, username: f.username, password: f.password, uidNumber: Number(f.uidNumber) || 0, gidNumber: gid });
        if (out?.initialPassword) setSecret({ open: true, title: t("{0} 的初始密码", f.username), text: out.initialPassword });
      }
      setEdit({ open: false });
      message.success(t("已保存"));
      load();
    } catch (e) {
      message.error(errorText(e));
      setEdit((x) => ({ ...x, saving: false }));
    }
  };
  const toggle = async (row) => {
    setSaving((s) => ({ ...s, [row.id]: true }));
    try {
      const status = row.status === 1 ? 2 : 1;
      await put(`/api/v2/users/${row.id}/status`, { status });
      setPage((p) => ({ ...p, items: p.items.map((u) => (u.id === row.id ? { ...u, status } : u)) }));
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setSaving((s) => ({ ...s, [row.id]: false }));
    }
  };
  const batch = async (title, rows, fn) => {
    if (!(await confirm(title, t("共 {0} 个用户", rows.length)))) return;
    const failed = [];
    for (const r of rows) await fn(r).catch(() => failed.push(r.username));
    if (failed.length) message.error(t("以下用户未能处理：{0}", failed.join(t("、"))));
    else message.success(t("已完成"));
    setSelected([]);
    load();
  };
  const showKey = (row, issued) => setSecret({ open: true, title: t("{0} 的私钥（只显示一次）", row.username), text: issued.privateKeyPem, filename: `${row.username}-private-key.pem` });
  const more = async (key, row) => {
    try {
      if (key === "reset") {
        if (!(await confirm(t("重置密码"), t("为 {0} 生成新密码？", row.username)))) return;
        const out = await post(`/api/v2/users/${row.id}/password-reset`);
        setSecret({ open: true, title: t("{0} 的新密码", row.username), text: out.password });
      } else if (key === "issue") {
        let list_ = cas;
        if (!list_.length) setCas((list_ = list(await get("/api/v2/pki/cas"))));
        setIssueDlg({ open: true, user: row, caId: list_.find((c) => c.enabled)?.id });
      } else if (key === "rotate") {
        if (!(await confirm(t("轮换证书"), t("轮换 {0} 的证书？旧证书将被吊销。", row.username)))) return;
        showKey(row, await post(`/api/v2/users/${row.id}/certificate/rotate`));
      } else if (key === "revoke") {
        if (!(await confirm(t("吊销证书"), t("吊销 {0} 的证书？该用户将无法用证书登录集群。", row.username)))) return;
        await del(`/api/v2/users/${row.id}/certificate`);
        message.success(t("已吊销"));
      } else if (key === "delete") {
        if (!(await confirm(t("删除用户"), t("删除用户 {0}？", row.username)))) return;
        await del(`/api/v2/users/${row.id}`);
        load();
      }
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const issue = async () => {
    try {
      showKey(issueDlg.user, await post(`/api/v2/users/${issueDlg.user.id}/certificate`, { caId: issueDlg.caId }));
      setIssueDlg({ open: false });
    } catch (e) {
      message.error(errorText(e));
    }
  };

  const moreItems = [
    can("user", "update") && { key: "reset", icon: <KeyOutlined />, label: t("重置密码") },
    can("pki", "create") && { key: "issue", icon: <SafetyCertificateOutlined />, label: t("签发证书") },
    can("pki", "update") && { key: "rotate", icon: <SyncOutlined />, label: t("轮换证书") },
    can("pki", "delete") && { key: "revoke", icon: <CloseCircleOutlined />, label: t("吊销证书") },
    can("user", "delete") && { type: "divider" },
    can("user", "delete") && { key: "delete", icon: <DeleteOutlined />, label: t("删除用户"), danger: true },
  ].filter(Boolean);

  const columns = [
    { title: t("用户名"), dataIndex: "username", sorter: (a, b) => a.username.localeCompare(b.username), width: 220,
      render: (_, r) => (
        <Flex align="center" gap={12}>
          <Avatar shape="square" style={{ background: "var(--console-accent-soft)", color: "var(--console-accent)", fontWeight: 650, borderRadius: 9 }}>{Array.from(r.nickname || r.username)[0]?.toUpperCase()}</Avatar>
          <Flex vertical><Typography.Text strong>{r.username}</Typography.Text><Typography.Text type="secondary" style={{ fontSize: 12 }}>{r.nickname || "—"}</Typography.Text></Flex>
        </Flex>
      ) },
    { title: t("邮箱"), dataIndex: "mail", ellipsis: true },
    { title: t("用户组"), ellipsis: true, render: (_, r) => (r.groupIds || []).map(groupName).join("、") || "—" },
    { title: t("角色"), width: 150, render: (_, r) => (r.roleIds || []).map((id) => <Tag key={id} color="blue" bordered={false}>{roleName(id)}</Tag>) },
    { title: t("状态"), width: 130, render: (_, r) => (
      <Space><Switch size="small" checked={r.status === 1} loading={saving[r.id]} disabled={!can("user", "update")} onChange={() => toggle(r)} />{r.status === 1 ? t("正常") : t("禁用")}</Space>
    ) },
    { title: t("操作"), width: 150, fixed: "right", render: (_, r) => (
      <Space size={4}>
        {can("user", "update") && <Button type="link" size="small" onClick={() => openEdit(r)}>{t("编辑")}</Button>}
        {moreItems.length > 0 && <Dropdown trigger={["click"]} menu={{ items: moreItems, onClick: ({ key }) => more(key, r) }}><Button type="link" size="small">{t("更多")} <DownOutlined /></Button></Dropdown>}
      </Space>
    ) },
  ];

  return (
    <>
      <PageHead title={t("用户管理")}>
        <Button icon={<ReloadOutlined />} onClick={() => load()}>{t("刷新")}</Button>
        {can("user", "create") && <Button type="primary" icon={<PlusOutlined />} onClick={() => openEdit()}>{t("新增用户")}</Button>}
      </PageHead>
      <Card>
        <Form layout="inline" colon={false} className="filters" onFinish={(v) => apply({ q: v.q || "" })}>
          <Form.Item label={t("用户名")} name="q"><Input allowClear prefix={<SearchOutlined />} placeholder={t("用户名 / 昵称 / 邮箱")} style={{ width: 220 }} onClear={() => apply({ q: "" })} /></Form.Item>
          <Form.Item label={t("状态")}><Select style={{ width: 130 }} value={filter.status} onChange={(v) => apply({ status: v })} options={[{ value: 0, label: t("全部") }, { value: 1, label: t("正常") }, { value: 2, label: t("禁用") }]} /></Form.Item>
          <Form.Item label={t("用户组")}>
            <Select allowClear showSearch optionFilterProp="label" placeholder={t("全部")} style={{ width: 180 }} value={filter.groupId} onChange={(v) => apply({ groupId: v })}
              options={groups.map((g) => ({ value: g.id, label: g.groupName }))} />
          </Form.Item>
          <Form.Item><Space><Button type="primary" htmlType="submit" loading={loading}>{t("查询")}</Button><Button htmlType="reset" onClick={() => apply({ q: "", status: 0, groupId: undefined })}>{t("重置")}</Button></Space></Form.Item>
        </Form>
        <div className="list-meta">
          <span>{t("共")} {page.total} {t("个用户")}</span>
          {selected.length > 0 && (
            <Space style={{ padding: "4px 12px", borderRadius: 8, background: "var(--console-accent-soft)", color: "var(--console-accent)" }}>
              {t("已选")} {selected.length} {t("项")}
              {can("user", "update") && <Button size="small" onClick={() => batch(t("批量启用"), selected.filter((r) => r.status !== 1), (r) => put(`/api/v2/users/${r.id}/status`, { status: 1 }))}>{t("批量启用")}</Button>}
              {can("user", "update") && <Button size="small" onClick={() => batch(t("批量禁用"), selected.filter((r) => r.status !== 2), (r) => put(`/api/v2/users/${r.id}/status`, { status: 2 }))}>{t("批量禁用")}</Button>}
              {can("user", "delete") && <Button size="small" danger onClick={() => batch(t("批量删除（不可撤销）"), selected, (r) => del(`/api/v2/users/${r.id}`))}>{t("批量删除")}</Button>}
            </Space>
          )}
        </div>
        <Table rowKey="id" loading={loading} dataSource={page.items} columns={columns} scroll={{ x: 1000 }}
          rowSelection={can("user", "update") ? { selectedRowKeys: selected.map((r) => r.id), onChange: (_, rows) => setSelected(rows) } : undefined}
          expandable={{ expandedRowRender: (r) => (
            <Descriptions size="small" column={{ xs: 1, md: 2, xl: 4 }} layout="vertical" colon={false} style={{ padding: "8px 12px" }}
              items={[
                { label: "DN", children: <span className="mono">{r.userDn || "—"}</span> },
                { label: "UID / GID", children: <span className="mono">{r.uidNumber || "—"} / {r.gidNumber || "—"}</span> },
                { label: "HOME", children: <span className="mono">{r.homeDirectory || "—"}</span> },
                { label: "Shell", children: <span className="mono">{r.loginShell || "—"}</span> },
                { label: t("工号"), children: r.jobNumber || "—" }, { label: t("手机"), children: r.mobile || "—" },
                { label: t("身份来源"), children: r.source || "—" }, { label: t("创建者"), children: r.creator || "—" },
                { label: t("创建时间"), children: time(r.createdAt) }, { label: t("花名"), children: r.givenName || "—" },
                { label: t("备注"), span: "filled", children: r.introduction || "—" },
              ]} />
          ) }}
          pagination={{ current: filter.page, pageSize: filter.size, total: page.total, showSizeChanger: true, pageSizeOptions: [10, 20, 50, 100], showTotal: (n) => t("共 {0} 条", n),
            onChange: (p, s) => { const f = { ...filter, page: s !== filter.size ? 1 : p, size: s }; setFilter(f); load(f); } }} />
      </Card>

      <Modal open={edit.open} title={edit.id ? t("编辑用户 {0}", form.getFieldValue("username") || "") : t("新增用户")} width={760} maskClosable={false} destroyOnHidden
        onCancel={() => setEdit({ open: false })} onOk={save} okText={t("保存")} cancelText={t("取消")} confirmLoading={edit.saving} forceRender>
        <Form form={form} layout="vertical" preserve={false}>
          <p className="form-section">{t("基本资料")}</p>
          <Row gutter={22}>
            <Col xs={24} sm={12}><Form.Item label={t("用户名")} name="username" rules={[{ required: true, message: t("请输入用户名") }]}><Input disabled={!!edit.id} placeholder={t("用户名（拼音）")} /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("中文名字")} name="nickname"><Input placeholder={t("中文名字")} /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("花名")} name="givenName"><Input /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("邮箱")} name="mail" rules={[{ type: "email", message: t("邮箱格式不正确") }]}><Input /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("手机号")} name="mobile"><Input /></Form.Item></Col>
            <Col xs={24} sm={12}><Form.Item label={t("工号")} name="jobNumber"><Input /></Form.Item></Col>
            {!edit.id && <Col xs={24} sm={12}><Form.Item label={t("初始密码")} name="password"><Input.Password autoComplete="new-password" placeholder={t("留空则自动生成")} /></Form.Item></Col>}
          </Row>
          <p className="form-section">{t("POSIX 身份")}</p>
          <Row gutter={22}>
            <Col xs={24} sm={12}><Form.Item label="UID" name="uidNumber"><Input disabled={!!edit.id} placeholder={t("留空自动分配")} /></Form.Item></Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("主组")} name="mainGroup" tooltip={t("每个用户必须有一个主组，决定 GID，并提供 HOME 与 Shell 预设；创建后不可更改")}
                extra={edit.id ? t("主组创建后不可更改") : undefined} rules={edit.id ? [] : [{ required: true, message: t("请选择主组") }]}>
                <Select showSearch optionFilterProp="label" disabled={!!edit.id} placeholder={t("选择 POSIX 组")}
                  options={posix.map((g) => ({ value: g.id, label: `${g.groupName} (GID ${g.gidNumber})` }))} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item label="HOME" name="homeDirectory" extra={!edit.id && mainPreset && !mainPreset.homeRoot ? t("所选主组没有 HOME 预设，需要填写") : undefined}
                rules={!edit.id && mainPreset && !mainPreset.homeRoot ? [{ required: true, message: t("请填写 HOME") }] : []}>
                <Input className="mono" placeholder={edit.id ? "" : mainPreset?.homeRoot ? `${mainPreset.homeRoot}/${username || t("<用户名>")}` : t("按主组预设生成")} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}><Form.Item label={t("登录 Shell")} name="loginShell"><Input className="mono" placeholder={edit.id ? "" : mainPreset?.loginShell || t("按主组预设")} /></Form.Item></Col>
          </Row>
          <p className="form-section">{t("成员关系与权限")}</p>
          <Row gutter={22}>
            <Col xs={24} sm={12}>
              <Form.Item label={t("所属组织")} name="orgs">
                <TreeSelect multiple treeDefaultExpandAll showSearch treeNodeFilterProp="title" placeholder={t("选择部门 / 普通组")} treeData={orgTree(0)} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item label={t("附加组")} name="supplementary" extra={t("主组以外的 POSIX 组")}>
                <Select mode="multiple" optionFilterProp="label" placeholder={t("选择附加 POSIX 组")}
                  options={posix.filter((g) => g.id !== form.getFieldValue("mainGroup")).map((g) => ({ value: g.id, label: `${g.groupName} (GID ${g.gidNumber})` }))} />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}><Form.Item label={t("角色")} name="roleIds"><Select mode="multiple" optionFilterProp="label" options={roles.map((r) => ({ value: r.id, label: r.name }))} /></Form.Item></Col>
            <Col span={24}><Form.Item label={t("备注")} name="introduction"><Input.TextArea rows={2} /></Form.Item></Col>
          </Row>
        </Form>
      </Modal>

      <Modal open={issueDlg.open} title={t("签发证书")} width={420} onCancel={() => setIssueDlg({ open: false })} onOk={issue} okText={t("签发")} cancelText={t("取消")} okButtonProps={{ disabled: !issueDlg.caId }}>
        <Select placeholder={t("选择 CA")} style={{ width: "100%" }} value={issueDlg.caId} onChange={(v) => setIssueDlg((d) => ({ ...d, caId: v }))}
          options={cas.map((c) => ({ value: c.id, label: `${c.name} (${c.kind})`, disabled: !c.enabled }))} />
      </Modal>
      <SecretModal {...secret} onClose={() => setSecret({ open: false })} />
    </>
  );
}
