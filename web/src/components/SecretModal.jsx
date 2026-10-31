import { Alert, App, Button, Modal } from "antd";
import { download } from "../hooks.js";
import { t } from "../i18n.js";

// Shows a one-time secret (password, private key) with copy and download.
export default function SecretModal({ open, title, text, filename, onClose }) {
  const { message } = App.useApp();
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      message.success(t("已复制"));
    } catch {
      message.error(t("复制失败，请手动选择文本"));
    }
  };
  return (
    <Modal open={open} title={title} width={560} maskClosable={false} onCancel={onClose}
      footer={[<Button key="c" onClick={copy}>{t("复制")}</Button>, filename && <Button key="d" onClick={() => download(text, filename)}>{t("下载")}</Button>, <Button key="ok" type="primary" onClick={onClose}>{t("我已保存")}</Button>]}>
      <Alert type="warning" showIcon title={t("该内容只显示这一次，关闭后无法再次查看")} style={{ marginBottom: 12 }} />
      <pre className="secret mono">{text}</pre>
    </Modal>
  );
}
