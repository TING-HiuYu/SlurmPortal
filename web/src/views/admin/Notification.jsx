import TemplateWorkbench from "../../components/TemplateWorkbench.jsx";
import { t } from "../../i18n.js";

export default function Notification() {
  return <TemplateWorkbench title={t("作业结束通知")} base="/api/v2/notification" configUrl="/api/v2/notification/config" perm="notification" enableLabel={t("启用作业邮件")} />;
}
