import { useEffect, useMemo, useState } from "react";
import { App, Button, Card, Checkbox, Drawer, Form, Input, InputNumber, Modal, Select, Space, Switch, Table, Tag, Typography } from "antd";
import { DeleteOutlined, EditOutlined, KeyOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get, post, put, patch, del, errorText } from "../../api/client.js";
import { can, useSession } from "../../session.js";
import { useConfirm } from "../../hooks.js";
import { t } from "../../i18n.js";

const actions = ["read", "create", "update", "delete"];
const actionLabel = { read: t("查看"), create: t("新建"), update: t("修改"), delete: t("删除") };
const items = (v) => (Array.isArray(v) ? v : v?.items || v?.permissions || []);

export default function Roles() {
  const { message } = App.useApp();
  const confirm = useConfirm();
  const { permissions } = useSession();
  const [roles, setRoles] = useState([]);
  const [loading, setLoading] = useState(false);
  const [edit, setEdit] = useState({ open: false, id: 0 });
  const [perms, setPerms] = useState({ open: false, role: null, set: {} });
  const [form] = Form.useForm();
  const [filter, setFilter] = useState({ name: "", keyword: "", status: 0 });
  const shown = roles.filter((r) => (!filter.name || r.name.includes(filter.name)) && (!filter.keyword || r.keyword.includes(filter.keyword)) && (!filter.status || r.status === filter.status));

  // The grantable catalogue is every route policy the current administrator holds.
  const catalog = useMemo(() => {
    const by = {};
    for (const key of permissions) {
      const [object, action] = key.split(":");
      (by[object] ||= []).push(action);
    }
    return Object.keys(by).sort().map((object) => ({ object, actions: by[object] }));
  }, [permissions]);

  const load = async () => {
    setLoading(true);
    try {
      setRoles(items(await get("/api/v2/roles")));
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

  const openEdit = (row) => {
    form.setFieldsValue(row ? { ...row } : { name: "", keyword: "", remark: "", sort: 10, status: 1 });
    setEdit({ open: true, id: row?.id || 0 });
  };
  const save = async () => {
    const { name, keyword, remark, sort, status } = await form.validateFields();
    try {
      if (edit.id) await patch(`/api/v2/roles/${edit.id}`, { name, remark, sort, status });
      else await post("/api/v2/roles", { name, keyword, remark, sort });
      setEdit({ open: false });
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const remove = async (row) => {
    if (!(await confirm(t("删除角色"), t("删除角色 {0}？", row.name)))) return;
    try {
      await del(`/api/v2/roles/${row.id}`);
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const openPerms = async (row) => {
    try {
      const set = {};
      for (const p of items(await get(`/api/v2/roles/${row.id}/permissions`))) {
        if (p.object === "*" && p.action === "*") for (const c of catalog) for (const a of c.actions) set[`${c.object}:${a}`] = true;
        else set[`${p.object}:${p.action}`] = true;
      }
      setPerms({ open: true, role: row, set });
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const savePerms = async () => {
    const list = Object.entries(perms.set).filter(([, on]) => on).map(([k]) => {
      const [object, action] = k.split(":");
      return { object, action };
    });
    try {
      await put(`/api/v2/roles/${perms.role.id}/permissions`, { permissions: list });
      message.success(t("权限已更新"));
      setPerms({ ...perms, open: false });
    } catch (e) {
      message.error(errorText(e));
    }
  };

  return (
    <>
      <PageHead title={t("角色权限")}>
        <Button icon={<ReloadOutlined />} onClick={load}>{t("刷新")}</Button>
        {can("role", "create") && <Button type="primary" icon={<PlusOutlined />} onClick={() => openEdit()}>{t("创建角色")}</Button>}
      </PageHead>
      <Card>
        <Form layout="inline" colon={false} className="filters">
          <Form.Item label={t("角色名称")}><Input allowClear placeholder={t("角色名称")} style={{ width: 180 }} value={filter.name} onChange={(e) => setFilter({ ...filter, name: e.target.value })} /></Form.Item>
          <Form.Item label={t("关键字")}><Input allowClear placeholder={t("关键字")} style={{ width: 180 }} value={filter.keyword} onChange={(e) => setFilter({ ...filter, keyword: e.target.value })} /></Form.Item>
          <Form.Item label={t("角色状态")}>
            <Select style={{ width: 140 }} value={filter.status} onChange={(v) => setFilter({ ...filter, status: v })} options={[{ value: 0, label: t("全部") }, { value: 1, label: t("正常") }, { value: 2, label: t("停用") }]} />
          </Form.Item>
          <Form.Item><Button icon={<ReloadOutlined />} onClick={() => setFilter({ name: "", keyword: "", status: 0 })}>{t("重置筛选")}</Button></Form.Item>
        </Form>
        <Table rowKey="id" loading={loading} dataSource={shown} scroll={{ x: 900 }} pagination={{ pageSize: 10, hideOnSinglePage: true, showTotal: (n) => t("共 {0} 条", n) }}
          columns={[
            { title: t("角色名称"), dataIndex: "name", sorter: (a, b) => a.name.localeCompare(b.name) },
            { title: t("关键字"), dataIndex: "keyword", sorter: (a, b) => a.keyword.localeCompare(b.keyword), render: (v) => <span className="mono">{v}</span> },
            { title: t("等级"), dataIndex: "sort", width: 90, sorter: (a, b) => a.sort - b.sort },
            { title: t("角色状态"), width: 110, render: (_, r) => <Tag color={r.status === 1 ? "success" : "default"}>{r.status === 1 ? t("正常") : t("停用")}</Tag> },
            { title: t("创建人"), dataIndex: "creator", width: 120, render: (v) => v || "—" },
            { title: t("说明"), dataIndex: "remark", ellipsis: true },
            { title: t("操作"), width: 220, fixed: "right", render: (_, r) => (
              <Space size={4}>
                {can("role", "update") && <Button type="link" size="small" icon={<EditOutlined />} onClick={() => openEdit(r)}>{t("编辑")}</Button>}
                <Button type="link" size="small" icon={<KeyOutlined />} onClick={() => openPerms(r)}>{t("权限")}</Button>
                {can("role", "delete") && <Button type="link" size="small" danger icon={<DeleteOutlined />} onClick={() => remove(r)}>{t("删除")}</Button>}
              </Space>
            ) },
          ]} />
      </Card>

      <Modal open={edit.open} title={edit.id ? t("编辑角色") : t("创建角色")} width={460} onCancel={() => setEdit({ open: false })} onOk={save} okText={t("保存")} cancelText={t("取消")} forceRender>
        <Form form={form} layout="vertical">
          <Form.Item label={t("名称")} name="name" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item label={t("关键字")} name="keyword" rules={[{ required: true }]}><Input disabled={!!edit.id} className="mono" /></Form.Item>
          <Form.Item label={t("说明")} name="remark"><Input /></Form.Item>
          <Form.Item label={t("排序")} name="sort"><InputNumber min={0} /></Form.Item>
          {!!edit.id && <Form.Item label={t("启用")} name="status" getValueProps={(v) => ({ checked: v === 1 })} normalize={(v) => (v ? 1 : 2)}><Switch /></Form.Item>}
        </Form>
      </Modal>

      <Drawer open={perms.open} title={t("{0} 的权限", perms.role?.name || "")} size={600} onClose={() => setPerms({ ...perms, open: false })}
        extra={can("role", "update") && <Button type="primary" onClick={savePerms}>{t("保存权限")}</Button>}>
        <Typography.Paragraph type="secondary">{t("权限按「对象 × 操作」授予。系统会阻止你授出自己没有的权限。")}</Typography.Paragraph>
        <Table rowKey="object" size="small" pagination={false} dataSource={catalog}
          columns={[
            { title: t("对象"), dataIndex: "object", render: (v) => <span className="mono">{v}</span> },
            ...actions.map((a) => ({
              title: actionLabel[a], width: 70, align: "center",
              render: (_, r) => r.actions.includes(a) && (
                <Checkbox checked={!!perms.set[`${r.object}:${a}`]} disabled={!can("role", "update")}
                  onChange={(e) => setPerms((p) => ({ ...p, set: { ...p.set, [`${r.object}:${a}`]: e.target.checked } }))} />
              ),
            })),
          ]} />
      </Drawer>
    </>
  );
}
