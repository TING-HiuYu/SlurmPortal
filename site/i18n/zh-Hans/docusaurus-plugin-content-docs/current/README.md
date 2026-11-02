# Slurm Portal 文档中心

本文档库同步发布为在线文档网站（内置全文检索与中英文切换）：**https://ting-hiuyu.github.io/SlurmPortal/**。如需快速了解项目定位与核心特性，请先参阅[项目 README](https://github.com/TING-HiuYu/SlurmPortal/blob/main/README.md)。

## 部署 Wiki {#deployment-wiki}

从全新的 Ubuntu 24.04 LTS 服务器起步，一步步搭建由 Slurm Portal 统一接管的生产级 Slurm 集群。

| 页面 | 内容概览 |
| --- | --- |
| [部署总览](deploy/README.md) | 系统组件构成、Serverless 与容器主服务对比，以及完整部署路线图 |
| [站点规划](deploy/plan.md) | 节点角色划分、主机名解析、防火墙端口矩阵与 POSIX UID/GID 号段规划 |
| [Munge](deploy/munge.md)、[Slurm](deploy/slurm.md)、[记账服务](deploy/accounting.md)、[slurmrestd](deploy/slurmrestd.md) | 编译与配置 Slurm 26.05、MariaDB 作业记账（`slurmdbd`）及基于 JWT 的 REST API |
| [OpenLDAP](deploy/openldap.md)、[SSSD 与 SSH](deploy/sssd-ssh.md)、[共享主目录与信任文件](deploy/shared-home.md) | 目录服务搭建、节点身份解析，以及承载 `$HOME` 与 SSH 信任文件的 NFSv4 共享存储 |
| [Serverless 主服务](deploy/master-saas.md)、[容器主服务](deploy/master-container.md) | 在 Cloudflare Workers 或 Docker Compose 上部署 Slurm Portal 控制平面 |
| [门户初始化配置](deploy/portal-setup.md) | 在控制台中配置 SSH 证书颁发机构（CA）、POSIX 组预设、RBAC 角色、邮件通道与 OAuth |
| [集群 Slave 代理](deploy/slaves.md) | 将集群接入主服务、离线预演同步计划，并启用 Slurm 账户与主目录触发器 |
| [作业通知与 GPU 监控](deploy/optional.md) | 部署 `slurm-lifecycle-hook`、`slurm-notify-observer` 与 slave 生命周期运行时 |
| [Web 访问网关](deploy/gateway.md) | 在登录节点部署 `web-access-gateway` 与 Nginx 反向代理，提供浏览器终端、SFTP 与 `rsync` 传输 |
| [端到端验证](deploy/verify.md) | 完整验证账号下发、SSH 证书登录、实时监控与证书吊销 |
| [日常运维指南](deploy/operations.md) | 日常管理操作、备份恢复、密钥与证书轮换及故障排查手册 |

## 开发 Wiki {#developer-wiki}

分层剖析 Slurm Portal 的系统架构、通信协议、数据模型与安全机制。

| 页面 | 内容概览 |
| --- | --- |
| [开发总览](dev/README.md) | 系统全景图、阅读路线指南与核心术语表 |
| [系统架构](dev/architecture.md) | 核心可执行程序、双运行时对比、网络拓扑与 Serverless 成本优化模型 |
| [请求生命周期](dev/request-lifecycle.md) | 追踪一次 HTTP 请求从边缘网关到数据库的完整执行链路 |
| [同步协议](dev/sync-protocol.md) | 快照生成、哈希发布、slave 增量对账、安全防线与触发器队列 |
| [HTTP 与边缘层](dev/layer-http.md)、[身份与访问层](dev/layer-access.md)、[领域模块层](dev/layer-domain.md)、[插件系统](dev/plugins.md)、[数据模型](dev/data-model.md)、[存储层](dev/layer-storage.md) | 深入各架构分层与数据库表结构设计 |
| [集群代理程序](dev/agents.md)、[Web 前端应用](dev/web-app.md) | Slave 代理、Web 访问网关、Slurm 生命周期钩子及 React 单页应用实现 |
| [安全模型](dev/security.md)、[密码学与签名](dev/cryptography.md) | 信任边界、认证机制、密钥体系与静态数据加密封存 |
| [API 参考](dev/api-reference.md) | 直接从路由器自动生成的全量 HTTP 接口参考 |
| [项目目录结构](dev/project-layout.md)、[本地开发指南](dev/development.md) | 代码库目录导览、本地调试环境搭建、测试套件与编码规范 |

## 更多资源 {#additional-resources}

- [常见问题与故障排查](faq.md)
- [版本更新日志](changelog.md)
- [致谢](acknowledgements.md)
- [交互式架构图](https://ting-hiuyu.github.io/SlurmPortal/architecture.html)

各部署组件的模板说明与辅助脚本保存在代码仓库的 [`deploy/`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/README.md) 目录下。
