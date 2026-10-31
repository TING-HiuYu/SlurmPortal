import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { Badge, Button, Card, Descriptions, Empty, Flex, Space, Switch, Table, Tag, Typography } from "antd";
import { CloseOutlined, MinusOutlined, PlusOutlined, ReloadOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get } from "../../api/client.js";
import { usePoll } from "../../hooks.js";
import { besideRect } from "../../placement.js";
import { antColor, checkStatus, label, tag, time, ago } from "../../format.js";
import "./Diagnostics.css";
import { t } from "../../i18n.js";

const statusText = { ok: t("正常"), stale: t("过期"), error: t("错误"), missing: t("无数据"), disabled: t("未启用") };
const badge = { ok: "success", stale: "warning", error: "error", missing: "default", disabled: "default" };
const detail = {
  read_ok: t("数据库读取正常"), read_failed: t("读取失败"), never_reported: t("从未收到该集群上报"), report_stale: t("上报已过期，slave 可能离线"),
  slave_collection_failed: t("slave 侧采集失败"), report_fresh: t("上报正常"), registration_fresh: t("网关注册正常"), registration_stale: t("网关注册已过期"),
  not_configured: t("未配置"), notification_disabled: t("作业通知未启用"), password_missing: t("已启用但未设置密码"), configured_not_probed: t("已配置（未做在线探测）"),
  smtp_configured_not_probed: t("SMTP 已配置（未在线探测）"), "mail-pool_configured_not_probed": t("邮件池已配置"), provider_disabled: t("服务商已停用"),
  notification_provider_unavailable: t("作业通知的服务商不存在或已停用"), notification_provider_ready: t("作业通知的服务商可用"),
  no_recent_failures: t("近 24 小时无投递失败"), recent_failures: t("近 24 小时有投递失败"), sends_awaiting_retry: t("有发送卡在重试中"),
};
const detailText = (d) => detail[d] || d;
function componentText(c) {
  const fixed = { database: t("数据库"), "cluster-status": t("节点状态上报"), "resource-status": t("资源与队列上报"), "mail-delivery": t("邮件投递") }[c.component];
  if (fixed) return fixed;
  if (c.component === "gateway") return t("网关 {0}", c.id.split(":")[1] || "");
  if (c.component === "mail") return c.id === "master:mail" ? t("作业通知") : t("服务商 {0}", c.id.slice("master:mail:".length));
  return c.component;
}
const clip = (s, n) => (Array.from(s || "").length > n ? Array.from(s).slice(0, n - 1).join("") + "…" : s);

// Layout: the mail zone on the left, the master zone in the middle and one zone per cluster on the
// right (two columns once there are more than three). Zone sizes derive from their node counts and
// every zone is centred vertically, so the picture stays balanced and nothing overlaps.
const NODE = { w: 210, h: 58 };
const PAD = 18, HEAD = 40, GAP = 14, COL = 150, ZGAP = 24, ZCOL = 40;
const rank = { error: 4, stale: 3, missing: 2, disabled: 1, ok: 0 };
const worst = (list) => list.reduce((w, s) => (rank[s] > rank[w] ? s : w), "ok");
const zoneH = (n) => HEAD + PAD + Math.max(n, 1) * NODE.h + Math.max(n - 1, 0) * GAP + PAD;
const zoneW = NODE.w + PAD * 2;

