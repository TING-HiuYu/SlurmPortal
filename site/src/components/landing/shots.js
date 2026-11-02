// Portal screenshots for the landing page, taken from the local container demo (demo data only).
// The English set lives in docs/images, the Chinese set in docs/images/zh.
const en = {
  dashboard: require("@site/../docs/images/dashboard.png").default,
  resources: require("@site/../docs/images/resources.png").default,
  clusters: require("@site/../docs/images/clusters.png").default,
  users: require("@site/../docs/images/users.png").default,
  groups: require("@site/../docs/images/groups.png").default,
  pki: require("@site/../docs/images/pki.png").default,
  mail: require("@site/../docs/images/mail.png").default,
  notification: require("@site/../docs/images/notification.png").default,
  diagnostics: require("@site/../docs/images/diagnostics.png").default,
  terminal: require("@site/../docs/images/terminal.png").default,
  audit: require("@site/../docs/images/audit.png").default,
  roles: require("@site/../docs/images/roles.png").default,
  login: require("@site/../docs/images/login.png").default,
};
const zh = {
  dashboard: require("@site/../docs/images/zh/dashboard.png").default,
  resources: require("@site/../docs/images/zh/resources.png").default,
  clusters: require("@site/../docs/images/zh/clusters.png").default,
  users: require("@site/../docs/images/zh/users.png").default,
  groups: require("@site/../docs/images/zh/groups.png").default,
  pki: require("@site/../docs/images/zh/pki.png").default,
  mail: require("@site/../docs/images/zh/mail.png").default,
  notification: require("@site/../docs/images/zh/notification.png").default,
  diagnostics: require("@site/../docs/images/zh/diagnostics.png").default,
  terminal: require("@site/../docs/images/zh/terminal.png").default,
  audit: require("@site/../docs/images/zh/audit.png").default,
  roles: require("@site/../docs/images/zh/roles.png").default,
  login: require("@site/../docs/images/zh/login.png").default,
};

/** shots returns the screenshot set for a Docusaurus locale. */
export default function shots(locale) {
  return locale === "zh-Hans" ? zh : en;
}

export const architecture = require("@site/../docs/images/architecture.png").default;
