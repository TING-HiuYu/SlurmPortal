//go:build !js

package main

// text is a route description in English and Simplified Chinese.
type text struct{ en, zh string }

// descriptions documents every route, keyed "METHOD path". apidoc fails when a route is missing,
// so a new endpoint cannot ship without a line here.
var descriptions = map[string]text{
	// app
	"GET /healthz":          {"Liveness check; also served on the sync endpoint.", "存活检查接口；在同步端点上同样提供。"},
	"GET /api/v2/me/access": {"Return the signed-in user's enabled plugins and granted permissions for building the UI navigation menu.", "返回当前登录用户可用的插件与权限集合，用于构建前端导航菜单。"},
	// audit
	"GET /api/v2/audit":        {"List audit log records with pagination and filtering.", "分页查询审计日志记录。"},
	"GET /api/v2/audit/verify": {"Verify the SHA-256 audit hash chain and report the first tampered or broken record, if any.", "遍历校验 SHA-256 审计哈希链，若存在篡改或断裂则返回首条异常记录。"},
	// auth
	"POST /api/v2/auth/login":                  {"Authenticate with username and password and issue a session token (HTTPS required).", "使用用户名与密码登录并签发会话令牌（仅限 HTTPS）。"},
	"POST /api/v2/auth/logout":                 {"Revoke the caller's current session token.", "吊销当前会话令牌。"},
	"POST /api/v2/auth/refresh":                {"Exchange the current session token for a new one and revoke the old token ID.", "用当前令牌换取新会话令牌，同时吊销旧令牌 ID。"},
	"GET /api/v2/auth/password-reset":          {"Check whether self-service email password reset is enabled.", "查询是否已启用自助邮箱验证码重置密码功能。"},
	"POST /api/v2/auth/password-reset/request": {"Send a 6-digit password reset code to the account matching the username or email address.", "向匹配用户名或邮箱的账号发送 6 位密码重置验证码。"},
	"POST /api/v2/auth/password-reset/confirm": {"Reset an account's password using a valid 6-digit email verification code.", "凭有效的 6 位邮箱验证码设置新密码。"},
	"PUT /api/v2/me/password":                  {"Update the signed-in user's password using their current password or an emailed verification code.", "通过当前密码或邮箱验证码修改个人密码。"},
	"POST /api/v2/me/password/code":            {"Send a password-change verification code to the signed-in user's email address.", "向当前登录用户的邮箱发送修改密码验证码。"},
	"GET /api/v2/password-mail/config":         {"Retrieve the password verification email configuration and template.", "获取密码验证码邮件的配置与模板。"},
	"PUT /api/v2/password-mail/config":         {"Update the password verification email configuration and template.", "更新密码验证码邮件的配置与模板。"},
	"POST /api/v2/password-mail/preview":       {"Render the password verification email template with sample variables.", "使用示例变量渲染密码验证码邮件模板预览。"},
	"GET /api/v2/password-mail/sample":         {"Return sample payload data and allowed template variables for password verification emails.", "返回密码验证码邮件的示例数据与可用模板变量。"},
	"POST /api/v2/password-mail/test":          {"Send a test password verification email through the configured provider.", "通过配置的邮件服务商发送测试验证码邮件。"},
	// identity
	"GET /api/v2/me":                         {"Return the signed-in user's profile and group memberships.", "获取当前登录用户的个人资料与所属用户组。"},
	"PUT /api/v2/me/shell":                   {"Update the signed-in user's default login shell.", "修改当前登录用户的默认登录 Shell。"},
	"GET /api/v2/users":                      {"List users with optional search, status, and group filters.", "分页列出用户，支持按关键字、状态和用户组筛选。"},
	"POST /api/v2/users":                     {"Create a user account with a POSIX UID, primary POSIX group, supplementary groups, and roles.", "创建用户账号，分配 POSIX UID、主 POSIX 组、附加组及角色。"},
	"GET /api/v2/users/{id}":                 {"Retrieve a single user's profile, groups, and roles.", "获取指定用户的详细资料、所属组与角色。"},
	"PATCH /api/v2/users/{id}":               {"Update a user account ranked lower than the caller.", "修改等级低于调用者的用户资料、用户组或角色。"},
	"DELETE /api/v2/users/{id}":              {"Delete a user ranked lower than the caller and revoke their active certificates.", "删除等级低于调用者的用户，并同步吊销其所有有效证书。"},
	"PUT /api/v2/users/{id}/status":          {"Enable or disable a user account; disabling automatically revokes active certificates.", "启用或禁用用户账号；禁用时自动吊销其有效证书。"},
	"POST /api/v2/users/{id}/password-reset": {"Generate a temporary random password for a user and return it once.", "为用户生成随机重置密码并仅返回一次。"},
	"GET /api/v2/groups":                     {"List all POSIX groups and organizational units.", "列出所有 POSIX 用户组与组织架构节点。"},
	"POST /api/v2/groups":                    {"Create a group; POSIX groups receive a GID and optional home directory / shell presets.", "创建用户组；POSIX 组自动分配 GID 并支持配置主目录与 Shell 预设。"},
	"GET /api/v2/groups/{id}":                {"Retrieve a group's details and member list.", "获取指定用户组的详情及成员列表。"},
	"PATCH /api/v2/groups/{id}":              {"Update a group's description, home root, or login shell preset.", "更新用户组的备注、主目录根路径或默认 Shell 预设。"},
	"DELETE /api/v2/groups/{id}":             {"Delete a group.", "删除指定的用户组。"},
	"POST /api/v2/groups/{id}/members":       {"Atomically add and remove group members in a single batch.", "在单次批处理操作中原子化添加与移除组成员。"},
	"GET /api/v2/roles":                      {"List all RBAC roles ordered by rank.", "按等级（Rank）顺序获取所有角色列表。"},
	"POST /api/v2/roles":                     {"Create a new role ranked strictly below the caller's best rank.", "创建等级低于调用者自身最高等级的新角色。"},
	"PATCH /api/v2/roles/{id}":               {"Update a role ranked below the caller.", "修改等级低于调用者的角色信息。"},
	"DELETE /api/v2/roles/{id}":              {"Delete a role ranked below the caller.", "删除等级低于调用者的角色。"},
	"GET /api/v2/roles/{id}/permissions":     {"List the `(object, action)` permission grants assigned to a role.", "获取指定角色拥有的 `(对象, 操作)` 权限列表。"},
	"PUT /api/v2/roles/{id}/permissions":     {"Replace a role's permissions (callers may only grant permissions they hold themselves).", "替换角色的权限集合（调用者只能授予自身已拥有的权限）。"},
	// pki
	"GET /api/v2/pki/cas":                        {"List Certificate Authorities (private keys are never returned).", "列出所有证书颁发机构（绝不返回私钥内容）。"},
	"POST /api/v2/pki/cas":                       {"Generate or import an SSH or X.509 CA; seals the private key with `PKI_MASTER_KEY`.", "生成或导入 SSH / X.509 CA，并使用 `PKI_MASTER_KEY` 加密封存私钥。"},
	"POST /api/v2/pki/cas/{id}/revocations":      {"Revoke a certificate serial number under the specified CA.", "在指定 CA 下吊销对应序列号的证书。"},
	"GET /api/v2/me/certificate":                 {"Retrieve metadata for the signed-in user's active certificate.", "查看当前登录用户持有的有效证书信息。"},
	"POST /api/v2/me/certificate":                {"Issue a new certificate and private key for the signed-in user (private key returned once).", "为当前登录用户签发新证书与私钥（私钥仅在响应中返回一次）。"},
	"POST /api/v2/users/{id}/certificate":        {"Issue an initial certificate for a user under a chosen CA.", "在指定 CA 下为目标用户签发首张证书。"},
	"POST /api/v2/users/{id}/certificate/rotate": {"Rotate a user's certificate and revoke the superseded serial.", "轮换用户证书并自动吊销旧证书序列号。"},
	"DELETE /api/v2/users/{id}/certificate":      {"Revoke a user's active certificate.", "吊销目标用户的当前有效证书。"},
	// mail
	"GET /api/v2/mail/kinds":                  {"List mail provider drivers compiled into this build (`smtp`, `rest`, `pool`).", "列出当前构建支持的邮件服务商类型（`smtp`、`rest`、`pool`）。"},
	"GET /api/v2/mail/providers":              {"List configured mail providers (secrets are redacted).", "列出已配置的邮件服务商（密钥字段已脱敏）。"},
	"PUT /api/v2/mail/providers/{name}":       {"Create or update a mail provider and seal its credentials.", "创建或更新邮件服务商，并加密封存其凭据。"},
	"DELETE /api/v2/mail/providers/{name}":    {"Delete a mail provider.", "删除指定的邮件服务商。"},
	"POST /api/v2/mail/providers/{name}/test": {"Send a test email through the specified provider.", "通过指定邮件服务商发送测试邮件。"},
	"GET /api/v2/mail/usage":                  {"Query daily email delivery counts broken down by provider and purpose.", "按天、服务商与业务用途查询邮件投递统计。"},
	// cfaccess
	"GET /api/v2/auth/access":          {"Check whether a verified Cloudflare Access identity is present on the request.", "检查当前请求是否携带已验证的 Cloudflare Access 身份。"},
	"POST /api/v2/auth/access/session": {"Exchange a verified Cloudflare Access JWT for a portal session token (HTTPS required).", "使用已验证的 Cloudflare Access JWT 换取门户会话令牌（仅限 HTTPS）。"},
	// cluster-status
	"GET /api/v2/cluster-status":   {"Return the latest node state summary for every cluster.", "获取所有集群的最新节点状态汇总。"},
	"POST /sync/v2/cluster-status": {"Receive a cluster node status report from a slave agent.", "接收 slave 代理上报的集群节点状态。"},
	// current-status
	"GET /api/v2/current-status/links": {"Return short-lived presigned S3/R2 URLs for reading each cluster's live status object.", "获取各集群实时状态对象的短期预签名 S3/R2 读取链接。"},
	"PUT /sync/v2/current-status":      {"Receive a combined live-status snapshot from a slave agent and write it to R2/S3.", "接收 slave 代理上报的综合实时状态快照并条件写入 R2/S3。"},
	// diagnostics
	"GET /api/v2/diagnostics": {"Return a live health report across the database, cluster agents, gateways, and mail providers.", "返回数据库、集群同步代理、Web 网关与邮件服务商的实时健康诊断报告。"},
	// job-notification
	"GET /api/v2/notification/config":   {"Retrieve job completion email settings and template.", "获取作业完成邮件通知的配置与模板。"},
	"PUT /api/v2/notification/config":   {"Update job completion email settings and template.", "保存作业完成邮件通知的配置与模板。"},
	"POST /api/v2/notification/preview": {"Render the job completion email template with sample job data.", "使用示例作业数据渲染作业邮件模板预览。"},
	"GET /api/v2/notification/sample":   {"Return the latest completed job payload (or synthetic sample) and allowed template variables.", "获取最近一次真实完成作业的数据（或内置示例）及可用模板变量。"},
	"POST /api/v2/notification/test":    {"Send a test job completion email.", "发送测试作业完成邮件。"},
	"POST /sync/v2/job-completions":     {"Receive a finished job report from a slave agent and dispatch an email to the job owner.", "接收 slave 代理上报的已完成作业事件并向作业所有者发送通知邮件。"},
	// mail-pool
	"GET /api/v2/mail-pool/usage": {"Return current quota usage across member providers in each mail pool.", "查询各邮件池内成员服务商在当前周期的配额使用情况。"},
	// oauth
	"GET /api/v2/oauth/connectors/public":   {"List enabled OAuth connectors shown on the sign-in page.", "列出登录页展示的已启用 OAuth 登录连接器。"},
	"GET /api/v2/oauth/connectors":          {"List OAuth connectors and their ordered access rules.", "列出所有 OAuth 连接器及其有序准入规则。"},
	"POST /api/v2/oauth/connectors":         {"Create an OAuth connector.", "创建 OAuth 连接器。"},
	"PUT /api/v2/oauth/connectors/{id}":     {"Update an OAuth connector and its rules.", "更新 OAuth 连接器及其准入规则。"},
	"DELETE /api/v2/oauth/connectors/{id}":  {"Delete an OAuth connector.", "删除指定的 OAuth 连接器。"},
	"GET /api/v2/oauth/authorize/{id}":      {"Initiate OAuth sign-in: bind a CSRF state cookie and redirect to the identity provider.", "发起 OAuth 登录：绑定防伪 state Cookie 并重定向至身份提供商。"},
	"GET /api/v2/oauth/callback/{provider}": {"Handle OAuth callback: evaluate connector rules, provision or match the user, and issue a session token.", "处理 OAuth 回调：匹配连接器规则，创建或关联用户并签发会话令牌。"},
	// resource-monitor
	"GET /api/v2/resource-monitor/stats":    {"Return 7-day site-wide GPU utilization aggregated by day and by user.", "获取全站近 7 天按天与按用户汇总的 GPU 资源使用统计。"},
	"GET /api/v2/resource-monitor/my-stats": {"Return the signed-in user's 7-day GPU utilization.", "获取当前登录用户近 7 天的 GPU 资源使用统计。"},
	"POST /sync/v2/resource-summaries":      {"Receive a per-job GPU utilization summary from a slave agent.", "接收 slave 代理上报的单作业 GPU 使用摘要。"},
	// resource-status
	"GET /api/v2/resource-status":   {"Return the latest CPU/memory/GPU capacity and job queue for every cluster.", "获取各集群最新的 CPU/内存/GPU 容量与作业队列状态。"},
	"POST /sync/v2/resource-status": {"Receive a cluster resource and job queue report from a slave agent.", "接收 slave 代理上报的集群资源与作业队列状态。"},
	// web-access
	"GET /api/v2/web-access/clusters":                       {"List clusters where the signed-in user is permitted to open a web terminal or file browser.", "列出当前用户有权打开 Web 终端或文件管理器的集群。"},
	"GET /api/v2/web-access/config":                         {"Retrieve per-cluster Web Access Gateway settings and registration status.", "获取各集群的 Web 访问网关配置与注册状态。"},
	"PUT /api/v2/web-access/config":                         {"Update per-cluster Web Access Gateway connection settings and allowed roles.", "保存各集群的 Web 访问网关连接参数与允许访问的角色。"},
	"GET /api/v2/web-access/keys":                           {"Return the Ed25519 public keys gateways use to verify session tickets.", "返回供网关校验会话票据的 Ed25519 公钥列表。"},
	"POST /api/v2/web-access/ticket":                        {"Issue a 5-minute Ed25519-signed gateway ticket scoped to a user, cluster, and action.", "签发绑定用户、集群与操作权限范围的 5 分钟 Ed25519 签名网关票据。"},
	"POST /api/v2/web-access/ssh-certificate":               {"Sign a 5-minute SSH user certificate for an ephemeral WebCrypto public key generated in the browser.", "为浏览器端 WebCrypto 生成的临时公钥签发 5 分钟有效期的 SSH 用户证书。"},
	"GET /api/v2/web-access/transfer-notification":          {"Retrieve file transfer completion email settings and template.", "获取跨集群文件传输完成邮件的配置与模板。"},
	"PUT /api/v2/web-access/transfer-notification":          {"Update file transfer completion email settings and template.", "保存跨集群文件传输完成邮件的配置与模板。"},
	"POST /api/v2/web-access/transfer-notification/preview": {"Render the file transfer email template with sample data.", "使用示例数据渲染文件传输邮件模板预览。"},
	"GET /api/v2/web-access/transfer-notification/sample":   {"Return the latest file transfer event (or sample) and allowed template variables.", "获取最近一次文件传输事件（或示例）及可用模板变量。"},
	"POST /api/v2/web-access/transfer-notification/test":    {"Send a test file transfer notification email.", "发送测试文件传输完成通知邮件。"},
	"POST /sync/v2/web-access/register":                     {"Receive a heartbeat registration and capability advertisement from a Web Access Gateway.", "接收 Web 访问网关的心跳注册及其支持的功能列表。"},
	"POST /sync/v2/web-access/events":                       {"Receive a completed cross-cluster `rsync` transfer event from a gateway.", "接收网关上报的跨集群 `rsync` 传输完成事件。"},
	// replication
	"GET /sync/v2/snapshot":      {"Download the full identity and trust snapshot (`X-Data-Hash` carries its SHA-256 digest).", "下载完整的身份与信任快照（响应头 `X-Data-Hash` 携带其 SHA-256 摘要）。"},
	"GET /sync/v2/snapshot-hash": {"Return only the current snapshot SHA-256 digest for deployments that do not use DNS TXT polling.", "仅返回当前快照的 SHA-256 摘要，供未启用 DNS TXT 轮询的部署使用。"},
}
