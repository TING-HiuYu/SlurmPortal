# Web 访问网关

**Web 访问网关**（`cmd/gateway`）运行在集群登录节点（`login`）的 Nginx 反向代理后方，为用户提供浏览器终端（`xterm.js`）、SFTP 文件浏览器与跨集群后台 `rsync` 文件传输功能。

每个网关会话均使用浏览器通过 WebCrypto 生成的 5 分钟有效临时 SSH 证书，以当前登录用户的身份建立原生 SSH 连接——因此集群上的 Unix 文件权限、用户组与磁盘配额与普通 SSH 登录完全一致。架构设计详见[集群代理程序：Web 访问网关](../dev/agents.md#web-access-gateway)。

**执行节点：** 主服务与 `login`。**预计用时：** 25 分钟。

## 1. 在主服务上授权网关 {#1-authorize-the-gateway-on-the-master}

网关通过客户端证书连接主服务的 `/sync/*` 接口，并使用 Ed25519 密钥对（`PLUGIN_WEB_ACCESS_TICKET_KEY`）校验浏览器会话票据。

### 容器主服务 {#container-master}

在主服务宿主机的 `deploy/compose/master` 目录下，为 `cluster1-login` 签发客户端证书并启用 `web-access` 插件：

```sh
sh ../../certs/issue.sh client secrets/certs cluster1-login
FP=$(openssl x509 -in secrets/certs/clients/cluster1-login.crt -outform DER | sha256sum | cut -d' ' -f1)
cat >> .env <<ENV
PLUGIN_WEB_ACCESS_TICKET_KEY=$(openssl rand -base64 32)
WEB_ACCESS_CONFIG={"ticket-key-id":"web-access-1","gateways":[{"fingerprint":"$FP","cluster-id":"cluster1","gateway-id":"cluster1-login","instance-id":"cluster1-login"}]}
ENV
sed -i "s/^PLUGINS_ENABLED=.*/&,web-access/" .env
sed -i "s/^SYNC_CLIENT_FINGERPRINTS=.*/&,$FP/" .env
docker compose up -d
```

将 `secrets/certs/clients/cluster1-login.crt`、`cluster1-login.key` 与 `secrets/certs/ca.crt` 复制到 `login` 节点。

### Serverless 主服务 {#serverless-master}

1. 在 Cloudflare 控制台（**SSL/TLS → Client Certificates**）为网关签发一张客户端证书并计算其 SHA-256 指纹。
2. 将票据签名种子（32 字节随机数的 Base64）写入 Worker Secret `PLUGIN_WEB_ACCESS_TICKET_KEY`。
3. 在 `wrangler.toml` 中将 `web-access` 加入 `PLUGINS_ENABLED`，将网关证书指纹加入 `SYNC_CLIENT_FINGERPRINTS`，并在 `PLUGINS_CONFIG` 中配置 `web-access` 对象后重新发布。

### 获取票据校验公钥 {#retrieve-the-public-ticket-verification-key}

以管理员身份登录门户，访问 `GET /api/v2/web-access/keys`，复制密钥 `web-access-1` 的 `x` 字段值（Base64URL 编码的 Ed25519 公钥），供下一步写入网关配置。

## 2. 在 `login` 节点安装并配置网关 {#2-install-and-configure-the-gateway-on-login}

将在[集群 Slave 代理](slaves.md#1-build-the-programs)中编译好的 `gateway` 二进制复制到 `login` 节点，创建 `web-access-gateway` 系统用户并安装双向 TLS 证书：

```sh
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin web-access-gateway
install -d -m 0755 /usr/local/libexec/go-ldap-admin
install -m 0755 gateway /usr/local/libexec/go-ldap-admin/web-access-gateway
# Container master only: trust the master's private sync CA
cp ca.crt /usr/local/share/ca-certificates/slurm-portal-sync-ca.crt && update-ca-certificates

D=/etc/go-ldap-admin/web-access-gateway
install -d -m 0750 -o root -g web-access-gateway $D
install -m 0640 -o root -g web-access-gateway cluster1-login.crt $D/master-client.crt.pem
install -m 0600 -o web-access-gateway -g web-access-gateway cluster1-login.key $D/master-client.key.pem
```

网关在本地回环地址（`127.0.0.1:19934`）上启用 TLS 监听，由前置 Nginx 负责终结公网 `443` 端口的 HTTPS。将本地上游 TLS 证书与私钥放置在 `$D/https.crt.pem` 与 `$D/https.key.pem`（属主为 `web-access-gateway`，私钥权限 `0600`），然后编写 `/etc/go-ldap-admin/web-access-gateway.yaml`：

```yaml title="/etc/go-ldap-admin/web-access-gateway.yaml"
gateway_id: cluster1-login
instance_id: cluster1-login
cluster_id: cluster1
listen: "127.0.0.1:19934"
tls_certificate: /etc/go-ldap-admin/web-access-gateway/https.crt.pem
tls_key: /etc/go-ldap-admin/web-access-gateway/https.key.pem
callback_listen: "127.0.0.1:19935"
allowed_origins:
  - https://portal.example.org
ticket_public_keys:
  web-access-1: <the x value from /api/v2/web-access/keys>
user_ca_keys_file: /srv/slurm-portal-trust/current/ssh/trusted-user-ca-keys.pub
file_root: /home
clusters:
  - id: cluster1
    ssh_address: "127.0.0.1:22"
state_directory: /var/lib/go-ldap-admin-web-access-gateway
master_origin: https://portal.example.org:8443
master_client_certificate: /etc/go-ldap-admin/web-access-gateway/master-client.crt.pem
master_client_key: /etc/go-ldap-admin/web-access-gateway/master-client.key.pem
```

*（对于 Serverless 主服务，将 `master_origin` 设为 `https://sync.portal.example.org`。）*

启动 systemd 服务并验证网关已向主服务成功注册：

```sh
chown web-access-gateway:web-access-gateway /etc/go-ldap-admin/web-access-gateway.yaml
chmod 0600 /etc/go-ldap-admin/web-access-gateway.yaml
cp /root/SlurmPortal/deploy/gateway/web-access-gateway.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now web-access-gateway
curl -sk https://127.0.0.1:19934/readyz        # returns {"code":0,"data":{"status":"ready"}}
```

## 3. 配置 Nginx 提供公网 HTTPS 与 WebSocket 代理 {#3-configure-nginx-for-public-https-and-websockets}

用户浏览器直接连接 `https://login.example.org`。请参考 [`deploy/gateway/nginx.conf.example`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/gateway/nginx.conf.example) 在 `login` 上配置 Nginx 与受信任的 TLS 证书，并将流量反向代理至 `https://127.0.0.1:19934`：

- 务必在 Nginx 的 CORS 映射中加入门户域名 `https://portal.example.org`（否则浏览器预检请求会报 `"Failed to fetch"`）。
- 为 `/v1/sessions/*/terminal` 开启 WebSocket `Upgrade` 与 `Connection` 头支持。
- 可使用 [`deploy/gateway/acme/`](https://github.com/TING-HiuYu/SlurmPortal/tree/main/deploy/gateway/acme) 自动续期 Let's Encrypt DNS-01 证书，或使用 [`deploy/gateway/container/`](https://github.com/TING-HiuYu/SlurmPortal/tree/main/deploy/gateway/container) 以单容器方式同时运行网关与 Nginx。

## 4. 在门户中启用集群 Web 访问 {#4-enable-web-access-in-the-portal}

在门户管理后台打开 **集群 Web 访问（Cluster web access）**，配置 `cluster1`：

1. 将 **网关地址（Gateway URL）** 设为 `https://login.example.org`。
2. 选择您的 **SSH CA**。
3. 设置目标 **SSH 主机与端口**（`127.0.0.1` 与 `22`）。
4. 勾选允许使用 Web 终端与文件浏览器的 **角色（Allowed roles）**。

保存后，集群 Web 访问状态将变为 **就绪（Ready）**，用户工作台侧边栏随即显示 **Web 终端** 与 **文件管理**。

## 验证是否生效 {#check-it-works}

以普通门户用户登录，打开 **工作台 → Web 终端** 并选择 `cluster1`，即可在 `/home/<主组>/<用户名>` 下打开交互式终端；打开 **文件管理** 即可通过 SFTP 浏览与下载文件。

| 常见问题 | 排查方法 |
| --- | --- |
| `/readyz` 返回 `registration_unavailable` | 网关无法访问 `master_origin`，或其证书指纹未加入主服务白名单 |
| `/readyz` 返回 `config_mismatch` | `web-access-gateway.yaml` 中的 ID 与主服务 `WEB_ACCESS_CONFIG` / `PLUGINS_CONFIG` 不一致 |
| 浏览器提示 `"Failed to fetch"` | Nginx CORS 映射未包含门户域名，或 `login.example.org` 的 TLS 证书不受浏览器信任 |
| `credential_invalid` / `ticket_invalid` | **集群 Web 访问**中选定的 SSH CA 未包含在 `user_ca_keys_file` 中，或 `ticket_public_keys` 的密钥 ID 不匹配 |

## 下一步 {#next}

继续阅读[端到端验证](verify.md)。
