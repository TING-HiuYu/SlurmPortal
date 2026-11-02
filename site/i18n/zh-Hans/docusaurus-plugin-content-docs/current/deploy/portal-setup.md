# 门户初始化配置

当主服务启动并运行后（无论是 [Serverless 主服务](master-saas.md)还是[容器主服务](master-container.md)），请在接入集群 slave 代理之前，使用 `admin` 账号登录 Web 控制台完成 SSH 证书颁发机构（CA）、主 POSIX 组预设、RBAC 角色、邮件服务商与可选 GitHub OAuth 登录的初始化配置。

**预计用时：** 10 分钟。

## 1. 创建 SSH 证书颁发机构（SSH CA） {#1-create-an-ssh-certificate-authority}

各集群的 `sshd` 将信任您在此处创建的 SSH CA：

1. 进入 **管理后台 → 证书颁发机构（Certificate authorities）**。
2. 点击 **创建 CA**，类型选择 **SSH (`ssh`)**。
3. 选择 **生成 Ed25519 密钥对**（或导入现有的 OpenSSH CA 私钥），并填写名称（例如 `Cluster SSH CA`）。
4. 确认其状态为 **已启用（Enabled）**。

主服务会立即使用 `PKI_MASTER_KEY` 加密封存 CA 私钥，并将 CA 公钥包含在下一份集群快照中，由各集群 slave 自动发布至 `current/ssh/trusted-user-ca-keys.pub`。

## 2. 创建主 POSIX 组与主目录预设 {#2-create-primary-posix-groups-and-home-presets}

Slurm Portal 中的每位用户都必须归属于唯一的一个**主 POSIX 组**，该组同时决定了用户的默认 GID、主目录根路径与默认登录 Shell：

1. 进入 **管理后台 → 用户组（Groups）** 并点击 **创建用户组**。
2. 选择 **POSIX 组 (`posixGroup`)**。
3. 填写组名（例如 `research`），将 **主目录根路径（Home root）** 设为 `/home/research`，将 **默认 Shell** 设为 `/bin/bash`。
4. 也可按需创建 **组织架构组 (`groupOfUniqueNames`)** 来映射院系或课题组树形结构。

此后当您创建归属于 `research` 主组的用户 `alice` 时，其主目录会自动设为 `/home/research/alice`，并在同步时由集群上的 `slurm_trigger.py` 自动在 NFSv4 共享存储上创建该目录。

## 3. 配置角色与分级授权（RBAC） {#3-configure-roles-and-delegated-administration}

系统内置了等级（Rank）为 `1` 的超级管理员角色。如需将成员管理权限下放给各课题组负责人，同时防止其越权操作：

1. 进入 **管理后台 → 角色与权限（Roles）** 并点击 **创建角色**。
2. 设置大于 `1` 的数值 **等级（`sort`）**（例如集群运维设为 `10`，课题组负责人设为 `50`）。
3. 仅勾选所需的 `(object, action)` 权限（如 `user:read`、`user:create`、`user:update` 与 `group:update`）。

受等级防提权机制保护，等级为 `50` 的角色绝无法修改等级在 `1`–`50` 之间的用户或角色，也无法授出自身未持有的权限。

## 4. 配置邮件服务与通知模板 {#4-configure-email-providers-and-templates}

作业完成报告、跨集群文件传输通知及密码重置验证码均通过**邮件中枢**发送：

1. 进入 **管理后台 → 邮件服务（Mail service）**，添加一个 **SMTP** 服务商（或 **REST** 邮件 API 服务商）。
2. 点击 **测试（Test）** 发送一封测试邮件，确认通道畅通。
3. *(可选)* 如需组合多个有日发送量上限的邮箱账号，可创建 **邮件池 (`pool`)** 服务商，按优先级配额、轮询或权重自动分发并支持故障切换。
4. 在以下页面中开启通知并按需自定义 HTML 模板：
   - **密码验证码（Password codes）**（开启后登录页将显示“忘记密码”入口）；
   - **作业通知（Job notifications）**（发送作业结束报告）；
   - **传输通知（Transfer notifications）**（发送跨集群 `rsync` 完成通知）。

## 5. 配置 GitHub OAuth 登录（可选） {#5-configure-github-oauth-sign-in-optional}

如需允许用户使用 GitHub 账号登录并按规则自动归入对应的 POSIX 组：

1. 在 GitHub（**Settings → Developer settings → OAuth Apps**）中创建 OAuth 应用，回调地址填：
   `https://portal.example.org/api/v2/oauth/callback/github`
2. 将 Client Secret 存入主服务环境变量（`OAUTH_GITHUB_CLIENT_SECRET`）或在创建连接器时直接填入。
3. 进入 **管理后台 → OAuth 连接器**（或运行 `deploy/worker/connector-github.mjs`），按匹配顺序列出准入规则：
   - 支持按 **GitHub 组织成员**、**用户名/邮箱通配符** 或 **所有人** 匹配；
   - 指定匹配成功后新账号自动分配的 **主 POSIX 组**、**附加组** 与 **角色**。

## 下一步 {#next}

继续阅读[集群 Slave 代理](slaves.md)，将您的第一个 Slurm 集群接入主服务。
