# 集群 Slave 代理

每个集群运行一个 **slave** 代理（`cmd/slave`），负责让集群与主服务保持同步：

- 将用户、用户组、成员关系与证书写入集群的 OpenLDAP；
- 写出供 `sshd` 读取的 SSH 信任文件（受信 CA 公钥与 KRL 吊销列表）；
- 在账号变更时执行本地触发器（如创建用户的 Slurm 关联账户与主目录）；
- 采集并上报节点、队列与资源实时状态。

同一个 slave 程序对两种主服务完全通用。本页在 `ctl` 上将其安装为原生 systemd 服务，以便直接执行触发器与状态采集命令。[在容器中运行 Slave](#in-a-container) 放在本页末尾介绍。

**执行节点：** `ctl`。**预计用时：** 20 分钟。

## 前置准备 {#what-you-need}

- 已按前序章节完成集群配置：[OpenLDAP](openldap.md)、[SSSD 与 SSH](sssd-ssh.md) 及[共享主目录与信任文件](shared-home.md)（包括创建 UID 为 `990` 的 `slurm-portal` 系统用户）。
- 从主服务获取当前集群的证书包：`client.crt`、`client.key`，容器主服务还包含 `master-ca.crt`：
  - **容器主服务：** 由 `./add-cluster.sh cluster1 "Cluster 1"` 生成在 `secrets/clusters/cluster1/` 下；
  - **Serverless 主服务：** 在 Cloudflare 控制台为该集群生成的客户端证书与私钥。
- 从 `ctl` 可正常访问主服务同步地址：`https://portal.example.org:8443`（容器主服务）或 `https://sync.portal.example.org`（Serverless 主服务）。

## 1. 编译可执行程序 {#1-build-the-programs}

在 `ctl` 上直接从源码编译静态二进制文件（Ubuntu 的 `golang-go` 会自动下载项目所需的精确 Go 版本）：

```sh
apt install -y golang-go git
git clone https://github.com/TING-HiuYu/SlurmPortal /root/SlurmPortal
cd /root/SlurmPortal
mkdir -p bin
for c in slave host-bridge slurm-lifecycle-hook slurm-notify-observer gateway; do
  CGO_ENABLED=0 go build -trimpath -o bin/$c ./cmd/$c
done
install -m 0755 bin/slave /usr/local/sbin/slurm-portal-slave
```

其余编译产出的二进制用于[作业通知与 GPU 监控](optional.md)及[Web 访问网关](gateway.md)。如果集群节点无法访问外网，可在任意同架构机器上编译后直接复制二进制文件（无任何外部动态库依赖）。

## 2. 部署密钥与配置文件 {#2-secrets-and-configuration}

将集群证书包复制到 `ctl` 的 `/root/cluster1`，并从 `infra` 获取 OpenLDAP 管理员密码：

```sh
install -d -m 0750 -o root -g slurm-portal /etc/slurm-portal
install -m 0640 -o root -g slurm-portal /root/cluster1/client.crt /root/cluster1/master-ca.crt /etc/slurm-portal/
install -m 0600 -o slurm-portal -g slurm-portal /root/cluster1/client.key /etc/slurm-portal/
scp root@infra:/root/ldap-admin-password /etc/slurm-portal/ldap-password
chown slurm-portal:slurm-portal /etc/slurm-portal/ldap-password
chmod 0600 /etc/slurm-portal/ldap-password
```

Slave 会拒绝读取权限过宽的私钥文件。接下来编写 `/etc/slurm-portal/slave.yml`：

```yaml title="/etc/slurm-portal/slave.yml"
# The master (container master shown; see the table below for serverless)
master-url: https://portal.example.org:8443
dns-name: ""
check-interval: 60s
cluster-id: cluster1
tls:
  cert-file: /etc/slurm-portal/client.crt
  key-file: /etc/slurm-portal/client.key
  ca-file: /etc/slurm-portal/master-ca.crt
  server-name: portal.example.org

# This cluster's OpenLDAP
ldap:
  url: ldap://infra.example.org
  start-tls: true
  server-name: infra.example.org     # the name in the LDAP server's certificate
  bind-dn: cn=admin,dc=example,dc=org
  password-file: /etc/slurm-portal/ldap-password
local-base-dn: dc=example,dc=org
local-group-dn: ou=groups,dc=example,dc=org

# What the slave writes
state-dir: /var/lib/slurm-portal
health-listen: 127.0.0.1:8081
trust-dirs:
  - /home/.slurm-portal-trust
trigger-file: /etc/slurm-portal/triggers.yaml
limits:
  max-user-deletions: 5
  allow-ca-removal: false

# Live node and queue status, read with scontrol and squeue
current-status:
  enabled: true
```

```sh
chown slurm-portal:slurm-portal /etc/slurm-portal/slave.yml
chmod 0600 /etc/slurm-portal/slave.yml
```

| 配置键 | Serverless 主服务 | 容器主服务 |
| --- | --- | --- |
| `master-url` | `https://sync.portal.example.org` | `https://portal.example.org:8443` |
| `dns-name` | `_sync.portal.example.org` | `""`（直接向主服务查询） |
| `tls.server-name` | `sync.portal.example.org` | `portal.example.org` |
| `tls.ca-file` | 不设置 | `/etc/slurm-portal/master-ca.crt` |

其余配置项说明见[集群代理配置参考](../dev/agents.md#configuration-reference)。`local-group-dn` 将用户组置于 SSSD 查找的 `ou=groups` 分支下。

## 3. 配置触发器：Slurm 账户与主目录 {#3-the-trigger-slurm-accounts-and-home-directories}

当新建用户时，`slurm_trigger.py` 会自动为其创建 Slurm 记账账户与关联，并初始化主目录 `/home/<主组>/<用户名>`；当用户被删除时，按策略禁止其继续提交作业（`disable`）或移除关联（`delete`）。触发器通过一条严格限定的 `sudo` 规则以 `root` 权限运行：

```sh
install -d -m 0755 /usr/local/libexec/go-ldap-admin
install -m 0755 /root/SlurmPortal/deploy/triggers/slurm_trigger.py /usr/local/libexec/go-ldap-admin/
install -d -m 0755 /etc/go-ldap-admin
cat > /etc/go-ldap-admin/slurm-trigger.json <<'EOF'
{
  "cluster": "cluster1",
  "deletePolicy": "disable",
  "homeRoot": "/home",
  "sharedRoot": "/home/shared",
  "storageCommands": [],
  "identityPrefix": ["/usr/bin/getent"],
  "sacctmgrPrefix": ["/usr/bin/sacctmgr"]
}
EOF
chmod 0600 /etc/go-ldap-admin/slurm-trigger.json

cat > /etc/sudoers.d/slurm-portal <<'EOF'
slurm-portal ALL=(root) NOPASSWD: /usr/local/libexec/go-ldap-admin/slurm_trigger.py --config /etc/go-ldap-admin/slurm-trigger.json *
EOF
chmod 0440 /etc/sudoers.d/slurm-portal
visudo -cf /etc/sudoers.d/slurm-portal

cat > /etc/slurm-portal/triggers.yaml <<'EOF'
version: 1
triggers:
  - id: slurm-account
    enabled: true
    events: [user.created, user.updated, user.deleted, membership.added, membership.removed]
    command:
      - /usr/bin/sudo
      - -n
      - /usr/local/libexec/go-ldap-admin/slurm_trigger.py
      - --config
      - /etc/go-ldap-admin/slurm-trigger.json
      - "%DATA%"
    timeoutSeconds: 120
EOF
chown slurm-portal:slurm-portal /etc/slurm-portal/triggers.yaml
chmod 0600 /etc/slurm-portal/triggers.yaml
```

触发器要求当前主机上的 `/home` 必须是 NFSv4 挂载点，且会自动忽略 UID 低于 `10000` 的系统账户。

## 4. 配置 systemd 服务 {#4-the-service}

```sh
cp /root/SlurmPortal/deploy/slave/slurm-portal-slave.service /etc/systemd/system/
mkdir -p /etc/systemd/system/slurm-portal-slave.service.d
cat > /etc/systemd/system/slurm-portal-slave.service.d/site.conf <<'EOF'
[Service]
# The trigger runs through sudo, and writes home directories; the trust folder is in /home.
NoNewPrivileges=false
ProtectHome=false
ReadWritePaths=
ReadWritePaths=/var/lib/slurm-portal /home
EOF
systemctl daemon-reload
```

## 5. 离线预演并启动服务 {#5-rehearse-then-start}

在首次正式启动前，先使用计划模式（`-plan`）离线预演首次同步将执行的所有操作。首先导出主服务数据库副本——对于容器主服务，在其宿主机上执行：

```sh
sqlite3 data/master/portal.db ".backup /tmp/portal-copy.db"
```

（对于 Serverless 主服务，执行 `npx wrangler@4 d1 export <database> --remote --output dump.sql`，再执行 `sqlite3 /tmp/portal-copy.db < dump.sql`。）将数据库副本复制到 `ctl` 的 `/root/portal-copy.db`，并在 `infra` 上导出当前 LDAP 目录至 `/root/cluster.ldif`：

```sh
# on infra
slapcat -n 1 > /root/cluster.ldif
```

将 `/root/cluster.ldif` 复制到 `ctl` 后执行预演：

```sh
slurm-portal-slave -plan -db file:/root/portal-copy.db -source-base-dn dc=example,dc=org \
  -ldif /root/cluster.ldif -local-base-dn dc=example,dc=org -local-group-dn ou=groups,dc=example,dc=org \
  -admin-dn cn=admin,dc=example,dc=org
```

报告会列出所有影响登录的操作（`loginOps`）以及安全防线是否通过：

- 在**全新空目录**上，预期会看到针对门户账号的 `add-user`、`add-group` 与 `add-member` 操作，且 `"guard": "pass"`。
- 在**迁移已有相同账号的存量目录**时，预期 `loginOps` 为空且 `"safe": true`；若出现非预期变更，请先排查原因再启动服务。

验证无误后删除临时副本文件（内含密码哈希），然后正式启动服务：

```sh
systemctl enable --now slurm-portal-slave
journalctl -u slurm-portal-slave -f        # "sync check failed" lines say what is wrong; Ctrl-C to stop
```

门户中的变更将在约 1 分钟内（`check-interval`）同步至集群。

## 将 sshd 指向信任文件 {#point-sshd-at-the-trust-files}

首次同步完成后，信任目录下会生成 `current` 世代。在 `login` 节点上配置 `sshd`：

```sh
ls /srv/slurm-portal-trust/current/ssh/
cat > /etc/ssh/sshd_config.d/50-slurm-portal.conf <<'EOF'
TrustedUserCAKeys /srv/slurm-portal-trust/current/ssh/trusted-user-ca-keys.pub
RevokedKeys /srv/slurm-portal-trust/current/ssh/revoked.krl
EOF
sshd -t && systemctl reload ssh
```

`current` 始终指向完整的不可变世代目录，因此 `sshd` 绝不会读到写了一半的文件。

:::warning
如果 `RevokedKeys` 指定的文件不存在，`sshd` 会拒绝**所有**公钥登录（包括管理员自己的公钥）。修改 `sshd` 配置时请务必保持一个已登录的 `root` 终端窗口打开。
:::

## 验证是否生效 {#check-it-works}

在 `ctl` 上检查就绪状态及门户测试用户（例如主组为 `research` 的 `alice`）：

```sh
curl -s 127.0.0.1:8081/readyz; echo
getent passwd alice
sacctmgr -nP show assoc where user=alice format=cluster,account,user
ls -ld /home/research/alice
```

```text
{"ready":true,"hash":"5630…","dirty":false,"lastOk":"…"}
alice:*:10000:10000:alice:/home/research/alice:/bin/bash
cluster1|alice|alice
drwx------ 2 alice research 4096 … /home/research/alice
```

| 常见问题 | 排查方法 |
| --- | --- |
| `/readyz` 显示未就绪 | 执行 `journalctl -u slurm-portal-slave` 查看具体失败步骤 |
| `x509: certificate signed by unknown authority` | 检查 `tls.ca-file`（容器主服务）或系统 CA 证书包 |
| `remote error: tls: unknown certificate` | 主服务未识别该客户端证书：检查主服务的 `CLUSTERS` 与 `SYNC_CLIENT_FINGERPRINTS` |
| LDAP 报 `Invalid credentials` | 检查密码文件内容（是否含多余换行）或 `bind-dn` |
| 创建用户后首次触发器报 `exit status 1` | 正常现象：SSSD 尚未刷新出新账号，下一轮检查会自动重试成功 |
| 触发器持续失败 | 手动执行日志中打印的命令查看具体报错，排查 NSS、NFS 挂载、`sudo` 或 Slurm 状态 |

## 升级 Slave {#updating}

```sh
cd /root/SlurmPortal && git pull
CGO_ENABLED=0 go build -trimpath -o bin/slave ./cmd/slave
install -m 0755 bin/slave /usr/local/sbin/slurm-portal-slave
systemctl restart slurm-portal-slave
```

Slave 重启后会从已保存的状态恢复，除非快照发生变化，否则不会向 LDAP 重复写入。

## 在容器中运行 Slave {#in-a-container}

[`deploy/compose/slave`](https://github.com/TING-HiuYu/SlurmPortal/tree/main/deploy/compose/slave) 支持通过 Docker Compose 运行同一个 slave：

```sh
cd deploy/compose/slave
./init.sh                       # .env, data folders, slave.yml from the example
cp /path/to/bundle/client.crt /path/to/bundle/client.key secrets/
cp /path/to/bundle/master-ca.crt secrets/            # container master only
printf '%s' 'the LDAP admin password' > secrets/ldap-password
chmod 600 secrets/*
docker compose up -d
```

将 `.env` 中的 `TRUST_DIR` 设为共享信任目录路径。与宿主机原生安装相比有两点不同：

- **触发器：** 容器内无法直接调用宿主机的 `sacctmgr`，需改用主机桥（`host-bridge`）通过 SSH 将事件交由宿主机上的 `slurm_trigger.py` 执行：详见 [`deploy/triggers`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/triggers/README.md#slave-in-a-container)。
- **实时状态采集：** 需要在容器镜像内安装 `scontrol`、`squeue`、`slurm.conf` 并挂载 Munge 套接字（可基于 [`slave.Dockerfile`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/docker/slave.Dockerfile) 扩展）。

该 Compose 编排还内置了一个演示用 OpenLDAP（`docker compose --profile demo up -d`），方便在没有真实集群的情况下快速体验同步流程。

## 下一步 {#next}

继续阅读可选集群组件：
- [作业通知与 GPU 监控](optional.md)
- [Web 访问网关](gateway.md)
- 或直接跳至[端到端验证](verify.md)。
