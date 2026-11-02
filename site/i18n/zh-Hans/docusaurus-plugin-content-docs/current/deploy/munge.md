# Munge

Slurm 各守护进程与命令行工具通过 **Munge** 在节点间相互认证：集群中的每台主机持有同一份共享密钥（`/etc/munge/munge.key`），在任一节点生成的认证凭据均可被其他节点校验通过。

**执行节点：** 所有集群节点（`ctl`、各计算节点、`login`）。**预计用时：** 5 分钟。

## 1. 安装软件包 {#1-install}

在所有集群节点上执行：

```sh
apt install -y munge
```

安装软件包会自动创建 `munge` 系统用户并在每台机器上生成一把随机密钥。由于各节点自动生成的密钥互不相同，下一步我们在 `ctl` 上生成统一密钥并分发至所有节点。

## 2. 生成统一密钥并分发至全集群 {#2-make-one-key-and-copy-it-everywhere}

在 `ctl` 上生成新密钥并设置属主与权限：

```sh
mungekey --create --force
chown munge:munge /etc/munge/munge.key
chmod 0400 /etc/munge/munge.key
systemctl restart munge
```

将密钥复制到其余所有集群节点，保持相同的属主与 `0400` 权限并重启服务：

```sh
for host in node1 login; do
  scp /etc/munge/munge.key root@$host:/etc/munge/munge.key
  ssh root@$host 'chown munge:munge /etc/munge/munge.key && chmod 0400 /etc/munge/munge.key && systemctl restart munge'
done
```

:::warning
`/etc/munge/munge.key` 是全集群共享的核心机密：任何持有该密钥的人都可以向 Slurm 冒充任意用户。请仅通过加密的 SSH 通道传输，切勿提交至代码仓库，并始终保持 `0400` 权限。
:::

## 验证是否生效 {#check-it-works}

验证在 `ctl` 上生成的凭据能否在计算节点与登录节点上成功解码：

```sh
munge -n | ssh node1 unmunge
munge -n | ssh login unmunge | grep STATUS
```

```text
STATUS:          Success (0)
ENCODE_HOST:     ctl (10.0.0.10)
```

| 报错信息 | 原因与排查方法 |
| --- | --- |
| `Invalid credential` | 两端密钥不一致：比对两台主机上的 `sha256sum /etc/munge/munge.key` |
| `Expired credential` / `Rewound credential` | 节点间存在时钟偏差：使用 `timedatectl` 检查 NTP 时间同步 |
| `Failed to access "/run/munge/munge.socket.2"` | `munged` 服务未运行：检查 `systemctl status munge` |

## 下一步 {#next}

继续阅读 [Slurm](slurm.md)。
