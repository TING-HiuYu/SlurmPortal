import TemplateWorkbench from "../../components/TemplateWorkbench.jsx";
import { t } from "../../i18n.js";

// Settings and template of the e-mailed password code (忘记密码 on the login page, 邮箱验证码 in 修改密码).
export default function PasswordMail() {
  return <TemplateWorkbench title={t("密码验证码")} base="/api/v2/password-mail" configUrl="/api/v2/password-mail/config" perm="notification" enableLabel={t("启用邮箱验证码")} />;
}
