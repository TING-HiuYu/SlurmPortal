import { useState } from "react";
import { Alert, Badge, Button, Card, Col, Descriptions, Divider, Empty, Flex, Row, Select, Switch, Tag, Tooltip, Typography } from "antd";
import { ReloadOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get } from "../../api/client.js";
import { usePoll } from "../../hooks.js";
import { nodesFromR2 } from "../../currentstatus.js";
import { antColor, nodeStatus, clusterStatus, label, tag, time } from "../../format.js";
import { t } from "../../i18n.js";

const errorText = (e) => ({ never_observed: t("尚未收到该集群的状态上报"), stale_snapshot: t("状态上报已过期，集群可能离线"), collection_failed: t("集群侧采集失败") })[e] || e;

export default function Nodes() {
  const { data, loading, refresh, paused, setPaused } = usePoll(async () => (await nodesFromR2().catch(() => null)) || get("/api/v2/cluster-status"), 30000);
  const [selected, setSelected] = useState([]);
  const all = data?.clusters || [];
  const clusters = selected.length ? all.filter((c) => selected.includes(c.id)) : all;
  return (
    <>
      <PageHead title={t("集群状态")}>
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
          <Typography.Text type="secondary" style={{ fontSize: 12, textAlign: "right" }}>{all.length} {t("个集群")}<br />{t("更新于")} {time(data?.updatedAt)}</Typography.Text>
        </Flex>
      </Card>
      {!loading && !clusters.length && <Empty description={t("没有配置集群")} />}
      <Row gutter={[22, 22]}>
        {clusters.map((c) => (
          <Col key={c.id} xs={24} xl={12}>
            <Card title={<Typography.Title level={5} style={{ margin: 0 }}>{c.name}</Typography.Title>}
              extra={<Tag color={antColor(tag(clusterStatus, c.status))}>{label(clusterStatus, c.status)}</Tag>}>
              <Descriptions column={1} size="small" colon={false} styles={{ label: { width: 96 } }}
                items={[
                  { label: t("访问地址"), children: <span className="mono">{c.address || "—"}</span> },
                  { label: t("当前 IP"), children: c.currentIps?.length ? <Flex gap={6} wrap>{c.currentIps.map((ip) => <Tag key={ip} className="mono">{ip}</Tag>)}</Flex> : <Typography.Text type="secondary">{t("未解析")}</Typography.Text> },
                ]} />
              {c.error && <Alert type="warning" showIcon title={errorText(c.error)} style={{ marginTop: 8 }} />}
              <Divider style={{ margin: "16px 0 12px" }} />
              <Flex justify="space-between"><Typography.Text strong>{t("节点")}</Typography.Text><Typography.Text type="secondary" style={{ fontSize: 12 }}>{c.nodesTotal ?? c.nodes?.length ?? 0} {t("个节点")}</Typography.Text></Flex>
              <Flex wrap gap="6px 14px" style={{ margin: "10px 0 12px", fontSize: 12 }}>
                {Object.entries(c.nodeCounts || {}).map(([s, n]) => <Badge key={s} status={antColor(tag(nodeStatus, s))} text={`${label(nodeStatus, s)} ${n}`} />)}
              </Flex>
              <Row gutter={[10, 10]} style={{ maxHeight: 360, overflowY: "auto" }}>
                {(c.nodes || []).map((n) => (
                  <Col key={n.name} span={12}>
                    <Tooltip title={n.slurm_state}>
                      <Card size="small" style={{ background: "var(--console-surface-soft)" }} styles={{ body: { padding: "10px 14px" } }}>
                        <Badge status={antColor(tag(nodeStatus, n.status))} text={<Typography.Text className="mono" ellipsis style={{ maxWidth: "85%" }}>{n.name}</Typography.Text>} />
                        <div className="muted" style={{ paddingLeft: 14 }}>{label(nodeStatus, n.status)}</div>
                      </Card>
                    </Tooltip>
                  </Col>
                ))}
              </Row>
              {c.nodesTruncated && <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("仅显示前")} {c.nodes.length} / {c.nodesTotal} {t("个节点")}</Typography.Text>}
              <Divider style={{ margin: "14px 0 10px" }} />
              <Flex gap={22} wrap className="muted"><span>{t("采集时间")} {time(c.observedAt)}</span><span>{t("最近报告")} {time(c.reportedAt || c.receivedAt)}</span></Flex>
            </Card>
          </Col>
        ))}
      </Row>
    </>
  );
}
