# 致谢

Slurm Portal 的诞生离不开众多开源项目与贡献者的工作。

## go-ldap-admin {#go-ldap-admin}

Slurm Portal 最初复刻自 [eryajf](https://github.com/eryajf) 及其贡献者开发的 [go-ldap-admin](https://github.com/eryajf/go-ldap-admin)。其核心账户模型、初始数据库表结构（v2 依然能够直接兼容打开）以及通过 Web 控制台管理 OpenLDAP 的设计理念，构成了本项目的基础。谨在此致以诚挚感谢。

## 依赖的开源软件 {#open-source-dependencies}

| 项目 | 在 Slurm Portal 中的用途 |
| --- | --- |
| [Slurm](https://slurm.schedmd.com/) (SchedMD) | 本门户所服务的 HPC 作业调度与集群管理系统 |
| [OpenLDAP](https://www.openldap.org/)、[SSSD](https://sssd.io/)、[OpenSSH](https://www.openssh.com/) | 集群目录服务、节点身份解析与基于证书的 SSH 登录认证 |
| [Go](https://go.dev/)（`go-ldap`、`golang-jwt`、`casbin`、`modernc.org/sqlite`、`x/crypto`） | 构建 Master 控制平面、集群 Slave 代理、Web 访问网关与生命周期守护进程 |
| [Cloudflare Workers](https://workers.cloudflare.com/)、D1、R2 | 无服务器运行时、分布式 SQLite 数据库与对象存储 |
| [Caddy](https://caddyserver.com/)、[RustFS](https://rustfs.com/) | 容器化部署方案中的自动 HTTPS 反向代理与 S3 兼容对象存储 |
| [React](https://react.dev/)、[Ant Design](https://ant.design/)、[xterm.js](https://xtermjs.org/)、[Vite](https://vite.dev/) | 管理控制台、用户工作台界面与交互式浏览器终端 |
| [Docusaurus](https://docusaurus.io/) | 本项目文档网站与多语言全文检索 |
| [Archify](https://github.com/tt-a1i/archify) | 交互式系统架构图生成工具 |

## 文档网站视觉设计 {#documentation-website-design}

首页视觉风格受 QingXia-Ela 开发的 [Rhine-Lab-Theme-Index-Page](https://github.com/QingXia-Ela/Rhine-Lab-Theme-Index-Page)（MIT 许可证）启发，但未使用其中的任何美术素材或图像资源。全站字体采用遵循 SIL Open Font License 的 [Michroma](https://fonts.google.com/specimen/Michroma)、[Saira](https://fonts.google.com/specimen/Saira) 与 [Noto Sans SC](https://fonts.google.com/noto/specimen/Noto+Sans+SC)。

## 维护者 {#maintainer}

Slurm Portal 由 [Suzuki coupling](https://github.com/TING-HiuYu) 开发与维护，相关技术文章与设计随笔发布于 [blog.thy.icu](https://blog.thy.icu)。