function buildLayout(data) {
  const checks = data?.checks || [];
  if (!checks.length) return { nodes: [], edges: [], zones: [], width: 0, height: 0 };
  const clusterName = (id) => data.clusters?.find((c) => c.id === id)?.name || id;
  const node = (id, title, check, status) => ({ id, title, check, status: status || check?.status || "missing", sub: check ? detailText(check.detail) : "" });
  const db = checks.find((c) => c.id === "master:database");
  const master = [{ ...node("master", t("主服务 API"), null, "ok"), sub: "API" }, ...(db ? [node(db.id, t("数据库"), db)] : [])];
  const mail = checks.filter((c) => c.component === "mail" || c.component === "mail-delivery").map((c) => node(c.id, componentText(c), c));
  const ids = [...new Set([...(data.clusters || []).map((c) => c.id), ...checks.filter((c) => c.clusterId).map((c) => c.clusterId)])];
  const clusters = ids.map((id) => ({ id, nodes: checks.filter((c) => c.clusterId === id).map((c) => node(c.id, componentText(c), c)) }));
  const orphan = checks.filter((c) => c.component === "gateway" && !c.clusterId);
  if (orphan.length) clusters.push({ id: "", nodes: orphan.map((c) => node(c.id, componentText(c), c)) });

  // Cluster zones fill the shorter of the right-hand columns first.
  const cols = clusters.length > 3 ? 2 : 1;
  const colH = Array(cols).fill(0);
  const placed = clusters.map((c) => {
    const col = colH.indexOf(Math.min(...colH));
    const y = colH[col];
    colH[col] += zoneH(c.nodes.length) + ZGAP;
    return { ...c, col, y };
  });
  const rightH = Math.max(...colH) - ZGAP;
  const height = Math.max(rightH, zoneH(master.length), zoneH(mail.length)) + 20;
  const xMail = 10, xMaster = mail.length ? xMail + zoneW + COL : 10, xRight = xMaster + zoneW + COL;
  const width = (clusters.length ? xRight + cols * zoneW + (cols - 1) * ZCOL : xMaster + zoneW) + 10;

  const zones = [], nodes = [];
  const putZone = (id, kind, title, sub, x, y, list) => {
    zones.push({ id, kind, title, sub, x, y, w: zoneW, h: zoneH(list.length) });
    list.forEach((n, k) => nodes.push({ ...n, x: x + PAD, y: y + HEAD + PAD + k * (NODE.h + GAP) }));
  };
  const centred = (h) => (height - h) / 2;
  putZone("z-master", "master", t("主服务"), "Slurm Portal", xMaster, centred(zoneH(master.length)), master);
  if (mail.length) putZone("z-mail", "mail", t("邮件"), t("{0} 项", mail.length), xMail, centred(zoneH(mail.length)), mail);
  const top = centred(rightH);
  for (const c of placed) {
    const w = worst(c.nodes.map((n) => n.status));
    putZone(`z-${c.id}`, "cluster", c.id ? clusterName(c.id) : t("未归属网关"), w === "ok" ? t("正常") : statusText[w], xRight + c.col * (zoneW + ZCOL), top + c.y, c.nodes);
  }

  const at = Object.fromEntries(nodes.map((n) => [n.id, n]));
  const edges = [];
  // side: "in" = a is right of b (cluster → master), "out" = a is left of b (mail → master), "loop" = same zone.
  const link = (from, to, status, side) => {
    const a = at[from], b = at[to];
    if (!a || !b) return;
    let d;
    if (side === "loop") {
      const x = a.x + NODE.w, y1 = a.y + NODE.h / 2, y2 = b.y + NODE.h / 2;
      d = `M${x},${y1} C${x + 36},${y1} ${x + 36},${y2} ${x + 2},${y2}`;
    } else {
      const sx = side === "in" ? a.x : a.x + NODE.w, tx = side === "in" ? b.x + NODE.w + 2 : b.x - 2;
      const sy = a.y + NODE.h / 2, ty = b.y + NODE.h / 2, mx = (sx + tx) / 2;
      d = `M${sx},${sy} C${mx},${sy} ${mx},${ty} ${tx},${ty}`;
    }
    edges.push({ id: `${from}>${to}`, ids: [from, to], d, status });
  };
  if (at["master:database"]) link("master", "master:database", at["master:database"].status, "loop");
  for (const c of clusters) for (const n of c.nodes) link(n.id, "master", n.status, "in");
  for (const n of mail) {
    if (n.id !== "master:mail" && at["master:mail"]) link(n.id, "master:mail", n.status, "loop");
    else link(n.id, "master", n.status, "out");
  }
  return { nodes, edges, zones, width, height };
}

const MIN_H = 360, CANVAS_PAD = 28;

