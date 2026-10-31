import { useEffect, useRef, useState } from "react";
import { App, Button, Card, Flex, Table, Tag } from "antd";
import { ReloadOutlined, SafetyOutlined } from "@ant-design/icons";
import PageHead from "../../components/PageHead.jsx";
import { get, errorText } from "../../api/client.js";
import { time } from "../../format.js";
import { t } from "../../i18n.js";

// Newest first; "load more" pages back to older records.
export default function Audit() {
  const { message } = App.useApp();
  const [rows, setRows] = useState([]);
  const [loading, setLoading] = useState(false);
  const [done, setDone] = useState(false);
  const [verify, setVerify] = useState(null);
  const before = useRef(0);

  const more = async (reset = false) => {
    setLoading(true);
    try {
      if (reset) before.current = 0;
      const batch = await get("/api/v2/audit", { before: before.current, limit: 200 });
      if (batch.length) before.current = batch[batch.length - 1].seq;
      setDone(batch.length < 200);
      setRows((r) => (reset ? batch : [...r, ...batch]));
    } catch (e) {
      message.error(errorText(e));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    more(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);
  const check = async () => {
    try {
      setVerify(await get("/api/v2/audit/verify"));
    } catch (e) {
      message.error(errorText(e));
    }
  };

  return (
    <>
      <PageHead title={t("操作日志")}>
        {verify && (
          <Tag color={verify.intact ? "success" : "error"}>
            {!verify.enabled ? t("审计未启用") : verify.intact ? t("哈希链完整（至 #{0}）", verify.verifiedThrough) : t("哈希链在 #{0} 处断裂：{1}", verify.brokenAt, verify.reason)}
          </Tag>
        )}
        <Button icon={<SafetyOutlined />} onClick={check}>{t("校验完整性")}</Button>
        <Button icon={<ReloadOutlined />} onClick={() => more(true)}>{t("最新")}</Button>
      </PageHead>
      <Card>
        <Table rowKey="seq" loading={loading} dataSource={rows} pagination={false} scroll={{ x: 1000 }} size="middle"
          columns={[
            { title: "#", dataIndex: "seq", width: 80 },
            { title: t("时间"), width: 180, render: (_, r) => time(r.time) },
            { title: t("操作者"), dataIndex: "actor", width: 130 },
            { title: t("操作"), dataIndex: "action", ellipsis: true, render: (v) => <span className="mono">{v}</span> },
            { title: t("对象"), dataIndex: "target", ellipsis: true },
            { title: t("结果"), width: 100, render: (_, r) => <Tag color={r.outcome === "success" ? "success" : r.outcome === "denied" ? "warning" : "error"}>{r.status}</Tag> },
            { title: t("请求 ID"), dataIndex: "requestId", width: 150, ellipsis: true, render: (v) => <span className="mono">{v}</span> },
          ]} />
        <Flex justify="center" style={{ marginTop: 16 }}><Button disabled={done} loading={loading} onClick={() => more()}>{t("加载更多")}</Button></Flex>
      </Card>
    </>
  );
}
