import { useEffect, useState } from "react";
import { Alert, App, Badge, Button, Card, Form, Input, Modal, Radio, Space, Table, Tag, Typography } from "antd";
import { PlusOutlined, ReloadOutlined, UploadOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import SecretModal from "../../components/SecretModal.jsx";
import { get, post, errorText } from "../../api/client.js";
import { can } from "../../session.js";
import { time } from "../../format.js";
import { t } from "../../i18n.js";

export default function Pki() {
  const { message } = App.useApp();
  const [cas, setCas] = useState([]);
  const [loading, setLoading] = useState(false);
  const [create, setCreate] = useState(false);
  const [view, setView] = useState(null);
  const [revokeCa, setRevokeCa] = useState(null);
  const [secret, setSecret] = useState({ open: false });
  const [form] = Form.useForm();
  const [revokeForm] = Form.useForm();
  const kind = Form.useWatch("kind", form);
  const mode = Form.useWatch("mode", form);

  const load = async () => {
    setLoading(true);
    try {
      setCas(await get("/api/v2/pki/cas"));
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

  const openCreate = (mode) => {
    form.setFieldsValue({ name: "", kind: "ssh", mode, publicKey: "", certificatePem: "", privateKeyPem: "" });
    setCreate(true);
  };
  const save = async () => {
    const f = await form.validateFields();
    const body = { name: f.name, kind: f.kind };
    if (f.mode === "import") Object.assign(body, f.kind === "ssh" ? { publicKey: f.publicKey } : { certificatePem: f.certificatePem }, { privateKeyPem: f.privateKeyPem });
    try {
      const out = await post("/api/v2/pki/cas", body);
      setCreate(false);
      if (out?.privateKeyPem) setSecret({ open: true, title: t("CA 私钥（离线备份，只显示一次）"), text: out.privateKeyPem, filename: `${f.name}-ca-key.pem` });
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };
  const revoke = async () => {
    const { serial } = await revokeForm.validateFields();
    try {
      await post(`/api/v2/pki/cas/${revokeCa.id}/revocations`, { serial: serial.toLowerCase(), reason: 0 });
      message.success(t("已撤销"));
      setRevokeCa(null);
      load();
    } catch (e) {
      message.error(errorText(e));
    }
  };

  return (
    <>
      <PageHead title={t("证书与信任")}>
        <Button icon={<ReloadOutlined />} onClick={load}>{t("刷新")}</Button>
        {can("pki", "create") && <Button icon={<UploadOutlined />} onClick={() => openCreate("import")}>{t("导入签发机构")}</Button>}
        {can("pki", "create") && <Button type="primary" icon={<PlusOutlined />} onClick={() => openCreate("generate")}>{t("创建签发机构")}</Button>}
      </PageHead>
      <Card title={<Space>{t("签发机构")}<Typography.Text type="secondary">{cas.length}</Typography.Text></Space>}
        extra={can("pki", "delete") && <Button onClick={() => { revokeForm.resetFields(); setRevokeCa(cas[0] || null); }} disabled={!cas.length}>{t("撤销证书")}</Button>}>
        <Alert type="info" showIcon style={{ marginBottom: 16 }} title={t("SSH CA 的公钥会通过 slave 发布到集群的 TrustedUserCAKeys；禁用或删除正在使用的 SSH CA 会让其签发的证书无法登录。用户证书在用户管理中签发。")} />
        <Table rowKey="id" loading={loading} dataSource={cas} pagination={false} scroll={{ x: 800 }}
          columns={[
            { title: "ID", dataIndex: "id", width: 70 },
            { title: t("名称"), dataIndex: "name" },
            { title: t("证书类型"), dataIndex: "kind", width: 120, render: (v) => <Tag>{v === "ssh" ? "OpenSSH" : "X.509"}</Tag> },
            { title: t("启用状态"), width: 110, render: (_, r) => <Badge status={r.enabled ? "success" : "default"} text={r.enabled ? t("启用") : t("停用")} /> },
            { title: t("撤销版本"), dataIndex: "crlNumber", width: 100 },
            { title: t("创建时间"), width: 180, render: (_, r) => time(r.createdAt) },
            { title: t("操作"), width: 170, fixed: "right", render: (_, r) => (
              <Space size={4}>
                <Button type="link" size="small" onClick={() => setView(r)}>{t("查看")}</Button>
                {can("pki", "delete") && <Button type="link" size="small" danger onClick={() => { revokeForm.resetFields(); setRevokeCa(r); }}>{t("撤销证书")}</Button>}
              </Space>
            ) },
          ]} />
      </Card>

      <Modal open={create} title={mode === "import" ? t("导入签发机构") : t("创建签发机构")} width={600} onCancel={() => setCreate(false)} onOk={save} okText={t("确定")} cancelText={t("取消")} forceRender>
        <Form form={form} layout="vertical">
          <Form.Item label={t("名称")} name="name" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item label={t("类型")} name="kind"><Radio.Group optionType="button" options={[{ value: "ssh", label: "SSH" }, { value: "x509", label: "X.509" }]} /></Form.Item>
          <Form.Item label={t("来源")} name="mode"><Radio.Group optionType="button" options={[{ value: "generate", label: t("生成新密钥") }, { value: "import", label: t("导入") }]} /></Form.Item>
          {mode === "import" && (
            <>
              {kind === "ssh"
                ? <Form.Item label={t("公钥")} name="publicKey" rules={[{ required: true }]}><Input.TextArea rows={2} className="mono" /></Form.Item>
                : <Form.Item label={t("证书 PEM")} name="certificatePem" rules={[{ required: true }]}><Input.TextArea rows={4} className="mono" /></Form.Item>}
              <Form.Item label={t("私钥 PEM")} name="privateKeyPem" rules={[{ required: true }]}><Input.TextArea rows={4} className="mono" /></Form.Item>
            </>
          )}
        </Form>
      </Modal>
      <Modal open={!!view} title={view?.name} width={680} footer={null} onCancel={() => setView(null)}>
        <pre className="secret mono">{view?.certificatePem}</pre>
      </Modal>
      <Modal open={!!revokeCa} title={t("撤销证书 · {0}", revokeCa?.name || "")} onCancel={() => setRevokeCa(null)} onOk={revoke} okText={t("撤销")} okButtonProps={{ danger: true }} cancelText={t("取消")} forceRender>
        <Form form={revokeForm} layout="vertical">
          <Form.Item label={t("证书序列号（十六进制）")} name="serial" rules={[{ required: true, pattern: /^[0-9a-fA-F]{1,40}$/, message: t("十六进制序列号") }]}><Input className="mono" /></Form.Item>
        </Form>
      </Modal>
      <SecretModal {...secret} onClose={() => setSecret({ open: false })} />
    </>
  );
}
