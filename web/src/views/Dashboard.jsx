import { useEffect, useState } from "react";
import { Link } from "react-router";
import { Badge, Card, Col, Flex, List, Row, Tag, Typography } from "antd";
import { AppstoreOutlined, CalendarOutlined, LockOutlined, RightOutlined, TeamOutlined, UserOutlined } from "@ant-design/icons";
import PageHead from "../components/PageHead.jsx";
import StatCard from "../components/StatCard.jsx";
import { get } from "../api/client.js";
import { nodesFromR2 } from "../currentstatus.js";
import { can, hasPlugin } from "../session.js";
import { visibleGroups } from "../routes.jsx";
import { antColor, clusterStatus, nodeStatus, label, tag, ago } from "../format.js";
import { dateLocale, t } from "../i18n.js";

const tones = ["blue", "teal", "violet", "amber"];
const size = (v) => (Array.isArray(v) ? v.length : (v?.total ?? v?.items?.length ?? null));

export default function Dashboard() {
  const [counts, setCounts] = useState({});
  const [clusters, setClusters] = useState([]);
  const entries = visibleGroups("admin").flatMap((g) => g.items).filter((r) => r.meta?.desc).slice(0, 9);
  const online = clusters.filter((c) => c.status === "normal" || c.status === "partial_maintenance").length;

  useEffect(() => {
    Promise.allSettled([
      can("user") ? get("/api/v2/users", { size: 1 }) : null,
      can("group") ? get("/api/v2/groups") : null,
      can("role") ? get("/api/v2/roles") : null,
      // Same source as 集群状态: the live R2 snapshot, falling back to the master API.
      hasPlugin("cluster-status") ? nodesFromR2().catch(() => null).then((r) => r || get("/api/v2/cluster-status")) : null,
    ]).then(([u, g, r, s]) => {
      const val = (x) => (x.status === "fulfilled" && x.value ? size(x.value) : null);
      setCounts({ users: val(u), groups: val(g), roles: val(r) });
      if (s.status === "fulfilled" && s.value) setClusters(s.value.clusters || []);
    });
  }, []);

  return (
    <>
      <PageHead title={t("控制台总览")}>
        <Typography.Text type="secondary"><CalendarOutlined /> {new Date().toLocaleDateString(dateLocale, { year: "numeric", month: "long", day: "numeric", weekday: "long" })}</Typography.Text>
      </PageHead>
      <div className="stat-row">
        {can("user") && <StatCard label={t("用户管理")} value={counts.users ?? "—"} icon={UserOutlined} to="/admin/users" />}
        {can("group") && <StatCard label={t("用户组")} value={counts.groups ?? "—"} icon={TeamOutlined} to="/admin/groups" />}
        {can("role") && <StatCard label={t("角色与授权")} value={counts.roles ?? "—"} icon={LockOutlined} to="/admin/roles" />}
        {clusters.length > 0 && <StatCard label={t("集群在线")} value={`${online} / ${clusters.length}`} icon={AppstoreOutlined} tone="teal" to={hasPlugin("cluster-status") ? "/admin/clusters" : undefined} />}
      </div>

      <Card className="surface" title={t("常用入口")} extra={<Typography.Text type="secondary">{entries.length}</Typography.Text>}>
        <List grid={{ gutter: 24, xs: 1, sm: 1, md: 2, lg: 3, xl: 3, xxl: 3 }} dataSource={entries}
          renderItem={(e, i) => (
            <List.Item style={{ marginBottom: 8 }}>
              <Link to={e.path}>
                <Flex align="center" gap={14} style={{ padding: "8px 4px" }}>
                  <span className={`chip ${tones[i % tones.length]}`} style={{ width: 42, height: 42, borderRadius: 10, fontSize: 18 }}><e.icon /></span>
                  <Flex vertical gap={4} style={{ minWidth: 0, flex: 1 }}>
                    <Typography.Text strong>{e.title}</Typography.Text>
                    <Typography.Text type="secondary" ellipsis style={{ fontSize: 12 }}>{e.meta.desc}</Typography.Text>
                  </Flex>
                  <RightOutlined style={{ color: "var(--console-placeholder)" }} />
                </Flex>
              </Link>
            </List.Item>
          )} />
      </Card>

      {clusters.length > 0 && (
        <Card className="surface" title={t("集群状态")} extra={hasPlugin("cluster-status") && <Link to="/admin/clusters">{t("查看全部 →")}</Link>}>
          <Row gutter={[16, 16]}>
            {clusters.map((c) => (
              <Col key={c.id} xs={24} md={12} xl={8}>
                <Card size="small" style={{ background: "var(--console-surface-soft)" }}>
                  <Flex justify="space-between" align="center">
                    <Typography.Text strong>{c.name}</Typography.Text>
                    <Tag color={antColor(tag(clusterStatus, c.status))}>{label(clusterStatus, c.status)}</Tag>
                  </Flex>
                  <Flex wrap gap="6px 14px" style={{ margin: "12px 0", fontSize: 12 }}>
                    {Object.entries(c.nodeCounts || {}).map(([s, n]) => <Badge key={s} status={antColor(tag(nodeStatus, s))} text={`${label(nodeStatus, s)} ${n}`} />)}
                  </Flex>
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t("观测于")} {ago(c.observedAt || c.reportedAt)}</Typography.Text>
                </Card>
              </Col>
            ))}
          </Row>
        </Card>
      )}
    </>
  );
}
