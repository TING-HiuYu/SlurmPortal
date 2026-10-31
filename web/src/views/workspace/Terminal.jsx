import { useEffect, useRef, useState } from "react";
import { Alert, Button, Card, Empty, Flex, Select, Tag, Typography } from "antd";
import { ApiOutlined, DisconnectOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import XTerm from "../../components/XTerm.jsx";
import { GatewaySession, authorizationExpired } from "../../gateway.js";
import { gatewayError, useWebAccessClusters } from "../../hooks.js";
import { getState } from "../../session.js";
import { t } from "../../i18n.js";

const statusText = { disconnected: t("未连接"), connecting: t("连接中"), connected: t("已连接"), error: t("连接错误") };
const statusColor = { connected: "success", error: "error", connecting: "processing" };

export default function Terminal() {
  const access = useWebAccessClusters();
  const { capabilities, loaded, error: loadError } = access;
  const clusters = access.clusters.filter((c) => c.features?.terminal);
  const [clusterId, setClusterId] = useState("");
  const [status, setStatus] = useState("disconnected");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const term = useRef(null);
  const conn = useRef({ session: null, socket: null, attempt: 0 });
  const cluster = clusters.find((c) => c.id === clusterId);

  useEffect(() => {
    if (!clusterId && clusters[0]) setClusterId(clusters[0].id);
  }, [clusters, clusterId]);

  const disconnect = () => {
    const c = conn.current;
    c.attempt++;
    c.socket?.close();
    c.session?.close();
    c.socket = c.session = null;
    setBusy(false);
    setStatus("disconnected");
  };
  useEffect(() => disconnect, []);

  const connect = async () => {
    disconnect();
    const c = conn.current;
    const mine = ++c.attempt;
    setBusy(true);
    setStatus("connecting");
    setError("");
    const s = new GatewaySession(cluster, getState().user.username, "terminal");
    try {
      await s.open();
      const { cols, rows } = term.current.size();
      const ws = await s.terminal(cols, rows);
      if (mine !== c.attempt) {
        ws.close();
        return s.close();
      }
      c.session = s;
      c.socket = ws;
      ws.addEventListener("open", () => {
        if (mine !== c.attempt) return;
        setBusy(false);
        setStatus("connected");
        term.current?.focus();
      });
      ws.addEventListener("message", (ev) => mine === c.attempt && term.current?.write(ev.data));
      ws.addEventListener("close", (ev) => {
        if (mine !== c.attempt) return;
        setBusy(false);
        setStatus(ev.code === 1000 ? "disconnected" : "error");
        if (ev.code !== 1000) setError(authorizationExpired(ev) ? t("授权已过期，请重新连接") : t("连接已关闭"));
      });
    } catch (e) {
      await s.close();
      if (mine !== c.attempt) return;
      setError(gatewayError(e));
      setStatus("error");
      setBusy(false);
    }
  };

  const send = (data) => conn.current.socket?.readyState === WebSocket.OPEN && conn.current.socket.send(data);
  const resize = (size) => conn.current.socket?.readyState === WebSocket.OPEN && conn.current.socket.send(JSON.stringify({ type: "resize", ...size }));

  return (
    <>
      <PageHead title={t("网页终端")} />
      <Card className="surface" styles={{ body: { padding: "18px 22px" } }}>
        <Flex justify="space-between" align="center" gap={12} wrap>
          <Flex align="center" gap={12}>
            <Typography.Text strong>{t("集群")}</Typography.Text>
            <Select style={{ width: 260 }} placeholder={t("请选择")} disabled={busy} value={clusterId || undefined} onChange={(v) => { disconnect(); setClusterId(v); }}
              options={clusters.map((c) => ({ value: c.id, label: c.name }))} />
          </Flex>
          <Flex align="center" gap={10}>
            <Tag color={statusColor[status] || "default"}>{statusText[status]}</Tag>
            {status !== "connected"
              ? <Button type="primary" icon={<ApiOutlined />} loading={busy} disabled={!capabilities.terminalEnabled || !cluster} onClick={connect}>{t("连接")}</Button>
              : <Button icon={<DisconnectOutlined />} onClick={disconnect}>{t("断开")}</Button>}
          </Flex>
        </Flex>
      </Card>
      {(error || loadError) && <Alert type="error" showIcon title={error || loadError} style={{ marginBottom: 12 }} />}
      {loaded && !capabilities.terminalEnabled ? <Card><Empty description={t("网页终端尚未启用")} /></Card>
        : loaded && !clusters.length ? <Card><Empty description={t("你没有可用的集群")} /></Card>
        : (
          <Card styles={{ body: { padding: 12 } }}>
            <XTerm ref={term} label={t("Web 终端")} onInput={send} onResize={resize} />
            <Typography.Paragraph type="secondary" style={{ margin: "10px 0 0", fontSize: 12 }}>
              {status === "connected" ? `${getState().user?.username} @ ${cluster?.name} · ` : ""}{t("会话使用 5 分钟有效的临时证书建立；连接期间自动续期。")}
            </Typography.Paragraph>
          </Card>
        )}
    </>
  );
}
