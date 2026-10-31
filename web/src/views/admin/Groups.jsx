import { useEffect, useMemo, useState } from "react";
import { App, Button, Card, Drawer, Dropdown, Flex, Form, Input, InputNumber, Modal, Radio, Select, Space, Table, Tag, TreeSelect, Typography } from "antd";
import { DeleteOutlined, DownOutlined, FileTextOutlined, FolderOpenOutlined, PlusOutlined, ReloadOutlined, SearchOutlined, TeamOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get, post, patch, del, errorText } from "../../api/client.js";
import { can } from "../../session.js";
import { list, useConfirm } from "../../hooks.js";
import { t } from "../../i18n.js";

const kindOf = (g) => (g.groupType === "ou" ? "ou" : g.groupClass === "posixGroup" ? "posix" : "ldap");
const kinds = { ou: ["OU", "default"], posix: ["POSIX", "blue"], ldap: ["LDAP", "green"] };

function toTree(rows) {
  const byId = new Map(rows.map((g) => [g.id, { ...g, children: [] }]));
  const roots = [];
  // A group whose parent is filtered out is listed at the top level.
  for (const g of byId.values()) (byId.get(g.parentId)?.children ?? roots).push(g);
  const prune = (n) => ({ ...n, children: n.children.length ? n.children.map(prune) : undefined });
  return roots.map(prune);
}

