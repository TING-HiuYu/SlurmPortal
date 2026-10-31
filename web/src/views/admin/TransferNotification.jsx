import TemplateWorkbench from "../../components/TemplateWorkbench.jsx";
import { t } from "../../i18n.js";

export default function TransferNotification() {
  return (
    <TemplateWorkbench title={t("传输结束通知")} desc={t("跨集群文件传输结束后按模板给发起人发邮件。")} base="/api/v2/web-access/transfer-notification" perm="web-access"
      enableLabel={t("启用传输完成邮件")} />
  );
}