export default function Diagnostics() {
  const { data, loading, refresh, paused, setPaused } = usePoll(() => get("/api/v2/diagnostics"), 30000);
  const [zoom, setZoom] = useState(1);
  const [focus, setFocus] = useState("");
  const [canvasW, setCanvasW] = useState(0);
  const [panelH, setPanelH] = useState(220);
  const canvas = useRef(null);
  const panel = useRef(null);
  const layout = useMemo(() => buildLayout(data), [data]);
  const counts = useMemo(() => (data?.checks || []).reduce((a, c) => ((a[c.status] = (a[c.status] || 0) + 1), a), {}), [data]);
  const linked = useMemo(() => new Set(layout.edges.filter((e) => e.ids.includes(focus)).flatMap((e) => e.ids)), [layout, focus]);
  const selected = layout.nodes.find((n) => n.id === focus);
  const clusterName = (id) => data?.clusters?.find((c) => c.id === id)?.name || id;

  useEffect(() => {
    const o = new ResizeObserver(([e]) => setCanvasW(Math.floor(e.contentRect.width)));
    o.observe(canvas.current);
    return () => o.disconnect();
  }, []);
  useLayoutEffect(() => {
    if (panel.current) setPanelH(panel.current.offsetHeight);
  }, [selected]);
  // Start fitted to the canvas width (never above 100%); the zoom buttons take over from there.
  const fitted = useRef(false);
  useEffect(() => {
    if (fitted.current || !layout.width || !canvasW) return;
    fitted.current = true;
    setZoom(Math.max(0.5, Math.min(1, Math.floor((canvasW / layout.width) * 10) / 10)));
  }, [layout.width, canvasW]);

  // The stage is at least as large as the visible canvas; the graph is centred on it and the detail
  // panel is placed on the stage beside the selected node so it never covers it.
  const svgW = layout.width * zoom, svgH = layout.height * zoom;
  const stage = { width: Math.max(svgW, canvasW), height: Math.max(svgH, MIN_H) };
  const ox = (stage.width - svgW) / 2, oy = (stage.height - svgH) / 2;
  const PANEL_W = 290;
  const rectOf = (n) => ({ x: ox + n.x * zoom, y: oy + n.y * zoom, width: NODE.w * zoom, height: NODE.h * zoom });
  const place = selected && besideRect(rectOf(selected), { width: PANEL_W, height: panelH }, stage, 16, 12,
    layout.nodes.filter((n) => n.id !== selected.id).map(rectOf));

  return (
    <>
      <PageHead title={t("服务拓扑与诊断")}>
        <Switch checked={!paused} onChange={(v) => setPaused(!v)} checkedChildren={t("自动刷新")} unCheckedChildren={t("自动刷新")} />
        <Button type="primary" icon={<ReloadOutlined />} loading={loading} onClick={refresh}>{t("立即检查")}</Button>
      </PageHead>

      <Card className="surface" styles={{ body: { padding: 0 } }}
        title={<Space size={18} wrap>{Object.entries(statusText).map(([k, v]) => <Badge key={k} status={badge[k]} text={<>{v} <b>{counts[k] || 0}</b></>} />)}</Space>}
        extra={
          <Space wrap>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>{layout.nodes.length} {t("个节点 ·")} {layout.edges.length} {t("条链路 · 检查于")} {time(data?.checkedAt)}</Typography.Text>
            <Space.Compact>
              <Button icon={<MinusOutlined />} onClick={() => setZoom((z) => Math.max(0.5, +(z - 0.1).toFixed(1)))} />
              <Button style={{ minWidth: 64 }} onClick={() => setZoom(1)}>{Math.round(zoom * 100)}%</Button>
              <Button icon={<PlusOutlined />} onClick={() => setZoom((z) => Math.min(2, +(z + 0.1).toFixed(1)))} />
            </Space.Compact>
          </Space>
        }>
        <div ref={canvas} className="diag-canvas" onClick={(e) => e.target === e.currentTarget && setFocus("")}>
          {layout.nodes.length ? (
            <div className="diag-stage" style={{ width: stage.width, height: stage.height }} onClick={(e) => e.target === e.currentTarget && setFocus("")}>
              <svg viewBox={`0 0 ${layout.width} ${layout.height}`} width={svgW} height={svgH} style={{ position: "absolute", left: ox, top: oy }} role="img" aria-label={t("服务拓扑")}
                onClick={(e) => e.target.tagName === "svg" && setFocus("")}>
                <defs>
                  {Object.keys(statusText).map((k) => (
                    <marker key={k} id={`arrow-${k}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
                      <path d="M0,0 L10,5 L0,10 z" className={`fill-${k}`} />
                    </marker>
                  ))}
                </defs>
                {layout.zones.map((z) => (
                  <g key={z.id}>
                    <rect x={z.x} y={z.y} width={z.w} height={z.h} rx="14" className={`zone ${z.kind}`} />
                    <text x={z.x + 16} y={z.y + 24} className="zone-title">{z.title}</text>
                    <text x={z.x + z.w - 16} y={z.y + 24} className="zone-sub" textAnchor="end">{z.sub}</text>
                  </g>
                ))}
                {layout.edges.map((e) => <path key={e.id} d={e.d} className={`edge ${e.status} ${focus && !e.ids.includes(focus) ? "dim" : ""}`} markerEnd={`url(#arrow-${e.status})`} />)}
                {layout.nodes.map((n) => (
                  <g key={n.id} className={`node ${n.status} ${focus === n.id ? "active" : ""} ${focus && focus !== n.id && !linked.has(n.id) ? "dim" : ""}`}
                    transform={`translate(${n.x},${n.y})`} tabIndex={0} role="button" aria-label={`${n.title}：${statusText[n.status]}`}
                    onClick={() => setFocus(focus === n.id ? "" : n.id)} onKeyDown={(e) => e.key === "Enter" && setFocus(n.id)}>
                    <rect width={NODE.w} height={NODE.h} rx="10" className="node-box" />
                    <rect width="4" height={NODE.h - 20} x="0" y="10" rx="2" className="node-bar" />
                    <circle cx={NODE.w - 16} cy="18" r="5" className="node-dot" />
                    <text x="16" y="24" className="node-title">{n.title}</text>
                    <text x="16" y="44" className="node-sub">{clip(n.sub, 26)}</text>
                  </g>
                ))}
              </svg>
              {selected && (
                <Card ref={panel} size="small" className="diag-panel" style={{ width: PANEL_W, left: place.left, top: place.top }} data-side={place.side}
                  title={selected.title} extra={<Button type="text" size="small" icon={<CloseOutlined />} aria-label={t("关闭")} onClick={() => setFocus("")} />}>
                  <Tag color={antColor(tag(checkStatus, selected.status))}>{label(checkStatus, selected.status)}</Tag>
                  <Descriptions column={1} size="small" layout="vertical" colon={false} style={{ marginTop: 10 }}
                    items={[
                      { label: t("说明"), children: selected.sub || "—" },
                      ...(selected.check ? [{ label: t("检查项"), children: <span className="mono">{selected.check.id}</span> }] : []),
                      ...(selected.check?.observedAt ? [{ label: t("最近观测"), children: `${time(selected.check.observedAt)}（${ago(selected.check.observedAt)}）` }] : []),
                      ...(selected.check?.latencyMs !== undefined && selected.check ? [{ label: t("耗时"), children: `${selected.check.latencyMs} ms` }] : []),
                    ]} />
                </Card>
              )}
            </div>
          ) : !loading && <Empty description={t("暂无诊断数据")} style={{ padding: 60 }} />}
        </div>
      </Card>

      <Card className="surface" title={t("检查明细")} extra={<span className="muted">{t("超过")} {data?.staleAfterSeconds || 180} {t("秒的上报视为过期")}</span>}>
        <Table rowKey="id" dataSource={data?.checks || []} pagination={false} size="middle" scroll={{ x: 760 }} onRow={(r) => ({ onClick: () => setFocus(r.id), style: { cursor: "pointer" } })}
          columns={[
            { title: t("状态"), width: 90, render: (_, r) => <Tag color={antColor(tag(checkStatus, r.status))}>{label(checkStatus, r.status)}</Tag> },
            { title: t("组件"), width: 170, render: (_, r) => componentText(r) },
            { title: t("集群"), width: 120, render: (_, r) => clusterName(r.clusterId) || "—" },
            { title: t("说明"), render: (_, r) => detailText(r.detail) },
            { title: t("最近观测"), width: 140, render: (_, r) => (r.observedAt ? ago(r.observedAt) : r.latencyMs !== undefined ? `${r.latencyMs} ms` : "—") },
          ]} />
      </Card>
    </>
  );
}

