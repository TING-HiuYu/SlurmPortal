import { Card, Flex, Typography } from "antd";
import { RightOutlined } from "@ant-design/icons";
import { Link } from "react-router";
import { t } from "../i18n.js";

export default function StatCard({ label, value, unit, icon: Icon, to, tone = "blue", hint }) {
  const body = (
    <Card hoverable={!!to} styles={{ body: { padding: "24px 24px 22px" } }}>
      <Flex justify="space-between" align="center">
        <Typography.Text type="secondary">{label}</Typography.Text>
        {Icon && <span className={`chip ${tone}`}><Icon /></span>}
      </Flex>
      <Flex justify="space-between" align="flex-end" style={{ marginTop: 18 }}>
        <span style={{ fontSize: 34, fontWeight: 700, letterSpacing: -0.5, lineHeight: 1.1, fontVariantNumeric: "tabular-nums" }}>
          {value}
          {unit && <small style={{ fontSize: 12, fontWeight: 400, marginLeft: 8, color: "var(--console-muted)" }}>{unit}</small>}
        </span>
        {to ? <span className="muted">{t("打开")} <RightOutlined style={{ fontSize: 10 }} /></span> : hint && <span className="muted">{hint}</span>}
      </Flex>
    </Card>
  );
  return to ? <Link to={to}>{body}</Link> : body;
}