export default function Groups() {
  const { message, modal } = App.useApp();
  const confirm = useConfirm();
  const [groups, setGroups] = useState([]);
  const [loading, setLoading] = useState(false);
  const [filter, setFilter] = useState({ name: "", remark: "", kind: undefined });
  const [create, setCreate] = useState(false);
  const [editRow, setEditRow] = useState(null);
  const [members, setMembers] = useState({ open: false, group: null, list: [], add: [], candidates: [] });
  const [form] = Form.useForm();
  const [remarkForm] = Form.useForm();
  const kind = Form.useWatch("kind", form);

  const load = async () => {
    setLoading(true);
    try {
      setGroups(list(await get("/api/v2/groups")));
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

  const shown = groups.filter((g) => (!filter.name || g.groupName.toLowerCase().includes(filter.name.toLowerCase())) && (!filter.remark || (g.remark || "").includes(filter.remark)) && (!filter.kind || kindOf(g) === filter.kind));
  const tree = useMemo(() => toTree(shown), [shown]);
  const parentTree = useMemo(() => toTree(groups).map(function m(n) { return { value: n.id, title: n.groupName, children: n.children?.map(m) }; }), [groups]);

  const save = async () => {
    const f = await form.validateFields();
    const k = { posix: ["cn", "posixGroup"], unique: ["cn", "groupOfUniqueNames"], ou: ["ou", ""] }[f.kind];
    try {
      await post("/api/v2/groups", { groupName: f.groupName, groupType: k[0], groupClass: k[1], gidNumber: f.kind === "posix" ? f.gidNumber || 0 : 0, parentId: f.parentId || 0, remark: f.remark || "",
        ...(f.kind === "posix" ? { homeRoot: (f.homeRoot || "").replace(/\/+$/, ""), loginShell: f.loginShell || "" } : {}) });
      setCreate(false);
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const saveRemark = async () => {
    try {
      const f = await remarkForm.validateFields();
      await patch(`/api/v2/groups/${editRow.id}`, { remark: f.remark || "",
        ...(kindOf(editRow) === "posix" ? { homeRoot: (f.homeRoot || "").replace(/\/+$/, ""), loginShell: f.loginShell || "" } : {}) });
      setEditRow(null);
      load();
    } catch (e) {
      if (!e?.errorFields) message.error(errorText(e));
    }
  };
  const loadMembers = async (g) => (await get("/api/v2/users", { groupId: g.id, size: 100 })).items;
  const openMembers = async (g) => {
    try {
      setMembers({ open: true, group: g, list: await loadMembers(g), add: [], candidates: [] });
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const change = async (add, remove) => {
    try {
      await post(`/api/v2/groups/${members.group.id}/members`, { add, remove });
      setMembers({ ...members, add: [], list: await loadMembers(members.group) });
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const searchUsers = async (q) => {
    const items = q ? (await get("/api/v2/users", { q, size: 20 }).catch(() => ({ items: [] }))).items : [];
    setMembers((m) => ({ ...m, candidates: items }));
  };
  const more = async (key, row) => {
    if (key === "dn") return modal.info({ title: row.groupName, content: <pre className="secret mono">{row.groupDn || "—"}</pre>, okText: t("关闭") });
    if (!(await confirm(t("删除分组"), t("删除分组 {0}？", row.groupName)))) return;
    try {
      await del(`/api/v2/groups/${row.id}`);
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };

  return (
    <>
      <PageHead title={t("用户组与组织")}>
        <Button icon={<ReloadOutlined />} onClick={load}>{t("刷新")}</Button>
        {can("group", "create") && <Button type="primary" icon={<PlusOutlined />} onClick={() => { form.setFieldsValue({ kind: "posix", groupName: "", gidNumber: null, parentId: undefined, remark: "", homeRoot: "", loginShell: "" }); setCreate(true); }}>{t("新增分组")}</Button>}
      </PageHead>
      <Card>
        <Form layout="inline" colon={false} className="filters">
          <Form.Item label={t("名称")}><Input allowClear prefix={<SearchOutlined />} placeholder={t("分组名称")} style={{ width: 200 }} value={filter.name} onChange={(e) => setFilter({ ...filter, name: e.target.value })} /></Form.Item>
          <Form.Item label={t("描述")}><Input allowClear placeholder={t("描述")} style={{ width: 200 }} value={filter.remark} onChange={(e) => setFilter({ ...filter, remark: e.target.value })} /></Form.Item>
          <Form.Item label={t("类型")}>
            <Select allowClear placeholder={t("全部类型")} style={{ width: 160 }} value={filter.kind} onChange={(v) => setFilter({ ...filter, kind: v })}
              options={[{ value: "ou", label: t("OU 目录") }, { value: "posix", label: t("POSIX 组") }, { value: "ldap", label: t("LDAP 组") }]} />
          </Form.Item>
          <Form.Item><Button onClick={() => setFilter({ name: "", remark: "", kind: undefined })}>{t("重置")}</Button></Form.Item>
        </Form>
        <div className="list-meta">{t("显示")} {tree.length} {t("个顶层条目，共")} {shown.length} {t("个分组")}</div>
        <Table rowKey="id" loading={loading} dataSource={tree} pagination={false} expandable={{ defaultExpandAllRows: true }} scroll={{ x: 900 }}
          locale={{ emptyText: t("没有符合条件的分组") }}
          columns={[
            { title: t("名称"), render: (_, r) => <Space>{r.groupType === "ou" ? <FolderOpenOutlined /> : <TeamOutlined />}<Typography.Text strong>{r.groupName}</Typography.Text></Space> },
            { title: t("类型"), width: 110, render: (_, r) => <Tag color={kinds[kindOf(r)][1]}>{kinds[kindOf(r)][0]}</Tag> },
            { title: "GID", width: 100, render: (_, r) => <span className="mono">{r.gidNumber || "—"}</span> },
            { title: t("主组预设"), width: 260, ellipsis: true, render: (_, r) => (kindOf(r) !== "posix" ? "—" : r.homeRoot || r.loginShell
              ? <span className="mono" style={{ fontSize: 12 }}>{r.homeRoot ? t("{0}/<用户名>", r.homeRoot) : t("HOME 未设置")}{r.loginShell ? ` · ${r.loginShell}` : ""}</span>
              : <Typography.Text type="warning" style={{ fontSize: 12 }}>{t("未设置 HOME 根目录")}</Typography.Text>) },
            { title: t("描述"), dataIndex: "remark", ellipsis: true, render: (v) => v || "—" },
            { title: t("成员"), width: 80, render: (_, r) => (r.groupType === "ou" ? "—" : r.memberCount ?? 0) },
            { title: t("操作"), width: 200, fixed: "right", render: (_, r) => (
              <Space size={4}>
                {r.groupType !== "ou" && <Button type="link" size="small" onClick={() => openMembers(r)}>{t("成员")}</Button>}
                {can("group", "update") && <Button type="link" size="small" onClick={() => { remarkForm.setFieldsValue({ remark: r.remark, homeRoot: r.homeRoot, loginShell: r.loginShell }); setEditRow(r); }}>{t("编辑")}</Button>}
                <Dropdown trigger={["click"]} menu={{ onClick: ({ key }) => more(key, r), items: [
                  { key: "dn", icon: <FileTextOutlined />, label: t("查看 DN") },
                  ...(can("group", "delete") ? [{ type: "divider" }, { key: "delete", icon: <DeleteOutlined />, label: t("删除分组"), danger: true }] : []),
                ] }}><Button type="link" size="small">{t("更多")} <DownOutlined /></Button></Dropdown>
              </Space>
            ) },
          ]} />
      </Card>

      <Modal open={create} title={t("新增分组")} width={520} onCancel={() => setCreate(false)} onOk={save} okText={t("创建")} cancelText={t("取消")} forceRender>
        <Form form={form} layout="vertical">
          <Form.Item label={t("名称")} name="groupName" rules={[{ required: true, message: t("请输入名称") }]}><Input /></Form.Item>
          <Form.Item label={t("类型")} name="kind">
            <Radio.Group optionType="button" options={[{ value: "posix", label: t("POSIX 组") }, { value: "unique", label: t("普通组") }, { value: "ou", label: t("目录 (OU)") }]} />
          </Form.Item>
          {kind === "posix" && (
            <>
              <Form.Item label="GID" name="gidNumber"><InputNumber min={0} placeholder={t("留空自动分配")} style={{ width: "100%" }} /></Form.Item>
              <Form.Item label={t("HOME 根目录")} name="homeRoot" tooltip={t("以此组为主组的新用户 HOME 为 根目录/用户名；留空则创建用户时需手动填写 HOME")}
                rules={[{ pattern: /^\/.+/, message: t("需为绝对路径") }]}><Input className="mono" placeholder={t("/HOME/组名")} /></Form.Item>
              <Form.Item label={t("默认登录 Shell")} name="loginShell" tooltip={t("以此组为主组的新用户的登录 Shell；留空使用系统默认")}><Input className="mono" placeholder="/bin/bash" /></Form.Item>
            </>
          )}
          <Form.Item label={t("上级")} name="parentId"><TreeSelect allowClear treeDefaultExpandAll placeholder={t("根")} treeData={parentTree} /></Form.Item>
          <Form.Item label={t("描述")} name="remark"><Input /></Form.Item>
        </Form>
      </Modal>
      <Modal open={!!editRow} title={t("编辑 {0}", editRow?.groupName || "")} onCancel={() => setEditRow(null)} onOk={saveRemark} okText={t("保存")} cancelText={t("取消")} forceRender>
        <Form form={remarkForm} layout="vertical">
          <Form.Item label={t("描述")} name="remark"><Input /></Form.Item>
          {editRow && kindOf(editRow) === "posix" && (
            <>
              <p className="form-section">{t("主组预设")}</p>
              <Form.Item label={t("HOME 根目录")} name="homeRoot" tooltip={t("以此组为主组的新用户 HOME 为 根目录/用户名；留空则创建用户时需手动填写 HOME")}
                rules={[{ pattern: /^\/.+/, message: t("需为绝对路径") }]}><Input className="mono" placeholder={t("/HOME/组名")} /></Form.Item>
              <Form.Item label={t("默认登录 Shell")} name="loginShell" tooltip={t("以此组为主组的新用户的登录 Shell；留空使用系统默认")}><Input className="mono" placeholder="/bin/bash" /></Form.Item>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("预设只用于之后创建的用户，不会改动已有用户的 HOME 与 Shell。")}</Typography.Text>
            </>
          )}
        </Form>
      </Modal>

      <Drawer open={members.open} title={t("{0} 的成员", members.group?.groupName || "")} size={480} onClose={() => setMembers({ ...members, open: false })}>
        {can("group", "update") && (
          <Space.Compact style={{ width: "100%", marginBottom: 12 }}>
            <Select mode="multiple" showSearch filterOption={false} placeholder={t("搜索用户添加")} style={{ flex: 1 }} value={members.add}
              onSearch={searchUsers}
              onChange={(v) => setMembers({ ...members, add: v })}
              options={members.candidates.map((u) => ({ value: u.id, label: `${u.username} ${u.nickname || ""}` }))} />
            <Button type="primary" disabled={!members.add.length} onClick={() => change(members.add, [])}>{t("添加")}</Button>
          </Space.Compact>
        )}
        <Table rowKey="id" size="small" dataSource={members.list} pagination={false}
          columns={[
            { title: t("用户名"), dataIndex: "username" },
            { title: t("昵称"), dataIndex: "nickname" },
            ...(can("group", "update") ? [{ width: 80, render: (_, r) => <Button type="link" danger size="small" onClick={() => change([], [r.id])}>{t("移除")}</Button> }] : []),
          ]} />
        <Flex justify="flex-end" style={{ marginTop: 12 }}><Typography.Text type="secondary">{members.list.length} {t("名成员")}</Typography.Text></Flex>
      </Drawer>
    </>
  );
}
