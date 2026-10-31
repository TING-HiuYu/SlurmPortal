import { useEffect, useRef, useState } from "react";
import { Alert, App, Breadcrumb, Button, Card, Descriptions, Empty, Flex, Input, List, Modal, Select, Space, Table, Tag, Typography } from "antd";
import { ApiOutlined, ArrowUpOutlined, DisconnectOutlined, DownloadOutlined, FileOutlined, FolderOutlined, HomeOutlined, SendOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import XTerm from "../../components/XTerm.jsx";
import { GatewaySession, parentDirectory, formatBytes, transferFinished, sessionUnavailable } from "../../gateway.js";
import { gatewayError, useWebAccessClusters } from "../../hooks.js";
import { getState } from "../../session.js";
import { time } from "../../format.js";
import { t } from "../../i18n.js";

const sortEntries = (list) => list.slice().sort((a, b) => Number(b.is_dir) - Number(a.is_dir) || a.name.localeCompare(b.name));
const statusColor = { completed: "success", failed: "error", partial: "warning" };

export default function Files() {
  const { message } = App.useApp();
  const access = useWebAccessClusters();
  const { capabilities, loaded, error: loadError } = access;
  const clusters = access.clusters.filter((c) => c.features?.files);
  const [clusterId, setClusterId] = useState("");
  const [session, setSession] = useState(null);
  const [busy, setBusy] = useState(false);
  const [loading, setLoading] = useState(false);
  const [downloading, setDownloading] = useState(false);
  const [error, setError] = useState("");
  const [dir, setDir] = useState({ path: "", entries: [], truncated: false });
  const [input, setInput] = useState("");
  const [selected, setSelected] = useState([]);
  const [picker, setPicker] = useState({ open: false, clusterId: undefined, session: null, path: "", dirs: [], loading: false, submitting: false, error: "" });
  const [progress, setProgress] = useState({ open: false, id: "", info: {} });
  const logs = useRef(null);
  const live = useRef({ session: null, picker: null, timer: null, socket: null });
  const cluster = clusters.find((c) => c.id === clusterId);
  const destinations = clusters.filter((c) => c.id !== clusterId && c.features?.transfer);

  useEffect(() => {
    if (!clusterId && clusters[0]) setClusterId(clusters[0].id);
  }, [clusters, clusterId]);
  useEffect(() => () => {
    const l = live.current;
    clearTimeout(l.timer);
    l.socket?.close();
    l.picker?.close();
    l.session?.close();
  }, []);

  const disconnect = () => {
    live.current.session?.close();
    live.current.session = null;
    setSession(null);
    setDir({ path: "", entries: [], truncated: false });
    setSelected([]);
    setInput("");
  };
  const fail = (e) => {
    if (sessionUnavailable(e)) {
      disconnect();
      setError(t("网关会话已失效，请重新连接"));
    } else setError(gatewayError(e));
  };
  const browse = async (p, s = session) => {
    if (!s) return;
    setLoading(true);
    setError("");
    try {
      const d = await s.list(p);
      setDir({ path: d.path, entries: sortEntries(d.entries || []), truncated: d.truncated === true });
      setInput(d.path);
      setSelected([]);
    } catch (e) {
      fail(e);
    } finally {
      setLoading(false);
    }
  };
  const connect = async () => {
    disconnect();
    setBusy(true);
    setError("");
    const s = new GatewaySession(cluster, getState().user.username, "files");
    try {
      await s.open();
      live.current.session = s;
      setSession(s);
      await browse(s.home, s);
    } catch (e) {
      await s.close();
      fail(e);
    } finally {
      setBusy(false);
    }
  };
  const doDownload = async () => {
    setDownloading(true);
    try {
      await session.download(selected.map((e) => e.path));
      message.success(t("下载已开始"));
    } catch (e) {
      fail(e);
    } finally {
      setDownloading(false);
    }
  };

  const openPicker = async (id) => {
    live.current.picker?.close();
    setPicker((p) => ({ ...p, clusterId: id, session: null, path: "", dirs: [], error: "", loading: true }));
    const s = new GatewaySession(destinations.find((c) => c.id === id), getState().user.username, "files");
    try {
      await s.open();
      live.current.picker = s;
      setPicker((p) => ({ ...p, session: s }));
      await pickBrowse(s.home, s);
    } catch (e) {
      await s.close();
      setPicker((p) => ({ ...p, error: gatewayError(e), loading: false }));
    }
  };
  const pickBrowse = async (p, s = picker.session) => {
    setPicker((x) => ({ ...x, loading: true }));
    try {
      const d = await s.list(p);
      setPicker((x) => ({ ...x, path: d.path, dirs: sortEntries((d.entries || []).filter((e) => e.is_dir)), loading: false }));
    } catch (e) {
      setPicker((x) => ({ ...x, error: gatewayError(e), loading: false }));
    }
  };
  const closePicker = () => {
    live.current.picker?.close();
    live.current.picker = null;
    setPicker({ open: false, clusterId: undefined, session: null, path: "", dirs: [], loading: false, submitting: false, error: "" });
  };

  const poll = async (id) => {
    let info = {};
    try {
      info = await live.current.session.transferStatus(id);
      setProgress((p) => (p.id === id ? { ...p, info } : p));
    } catch {
      /* keep the last known state and retry */
    }
    if (!transferFinished(info.status)) live.current.timer = setTimeout(() => poll(id), 3000);
  };
  const startTransfer = async () => {
    setPicker((p) => ({ ...p, submitting: true }));
    try {
      const target = destinations.find((c) => c.id === picker.clusterId);
      const d = await session.transfer(target, selected.map((e) => e.path), picker.path);
      closePicker();
      setProgress({ open: true, id: d.transfer_id, info: {} });
      poll(d.transfer_id);
    } catch (e) {
      setPicker((p) => ({ ...p, error: gatewayError(e), submitting: false }));
    }
  };
  const openLogs = async () => {
    try {
      const ws = await live.current.session.transferLogs(progress.id);
      live.current.socket = ws;
      ws.addEventListener("message", (ev) => logs.current?.write(ev.data));
    } catch {
      logs.current?.write(t("无法连接传输日志\r\n"));
    }
  };
  const stopProgress = () => {
    clearTimeout(live.current.timer);
    live.current.socket?.close();
    live.current.socket = null;
    setProgress({ open: false, id: "", info: {} });
  };

  const crumbs = dir.path ? dir.path.split("/").filter(Boolean) : [];
  return (
    <>
      <PageHead title={t("文件浏览器")} />
      <Card className="surface" styles={{ body: { padding: "18px 22px" } }}>
        <Flex justify="space-between" align="center" gap={12} wrap>
          <Flex align="center" gap={12}>
            <Typography.Text strong>{t("集群")}</Typography.Text>
<Select style={{ width: 260 }} placeholder={t("请选择")} disabled={busy} value={clusterId || undefined} onChange={(v) => { disconnect(); setClusterId(v); }}
          options={clusters.map((c) => ({ value: c.id, label: c.name }))} />
            <Tag color={session ? "success" : "default"}>{session ? t("已连接") : t("未连接")}</Tag>
          </Flex>
          <Flex align="center" gap={10} wrap>
            {!session
          ? <Button type="primary" icon={<ApiOutlined />} loading={busy} disabled={!capabilities.filesEnabled || !cluster} onClick={connect}>{t("连接")}</Button>
          : (
            <>
              <Button icon={<DownloadOutlined />} disabled={!selected.length} loading={downloading} onClick={doDownload}>{t("下载所选 (")}{selected.length})</Button>
              {capabilities.transferEnabled && (
                <Button type="primary" icon={<SendOutlined />} disabled={!selected.length || !destinations.length} onClick={() => setPicker((p) => ({ ...p, open: true }))}>{t("传输到其他集群")}</Button>
              )}
              <Button icon={<DisconnectOutlined />} onClick={disconnect}>{t("断开")}</Button>
            </>
          )}
          </Flex>
        </Flex>
      </Card>
      {(error || loadError) && <Alert type="error" showIcon title={error || loadError} style={{ marginBottom: 12 }} />}
      {loaded && !capabilities.filesEnabled ? <Card><Empty description={t("文件浏览尚未启用")} /></Card>
        : loaded && !clusters.length ? <Card><Empty description={t("你没有可用的集群")} /></Card>
        : (
          <Card>
            <Space.Compact style={{ width: "100%", marginBottom: 12 }}>
              <Button icon={<HomeOutlined />} disabled={!session} onClick={() => browse(session.home)} />
              <Button icon={<ArrowUpOutlined />} disabled={!session || dir.path === session?.fileRoot} onClick={() => browse(parentDirectory(dir.path))} />
              <Input disabled={!session} value={input} onChange={(e) => setInput(e.target.value)} onPressEnter={() => browse(input)} className="mono" />
              <Button disabled={!session} onClick={() => browse(input)}>{t("前往")}</Button>
            </Space.Compact>
            {session && (
              <Breadcrumb style={{ marginBottom: 12 }}
                items={[{ title: <a onClick={() => browse("/")}>/</a> }, ...crumbs.map((c, i) => ({ title: <a onClick={() => browse("/" + crumbs.slice(0, i + 1).join("/"))}>{c}</a> }))]} />
            )}
            <Table rowKey="path" size="middle" loading={loading || busy} dataSource={dir.entries} pagination={false} scroll={{ y: "calc(100vh - 400px)", x: 700 }}
              locale={{ emptyText: session ? t("空目录") : t("连接后浏览文件") }}
              rowSelection={{ selectedRowKeys: selected.map((e) => e.path), onChange: (_, rows) => setSelected(rows) }}
              onRow={(r) => ({ onDoubleClick: () => r.is_dir && browse(r.path) })}
              columns={[
                { title: t("名称"), dataIndex: "name", render: (v, r) => (r.is_dir ? <a onClick={() => browse(r.path)}><FolderOutlined /> {v}</a> : <span><FileOutlined /> {v}</span>) },
                { title: t("大小"), width: 110, render: (_, r) => (r.is_dir ? "—" : formatBytes(r.size)) },
                { title: t("修改时间"), width: 180, render: (_, r) => time(r.modified_at) },
                { title: t("权限"), dataIndex: "mode", width: 120, render: (v) => <span className="mono">{v}</span> },
              ]} />
            {dir.truncated && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("目录内容过多，只显示了一部分。")}</Typography.Text>}
          </Card>
        )}

      <Modal open={picker.open} title={t("传输到其他集群")} width={640} maskClosable={false} onCancel={closePicker}
        okText={t("开始传输")} cancelText={t("取消")} okButtonProps={{ disabled: !picker.path, loading: picker.submitting }} onOk={startTransfer}>
        <Select placeholder={t("目标集群")} style={{ width: "100%", marginBottom: 12 }} value={picker.clusterId} onChange={openPicker}
          options={destinations.map((c) => ({ value: c.id, label: c.name }))} />
        <Space style={{ marginBottom: 8 }}>
          <Button icon={<ArrowUpOutlined />} disabled={!picker.session || picker.path === picker.session?.fileRoot} onClick={() => pickBrowse(parentDirectory(picker.path))} />
          <span className="mono">{picker.path || "—"}</span>
        </Space>
        <List bordered size="small" loading={picker.loading} style={{ minHeight: 160, maxHeight: 300, overflowY: "auto", marginBottom: 8 }}
          locale={{ emptyText: picker.session ? t("没有子目录") : t("选择目标集群") }} dataSource={picker.dirs}
          renderItem={(d) => <List.Item style={{ cursor: "pointer" }} onClick={() => pickBrowse(d.path)}><FolderOutlined />&nbsp;{d.name}</List.Item>} />
        <Typography.Text type="secondary">{selected.length} {t("个项目 →")} {picker.path || "—"}</Typography.Text>
        {picker.error && <Alert type="error" showIcon title={picker.error} style={{ marginTop: 8 }} />}
      </Modal>

      <Modal open={progress.open} title={t("传输进度")} width={900} maskClosable={false} footer={null} onCancel={stopProgress} afterOpenChange={(o) => o && openLogs()} destroyOnHidden>
        <Space style={{ marginBottom: 12 }}>
          <Tag color={statusColor[progress.info.status] || "processing"}>{progress.info.status || "starting"}</Tag>
          <span className="mono muted">{progress.id}</span>
        </Space>
        <Descriptions bordered size="small" column={4} style={{ marginBottom: 12 }}
          items={[
            { label: t("已发送"), children: formatBytes(progress.info.bytes_sent) },
            { label: t("耗时"), children: progress.info.elapsed_seconds == null ? "—" : `${progress.info.elapsed_seconds} s` },
            { label: t("速率"), children: progress.info.average_mb_per_second == null ? "—" : `${Number(progress.info.average_mb_per_second).toFixed(2)} MB/s` },
            { label: t("退出码"), children: progress.info.exit_code ?? "—" },
          ]} />
        <XTerm ref={logs} readonly label={t("传输日志")} />
        <Typography.Paragraph type="secondary" style={{ margin: "10px 0 0", fontSize: 12 }}>{t("关闭此窗口不会中断传输；完成后会发送邮件通知（如已启用）。")}</Typography.Paragraph>
      </Modal>
    </>
  );
}
