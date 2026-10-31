import { Button, Dropdown } from "antd";
import { TranslationOutlined } from "@ant-design/icons";
import { locale, locales, setLocale, t } from "../i18n.js";

// Language drop-down beside the theme switch; choosing a language reloads the page in it.
export default function LocaleSwitch({ compact = false }) {
  const current = locales.find((l) => l.value === locale);
  return (
    <Dropdown trigger={["click"]} placement="bottomRight"
      menu={{ selectable: true, selectedKeys: [locale], items: locales.map((l) => ({ key: l.value, label: l.label })), onClick: ({ key }) => setLocale(key) }}>
      <Button type="text" aria-label={t("语言")} title={compact ? t("语言") : undefined} icon={<TranslationOutlined />}>{!compact && current?.label}</Button>
    </Dropdown>
  );
}
