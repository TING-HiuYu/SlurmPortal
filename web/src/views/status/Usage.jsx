import { useEffect, useState } from "react";
import { Alert, App, Button, Card, Flex, Select, Spin, Table, Tag } from "antd";
import { DashboardOutlined, FundOutlined, ReloadOutlined, ScheduleOutlined, ThunderboltOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import StatCard from "../../components/StatCard.jsx";
import BarChart from "../../components/BarChart.jsx";
import { get, errorText, ApiError } from "../../api/client.js";
import { t } from "../../i18n.js";

const stateNames = { COMPLETED: t("正常完成"), FAILED: t("失败"), CANCELLED: t("取消"), TIMEOUT: t("超时"), OUT_OF_MEMORY: t("内存超限"), NODE_FAIL: t("节点故障"), PREEMPTED: t("被抢占"), BOOT_FAIL: t("启动失败"), DEADLINE: t("超过截止时间") };
const stateLabel = (s) => (stateNames[s] ? `${stateNames[s]} ${s}` : s);
const sum = (rows, k) => (rows || []).reduce((a, r) => a + (r[k] || 0), 0);
const pct = (v) => (v === null || v === undefined ? "—" : `${v.toFixed(1)}%`);

// Per-state rows for the chart hover card.
function breakdown(key, digits) {
  return (d) => (
    <div style={{ marginTop: 8 }}>
      {(d.day.states || []).filter((s) => s[key] > 0).map((s) => (
        <Flex key={s.state} justify="space-between" gap={12} style={{ marginTop: 4 }}>
          <span>{stateNames[s.state] || s.state}</span><span style={{ fontVariantNumeric: "tabular-nums" }}>{digits ? s[key].toFixed(digits) : s[key]}</span>
        </Flex>
      ))}
      <Flex justify="space-between" style={{ marginTop: 8, paddingTop: 6, borderTop: "1px solid var(--console-border-soft)" }}>
        <span>{t("完整 / 部分采样")}</span><span>{d.day.completeJobCount} / {d.day.partialJobCount}</span>
      </Flex>
    </div>
  );
}

export default function Usage({ page }) {
  const { message } = App.useApp();
  const all = page.meta.scope === "all";
  const [states, setStates] = useState([]);
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [denied, setDenied] = useState(false);

  const refresh = async (sel = states) => {
    setLoading(true);
    setDenied(false);
    try {
      const d = await get(all ? "/api/v2/resource-monitor/stats" : "/api/v2/resource-monitor/my-stats", data ? { states: sel.join(",") } : undefined);
      setData(d);
      if (!sel.length) setStates(d.selectedStates || []);
    } catch (e) {
      if (e instanceof ApiError && e.status === 403 && !all) setDenied(true);
      else message.error(errorText(e));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    refresh();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const days = [...(data?.days || [])].sort((a, b) => a.date.localeCompare(b.date));
  const observed = days.reduce((a, d) => a + (d.observedGpuSeconds || 0), 0);
  const weighted = days.reduce((a, d) => a + (d.gpuUtilizationPercent ?? 0) * (d.observedGpuSeconds || 0), 0);
  const totals = {
    jobs: days.reduce((a, d) => a + sum(d.states, "jobCount"), 0),
    gpu: days.reduce((a, d) => a + sum(d.states, "gpuHours"), 0),
    cpu: days.reduce((a, d) => a + sum(d.states, "cpuCoreHours"), 0),
    util: observed > 0 ? weighted / observed : null,
  };
  const users = (data?.users || []).map((u) => ({ ...u, jobCount: u.jobCount || 0, gpuHours: u.gpuHours || 0, cpuCoreHours: u.cpuCoreHours || 0 }));

  return (
    <>
      <PageHead title={all ? t("近期资源汇总") : t("我的资源统计")}>
        <Tag bordered={false}>{data?.timezone || "Asia/Shanghai"} {t("· 近 7 天")}</Tag>
        <Button icon={<ReloadOutlined />} loading={loading} onClick={() => refresh()}>{t("刷新数据")}</Button>
      </PageHead>
      {denied && <Alert type="info" showIcon title={t("你的账号没有有效的 POSIX UID，暂无个人资源数据")} />}
      {!denied && !data && <Spin spinning={loading}><div style={{ minHeight: 200 }} /></Spin>}
      {!denied && data && (
        <>
          <div className="stat-row">
            <StatCard label={t("作业总数")} value={totals.jobs} unit={t("个")} icon={ScheduleOutlined} />
            <StatCard label={t("GPU 分配时长")} value={totals.gpu.toFixed(2)} unit={t("GPU·时")} icon={ThunderboltOutlined} tone="teal" />
            <StatCard label={t("CPU 核时")} value={totals.cpu.toFixed(1)} unit={t("核·时")} icon={DashboardOutlined} tone="violet" />
            <StatCard label={t("平均 GPU 利用率")} value={totals.util === null ? "—" : totals.util.toFixed(1)} unit={totals.util === null ? "" : "%"} icon={FundOutlined} tone="amber" />
          </div>
          <Card className="surface" title={t("作业统计")}
            extra={<Select mode="multiple" maxTagCount={0} maxTagPlaceholder={(v) => t("结束状态：已选 {0} 种", v.length)} placeholder={t("全部结束状态")} style={{ width: 240 }} value={states}
              onChange={(v) => { setStates(v); refresh(v); }} options={(data.availableStates || []).map((s) => ({ value: s, label: stateLabel(s) }))} />}>
            <div className="grid-2">
              <div>
                <p className="muted" style={{ fontSize: 13, margin: "0 0 10px" }}>{t("每日作业数 · 个")}</p>
                <BarChart title={t("作业数")} unit={t("个")} details={breakdown("jobCount", 0)}
                  data={days.map((d) => ({ label: d.date.slice(5), full: d.date, value: sum(d.states, "jobCount"), day: d }))} />
              </div>
              <div>
                <p className="muted" style={{ fontSize: 13, margin: "0 0 10px" }}>{t("每日 GPU 分配时长 · GPU·时")}</p>
                <BarChart title={t("GPU 分配时长")} unit={t("GPU·时")} digits={1} color="var(--console-chart-2)" details={breakdown("gpuHours", 1)}
                  data={days.map((d) => ({ label: d.date.slice(5), full: d.date, value: sum(d.states, "gpuHours"), day: d }))} />
              </div>
            </div>
          </Card>
          <Card className="surface" title={t("按日明细")}>
            <Table rowKey="date" dataSource={data.days} pagination={false} size="middle" scroll={{ x: 800 }}
              columns={[
                { title: t("日期"), dataIndex: "date", width: 120 },
                { title: t("作业数"), width: 90, render: (_, r) => sum(r.states, "jobCount") },
                { title: t("CPU 核时"), render: (_, r) => sum(r.states, "cpuCoreHours").toFixed(1) },
                { title: t("GPU 小时"), render: (_, r) => sum(r.states, "gpuHours").toFixed(1) },
                { title: t("GPU 利用率"), render: (_, r) => pct(r.gpuUtilizationPercent) },
                { title: t("显存利用率"), render: (_, r) => pct(r.gpuMemoryUtilizationPercent) },
                { title: t("完整 / 部分采样"), width: 140, render: (_, r) => `${r.completeJobCount} / ${r.partialJobCount}` },
              ]} />
          </Card>
          {all && (
            <Card className="surface" title={t("用户资源排行")} extra={<span className="muted">{users.length} {t("个用户")}</span>}>
              <Table rowKey={(r) => r.uid} dataSource={users} size="middle" pagination={{ pageSize: 20, hideOnSinglePage: true }} scroll={{ x: 700 }}
                columns={[
                  { title: "#", width: 56, render: (_, __, i) => i + 1 },
                  { title: t("用户"), render: (_, r) => r.username || `uid ${r.uid}` },
                  { title: t("作业数"), dataIndex: "jobCount", width: 100, sorter: (a, b) => a.jobCount - b.jobCount },
                  { title: t("GPU 小时"), dataIndex: "gpuHours", defaultSortOrder: "descend", sorter: (a, b) => a.gpuHours - b.gpuHours, render: (v) => v.toFixed(1) },
                  { title: t("CPU 核时"), dataIndex: "cpuCoreHours", sorter: (a, b) => a.cpuCoreHours - b.cpuCoreHours, render: (v) => v.toFixed(1) },
                  { title: t("GPU 利用率"), render: (_, r) => pct(r.gpuUtilizationPercent) },
                ]} />
            </Card>
          )}
        </>
      )}
    </>
  );
}
