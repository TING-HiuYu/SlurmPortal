// Slurm Portal project site: landing page, developer wiki and deployment wiki.
// The wikis are the repository's docs/ folder, so they stay readable on GitHub too.
import { themes as prismThemes } from "prism-react-renderer";

const repo = "https://github.com/TING-HiuYu/SlurmPortal";

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: "Slurm Portal",
  tagline: "Accounts, SSH certificates and live status for Slurm HPC clusters",
  favicon: "img/logo.svg",
  url: "https://ting-hiuyu.github.io",
  baseUrl: "/SlurmPortal/",
  organizationName: "TING-HiuYu",
  projectName: "SlurmPortal",
  trailingSlash: false,
  clientModules: ["./src/fonts.js"],
  onBrokenLinks: "throw",
  onBrokenAnchors: "throw",
  markdown: {
    // .md files are plain CommonMark (they must also render on GitHub); .mdx enables JSX.
    format: "detect",
    mermaid: true,
    hooks: { onBrokenMarkdownLinks: "throw", onBrokenMarkdownImages: "throw" },
  },
  i18n: {
    defaultLocale: "en",
    locales: ["en", "zh-Hans"],
    localeConfigs: {
      en: { label: "English", htmlLang: "en" },
      "zh-Hans": { label: "简体中文", htmlLang: "zh-CN" },
    },
  },
  presets: [
    [
      "classic",
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: "../docs",
          routeBasePath: "docs",
          sidebarPath: "./sidebars.js",
          editUrl: `${repo}/edit/main/docs/`,
          editLocalizedFiles: false,
          showLastUpdateTime: false,
          exclude: ["**/diagrams/**"],
        },
        blog: false,
        theme: { customCss: "./src/css/custom.css" },
      }),
    ],
  ],
  themes: [
    "@docusaurus/theme-mermaid",
    [
      "@easyops-cn/docusaurus-search-local",
      {
        hashed: true,
        language: ["en", "zh"],
        docsRouteBasePath: "docs",
        indexBlog: false,
        highlightSearchTermsOnTargetPage: true,
        explicitSearchResultPath: true,
      },
    ],
  ],
  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      image: "img/social-card.png",
      colorMode: { respectPrefersColorScheme: true },
      docs: { sidebar: { hideable: true } },
      navbar: {
        title: "Slurm Portal",
        logo: { alt: "Slurm Portal", src: "img/logo.svg" },
        items: [
          { type: "docSidebar", sidebarId: "dev", position: "left", label: "Developer wiki" },
          { type: "docSidebar", sidebarId: "deploy", position: "left", label: "Deployment wiki" },
          { to: "/docs/faq", label: "FAQ", position: "left" },
          { to: "/docs/changelog", label: "Changelog", position: "left" },
          { href: "https://blog.thy.icu", label: "Blog", position: "right" },
          { type: "localeDropdown", position: "right" },
          { href: repo, position: "right", className: "header-github-link", "aria-label": "GitHub repository" },
        ],
      },
      footer: {
        style: "dark",
        links: [
          {
            title: "Docs",
            items: [
              { label: "Developer wiki", to: "/docs/dev" },
              { label: "Deployment wiki", to: "/docs/deploy" },
              { label: "FAQ", to: "/docs/faq" },
            ],
          },
          {
            title: "Project",
            items: [
              { label: "GitHub", href: repo },
              { label: "Security policy", href: `${repo}/blob/main/SECURITY.md` },
              { label: "Contributing", href: `${repo}/blob/main/CONTRIBUTING.md` },
            ],
          },
          {
            title: "More",
            items: [
              { label: "Blog", href: "https://blog.thy.icu" },
              { label: "Acknowledgements", to: "/docs/acknowledgements" },
              { label: "go-ldap-admin", href: "https://github.com/eryajf/go-ldap-admin" },
            ],
          },
        ],
        copyright: `Slurm Portal · Apache License 2.0`,
      },
      prism: {
        theme: prismThemes.github,
        darkTheme: prismThemes.dracula,
        additionalLanguages: ["bash", "yaml", "toml", "ini", "json", "go", "nginx", "sql", "docker"],
      },
      mermaid: { theme: { light: "neutral", dark: "dark" } },
    }),
};

export default config;
