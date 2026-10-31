import { useMemo, useState } from "react";
import { Button, Card, Flex, Input, Progress, Select, Switch, Table, Tag, Typography } from "antd";
import { ReloadOutlined, SearchOutlined, UserOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get } from "../../api/client.js";
import { usePoll } from "../../hooks.js";
import { queueFromR2 } from "../../currentstatus.js";
import { getState } from "../../session.js";
import { duration, percent, time } from "../../format.js";
import { t } from "../../i18n.js";

const kinds = [
  { key: "cpu", title: "CPU", badge: "CPU", unit: t("核"), color: "var(--console-accent)", tag: "blue", fmt: (v) => v.toLocaleString() },
  { key: "memory_mib", title: t("内存"), badge: "MEM", unit: "GiB", color: "var(--console-violet)", tag: "purple", fmt: (v) => Math.round(v / 1024).toLocaleString() },
  { key: "gpu", title: "GPU", badge: "GPU", unit: t("张"), color: "var(--console-teal)", tag: "cyan", fmt: (v) => v.toLocaleString() },
];
const stateColor = (s) => ({ RUNNING: "processing", PENDING: "warning", COMPLETING: "success", FAILED: "error" })[s] || "default";

export default function Queue() {
  const { data, loading, refresh, paused, setPaused } = usePoll(async () => (await queueFromR2().catch(() => null)) || get("/api/v2/resource-status"), 30000);
  const [selected, setSelected] = useState([]);
  const [f, setF] = useState({ state: undefined, user: undefined, mine: false, q: "" });
  const all = data?.clusters || [];
  const clusters = selected.length ? all.filter((c) => selected.includes(c.id)) : all;
  const totals = useMemo(() => Object.fromEntries(kinds.map((k) => {
    const r = clusters.map((c) => c.resources?.[k.key]).filter(Boolean);
    return [k.key, { available: r.reduce((a, x) => a + (x.available || 0), 0), total: r.reduce((a, x) => a + (x.total || 0), 0), clusters: r.length }];
  })), [clusters]);
  const allJobs = useMemo(() => clusters.flatMap((c) => (c.jobs || []).map((j) => ({ ...j, cluster: c.name, key: `${c.id}/${j.id}` }))), [clusters]);
  const me = getState().user?.username;
  const q = f.q.trim().toLowerCase();
  const jobs = allJobs.filter((j) => (!f.state || j.state === f.state) && (!f.user || j.user === f.user) && (!f.mine || j.user === me) &&
    (!q || [j.name, j.partition].some((v) => (v || "").toLowerCase().includes(q))));
  const uniq = (k) => [...new Set(allJobs.map((j) => j[k]))].sort().map((v) => ({ value: v, label: v }));

  return (
    <>
      <PageHead title={t("当前资源概览")}>
        <Switch checked={!paused} onChange={(v) => setPaused(!v)} checkedChildren={t("自动刷新")} unCheckedChildren={t("自动刷新")} />
        <Button icon={<ReloadOutlined />} loading={loading} onClick={refresh}>{t("刷新")}</Button>
      </PageHead>
      <Card className="surface" styles={{ body: { padding: "18px 22px" } }}>
        <Flex justify="space-between" align="center" gap={16} wrap>
          <Flex align="center" gap={12}>
            <Typography.Text strong>{t("集群")}</Typography.Text>
            <Select mode="multiple" allowClear maxTagCount="responsive" placeholder={t("全部集群")} style={{ width: 320 }} value={selected} onChange={setSelected}
              options={all.map((c) => ({ value: c.id, label: c.name }))} />
          </Flex>
          <Typography.Text type="secondary" style={{ fontSize: 12, textAlign: "right" }}>{t("已选")} {clusters.length} {t("个集群")}<br />{t("主服务更新于")} {time(data?.updatedAt)}</Typography.Text>
        </Flex>
      </Card>

      <Flex justify="space-between" align="baseline" style={{ margin: "6px 0 14px" }}>
        <h3 className="section-title">{t("空闲资源")}</h3><Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("空闲 / 总量")}</Typography.Text>
      </Flex>
      <div className="stat-row">
        {kinds.map((k) => (
          <Card key={k.key}>
            <Flex justify="space-between" align="center"><Typography.Text type="secondary">{k.title}</Typography.Text><Tag color={k.tag} bordered={false}>{k.badge}</Tag></Flex>
            <div style={{ margin: "14px 0 10px" }}>
              <strong style={{ fontSize: 34, fontVariantNumeric: "tabular-nums" }}>{k.fmt(totals[k.key].available)}</strong>
              <span style={{ fontSize: 22, color: "var(--console-muted)" }}> / {k.fmt(totals[k.key].total)}</span>
              <small className="muted" style={{ marginLeft: 8 }}>{k.unit}</small>
            </div>
            <Progress percent={percent(totals[k.key].available, totals[k.key].total)} showInfo={false} strokeColor={k.color} size="small" />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("当前有效：")}{totals[k.key].clusters} / {clusters.length} {t("个集群")}</Typography.Text>
          </Card>
        ))}
      </div>

      <Card className="surface" title={t("当前作业队列")} extra={<Typography.Text type="secondary">{jobs.length} {t("个作业")}</Typography.Text>}>
        <Flex wrap gap={12} align="center" justify="space-between" style={{ marginBottom: 18 }}>
          <Flex wrap gap={12} align="center">
            <Select allowClear placeholder={t("全部状态")} style={{ width: 180 }} value={f.state} onChange={(v) => setF({ ...f, state: v })} options={uniq("state")} />
            <Select allowClear showSearch placeholder={t("全部用户")} style={{ width: 180 }} value={f.user} onChange={(v) => setF({ ...f, user: v })} options={uniq("user")} />
            <Button type={f.mine ? "primary" : "default"} icon={<UserOutlined />} aria-pressed={f.mine} onClick={() => setF({ ...f, mine: !f.mine })}>{t("只看我的")}</Button>
          </Flex>
          <Input allowClear prefix={<SearchOutlined />} placeholder={t("作业名 / 分区")} style={{ width: 240 }} value={f.q} onChange={(e) => setF({ ...f, q: e.target.value })} />
        </Flex>
        <Table rowKey="key" dataSource={jobs} size="middle" scroll={{ x: 1100, y: 560 }} pagination={{ pageSize: 50, hideOnSinglePage: true }} locale={{ emptyText: t("没有作业") }}
          columns={[
            { title: t("作业 ID"), dataIndex: "id", width: 110, sorter: (a, b) => Number(a.id) - Number(b.id), defaultSortOrder: "descend", render: (v) => <span className="mono">{v}</span> },
            { title: t("作业状态"), dataIndex: "state", width: 120, render: (v) => <Tag color={stateColor(v)}>{v}</Tag> },
            { title: t("名称"), dataIndex: "name", width: 200, ellipsis: true },
            { title: t("用户"), dataIndex: "user", width: 120 },
            { title: t("集群"), dataIndex: "cluster", width: 110 },
            { title: t("分区"), dataIndex: "partition", width: 100 },
            { title: t("资源"), width: 170, render: (_, r) => (<>CPU {r.allocated?.cpu ?? r.requested?.cpu ?? "—"} · GPU {r.allocated?.gpu ?? r.requested?.gpu ?? 0}<div className="muted">{r.allocated?.cpu != null ? t("已分配资源") : t("申请资源")}</div></>) },
            { title: t("已运行 / 时限"), width: 180, render: (_, r) => `${duration(r.elapsed_seconds)} / ${duration(r.time_limit_seconds)}` },
            { title: t("原因"), dataIndex: "reason", width: 120, ellipsis: true },
          ]} />
        {clusters.filter((c) => c.jobsTruncated).map((c) => <Typography.Text key={c.id} type="secondary" style={{ fontSize: 12, display: "block" }}>{c.name}{t("：仅显示前")} {c.jobs.length} / {c.jobsTotal} {t("个作业")}</Typography.Text>)}
      </Card>
    </>
  );
}
