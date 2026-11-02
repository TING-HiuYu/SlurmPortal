# 共享主目录与信任文件

用户在登录节点与计算节点作业中需要访问同一个 `$HOME` 主目录，且各节点的 `sshd` 需要读取 slave 发布的信任文件。这两项需求可通过 `infra` 上的同一个 NFSv4 导出目录一并满足。

**执行节点：** `infra`（服务端）；`ctl`、计算节点、`login`（客户端）。**预计用时：** 15 分钟。

| `infra` 上的路径 | 挂载位置 | 在 `ctl` 上的权限 | 在其他节点上的权限 |
| --- | --- | --- | --- |
| `/srv/nfs/home` | `/home` | 读写，允许 `root`（`no_root_squash`） | 读写，映射 `root`（`root_squash`） |
| `/srv/nfs/home/.slurm-portal-trust` | `ctl` 上为 `/home/.slurm-portal-trust`；其他节点为 `/srv/slurm-portal-trust` | 由 slave 写入 | 只读挂载，5 秒属性缓存（`actimeo=5`） |

`ctl` 拥有更高权限是因为 slave 运行在该节点上：其触发器需要以 `root` 身份创建用户主目录，且 slave 进程需要写入信任文件。

## 主目录结构说明 {#home-layout}

标准触发器会将每位用户的主目录创建为 `/home/<主组名称>/<用户名>`，属主为该用户，权限为 `0700`。在门户中创建可用作主组的 POSIX 组时，请将其主目录根路径设为 `/home/<组名>`。各组专属的共享目录（`/home/<组名>/shared`）或全局共享目录（`/home/shared`）会自动以符号链接 `~/SHARED` 链入每个用户的主目录。

## 1. 配置 NFS 服务端 {#1-the-server}

Slave 在 `ctl` 上以 `slurm-portal` 系统用户运行，信任目录在 NFS 服务端必须归属该用户的**数值型 UID**。在 `ctl` 上选择一个未被占用的 UID（本指南使用 `990`，可先用 `getent passwd 990` 确认无输出）并创建该系统用户：

```sh
useradd --system --uid 990 --user-group --home-dir /var/lib/slurm-portal --shell /usr/sbin/nologin slurm-portal
```

在 `infra` 上安装 NFS 服务端，创建共享目录并将 `.slurm-portal-trust` 的属主设为 `990:990`：

```sh
apt install -y nfs-kernel-server
mkdir -p /srv/nfs/home/shared /srv/nfs/home/.slurm-portal-trust
chown 990:990 /srv/nfs/home/.slurm-portal-trust
```

编写 `/etc/exports`（请将 `10.0.0.0/24` 替换为您自己的集群网段）：

```text title="/etc/exports"
/srv/nfs/home  ctl(rw,fsid=0,sync,no_subtree_check,no_root_squash) 10.0.0.0/24(rw,fsid=0,sync,no_subtree_check)
```

```sh
exportfs -ra
exportfs -v
```

`fsid=0` 将该目录设为 NFSv4 根导出点，客户端直接挂载 `infra:/` 即可。

:::note
请仅导出 `/srv/nfs/home` 这一条路径。若对同一文件系统的父目录与子目录分别配置不同导出选项（例如只读子目录），Linux NFS 服务端在某些请求下会错误套用父目录选项。
:::

## 2. 配置 NFS 客户端 {#2-the-clients}

在 `ctl`、所有计算节点与 `login` 上安装客户端工具：

```sh
apt install -y nfs-common
```

在 `ctl` 上仅需挂载 `/home`（slave 直接通过 `/home/.slurm-portal-trust` 写入信任文件）：

```text title="/etc/fstab (ctl)"
infra:/  /home  nfs4  rw,hard,_netdev  0 0
```

在**计算节点与 `login`** 上，除挂载 `/home` 外，再以只读、5 秒短缓存方式将信任目录挂载至 `/srv/slurm-portal-trust`：

```text title="/etc/fstab (compute nodes, login)"
infra:/                     /home                    nfs4  rw,hard,_netdev  0 0
infra:/.slurm-portal-trust  /srv/slurm-portal-trust  nfs4  ro,hard,actimeo=5,nosharecache,_netdev  0 0
```

```sh
mkdir -p /srv/slurm-portal-trust      # compute nodes and login only
systemctl daemon-reload
mount -a
findmnt -t nfs4
```

:::warning
挂载 `/home` 会遮蔽本地 `/home` 原有的内容。挂载前请先将本地管理员的主目录迁移至其他位置或确认本地 `/home` 为空。
:::

**为什么信任目录需要单独挂载：** NFS 客户端默认缓存文件属性长达 60 秒。通过 `actimeo=5,nosharecache` 单独挂载后，一旦在门户中吊销某张证书，slave 写出新吊销列表后数秒内即可在所有登录节点生效，同时不影响 `/home` 下普通作业文件的正常缓存性能。

## 验证是否生效 {#check-it-works}

在 `ctl` 上验证信任目录可由 `slurm-portal` 写入且 `no_root_squash` 生效：

```sh
sudo -u slurm-portal touch /home/.slurm-portal-trust/probe && echo "trust writable"
touch /home/probe && ls -l /home/probe     # owned by root: no_root_squash works
```

在 `login` 上验证文件可见、信任挂载点只读且 5 秒缓存参数已生效：

```sh
ls -l /srv/slurm-portal-trust/probe /home/probe
touch /srv/slurm-portal-trust/x            # "Read-only file system": expected
findmnt -n -o OPTIONS /srv/slurm-portal-trust | grep -o 'acregmax=5'
```

验证完毕后在 `ctl` 上清理探测文件：

```sh
rm /home/.slurm-portal-trust/probe /home/probe
```

| 常见问题 | 排查方法 |
| --- | --- |
| `access denied by server` | 客户端主机名或 IP 与 `/etc/exports` 不匹配：在 `infra` 上执行 `exportfs -v` 检查 |
| 在 `ctl` 上写入信任目录报 `Permission denied` | 服务端信任目录属主不是 `990`，或 `ctl` 上 `slurm-portal` 的 UID 不是 `990` |
| 在 `ctl` 上提示 `Read-only file system` | `infra` 未能将 `ctl` 解析为对应 IP，导致退回匹配了网段规则 |
| 文件属主显示为 `nobody` | 当前节点的 SSSD 未正常解析该用户：参见 [SSSD 与 SSH](sssd-ssh.md) |

## 下一步 {#next}

集群基础环境已准备就绪。接下来请部署门户主服务：
- [Serverless 主服务（Cloudflare）](master-saas.md)，或
- [容器主服务（Docker Compose）](master-container.md)。
