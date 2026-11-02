# slurmrestd

`slurmrestd` 是 Slurm 官方提供的 HTTP REST API 服务。Slurm Portal 通过它读取已结束作业的运行详情与 GPU 分配信息，用于发送作业完成邮件和统计近 7 天 GPU 资源用量。在集群中，只有部署在 `ctl` 本机的只读代理 **observer** 会访问它，因此只需监听本地回环地址 `127.0.0.1`。

**执行节点：** `ctl`。**预计用时：** 10 分钟。*（如果不需要作业完成邮件与 GPU 用量统计，可跳过本页。）*

## 1. 生成 Slurm JWT 密钥 {#1-a-jwt-key-for-slurm}

`slurmrestd` 使用由 `slurmctld` 与 `slurmdbd` 共享的 HS256 密钥验证 JSON Web Token（JWT）：

```sh
dd if=/dev/random of=/var/lib/slurm/slurmctld/jwt_hs256.key bs=32 count=1
chown slurm:slurm /var/lib/slurm/slurmctld/jwt_hs256.key
chmod 0600 /var/lib/slurm/slurmctld/jwt_hs256.key
```

在 `/etc/slurm/slurm.conf` 中添加以下两行：

```ini
AuthAltTypes=auth/jwt
AuthAltParameters=jwt_key=/var/lib/slurm/slurmctld/jwt_hs256.key
```

并将完全相同的两行也添加到 `/etc/slurm/slurmdbd.conf` 中。随后将 `slurm.conf` 分发至所有节点并重启服务：

```sh
for host in node1 login; do scp /etc/slurm/slurm.conf root@$host:/etc/slurm/; done
systemctl restart slurmdbd slurmctld
ssh root@node1 systemctl restart slurmd
```

该密钥仅保存在 `ctl` 上且权限必须为 `0600`——任何持有该密钥的人都可以签发任意 Slurm 用户的认证令牌。

## 2. 安装并启动 slurmrestd {#2-install-and-run-slurmrestd}

`slurmrestd` 严禁以 `root` 或 `slurm` 用户运行。请先创建专用的无特权系统用户 `slurmrestd`，再安装软件包：

```sh
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin slurmrestd
apt install -y /root/slurm-debs/slurm-smd-slurmrestd_*.deb
```

添加 systemd 覆盖配置，使其仅监听本地回环地址 `127.0.0.1:6820`（软件包默认监听所有网卡）：

```sh
mkdir -p /etc/systemd/system/slurmrestd.service.d
cat > /etc/systemd/system/slurmrestd.service.d/listen.conf <<'EOF'
[Service]
Environment=SLURMRESTD_LISTEN=127.0.0.1:6820
EOF
systemctl daemon-reload
systemctl enable --now slurmrestd
systemctl restart slurmrestd
```

## 验证是否生效 {#check-it-works}

以 `root` 身份生成一个 60 秒有效期的短期令牌并调用接口验证：

```sh
export $(scontrol token username=root lifespan=60)
curl -s -H "X-SLURM-USER-NAME: root" -H "X-SLURM-USER-TOKEN: $SLURM_JWT" \
  http://127.0.0.1:6820/slurm/v0.0.45/ping | grep -E '"hostname"|"responding"'
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:6820/slurm/v0.0.45/ping   # 401 without a token
```

```text
      "hostname": "ctl",
      "responding": true,
401
```

后续部署的门户观察者（`slurm-notify-observer`）会自动使用该密钥按需签发短期令牌，其他组件无需接触该密钥。

| 常见问题 | 排查方法 |
| --- | --- |
| `slurmrestd` 无法启动 | 查看 `journalctl -u slurmrestd`，确认 `slurmrestd` 系统用户已创建 |
| 携带令牌仍返回 `401` | 确认 `ctl` 上的 `slurm.conf` 与 `slurmdbd.conf` 均已添加 `AuthAltTypes=auth/jwt` 且已重启服务 |
| 访问 `v0.0.45` 返回 `404` | 当前安装的 Slurm 版本低于 26.05，请参阅 [Slurm 安装指南](slurm.md) |

## 下一步 {#next}

继续阅读 [OpenLDAP](openldap.md)。
