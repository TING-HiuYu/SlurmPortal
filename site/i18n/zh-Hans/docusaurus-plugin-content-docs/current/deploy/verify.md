# 端到端验证

本页通过创建一个测试用户（`alice`），完整检验前序章节搭建的所有环节。每一步都明确说明了它所验证的系统能力，跑完整个流程大约需要 5 分钟。

## 1. 准备门户基础配置 {#1-prepare-the-portal}

以管理员身份登录门户，确认已完成[门户初始化配置](portal-setup.md)中的两项基础设置：

1. **证书颁发机构：** 确认已有一个启用状态的 **SSH (`ssh`)** CA。
2. **用户组：** 确认已创建名为 `research` 的 POSIX 组，其主目录根路径为 `/home/research`，默认 Shell 为 `/bin/bash`。
3. *(可选)* 如需为该组提供专属共享目录，可在 `infra` 上创建 `mkdir -p /srv/nfs/home/research/shared`（未创建时 `~/SHARED` 默认链接至 `/home/shared`）。

## 2. 创建测试用户 {#2-create-a-user}

在 **用户管理（Users）** 中创建用户 `alice`，主 POSIX 组选 `research` 并设置密码。

等待约 1 分钟后，在 `ctl` 上执行以下检查：

```sh
curl -s 127.0.0.1:8081/readyz; echo                  # "ready":true, a new hash
getent passwd alice                                    # from LDAP, through sssd
sacctmgr -nP show assoc where user=alice format=cluster,account,user
ls -ld /home/research/alice                            # made by the trigger
```

**本步验证：** Slave 已成功从主服务拉取新快照并写入 OpenLDAP，SSSD 已能解析该账号，且触发器已自动创建其 Slurm 记账关联与 NFSv4 主目录。

## 3. 使用 SSH 证书登录 {#3-log-in-with-a-certificate}

在 **用户管理** 中打开 `alice`，在 SSH CA 下为其签发一张证书，将页面**仅展示一次**的私钥与证书分别保存为本地的 `~/.ssh/alice` 与 `~/.ssh/alice-cert.pub`（普通用户也可在 **证书与凭据** 页面自助签发）：

```sh
chmod 600 ~/.ssh/alice
ssh -i ~/.ssh/alice -o CertificateFile=~/.ssh/alice-cert.pub alice@login.example.org
```

登录到 `login` 节点后执行：

```sh
id                       # uid=10000(alice) gid=10000(research)
pwd                      # /home/research/alice
srun -N1 hostname        # node1
sacct -X --format=JobID,User,Account,State
```

**本步验证：** `login` 上的 `sshd` 已通过共享信任目录信任门户 CA，NFS 主目录挂载正常，且 `alice` 可以在自己的 Slurm 账户下成功运行作业。

## 4. 查看实时集群状态 {#4-live-status}

在门户中打开 **当前资源（Current resources）** 与 **集群状态（Cluster status）**，可以看到 `cluster1` 的节点状态、空闲 CPU/内存以及作业队列。以 `alice` 身份提交 `sbatch --wrap "sleep 120"`，1 分钟内即可在页面队列中看到该作业。

**本步验证：** Slave 的状态采集器已正常上报至主服务与对象存储，且浏览器能够通过预签名链接直接读取监控数据。

## 5. 验证证书吊销 {#5-revocation}

在 **用户管理** 中吊销 `alice` 的证书。等待约 1 分半钟（一次 slave 轮询加信任目录挂载点的 5 秒属性缓存）后，再次执行相同的 `ssh` 登录命令：

```text
alice@login.example.org: Permission denied (publickey).
```

此时登录被立即拒绝，且 `login` 上的 `journalctl -u ssh` 会记录 `revoked by file …/revoked.krl`。

**本步验证：** 证书吊销无需人工登录节点操作即可自动通过 KRL 同步至所有 `sshd` 并强制生效。

## 6. 验证可选扩展功能 {#6-optional-parts}

如果您部署了可选组件：

- **作业邮件与 GPU 监控：** 以 `alice` 身份提交 `sbatch --wrap "sleep 90"`；作业结束后，`alice` 会收到作业汇总邮件，且 **近期资源汇总** 页面会列出该作业。
- **Web 终端：** 以 `alice` 身份登录门户，打开 **Web 终端** 并选择 `cluster1`，即可直接在 `/home/research/alice` 下打开交互式 Shell。

## 7. 清理测试账号 {#7-clean-up}

在门户中删除用户 `alice`。在下一轮同步中，slave 会将其从 OpenLDAP 中移除，同时触发器会禁止该账户继续提交 Slurm 作业（`deletePolicy: disable`；若设为 `delete` 则直接移除关联）。其主目录 `/home/research/alice` 会保留在存储上，确认无需保留数据后手动清理即可。

## 故障排查速查表 {#when-something-fails}

| 现象 | 首要排查点 |
| --- | --- |
| 新建用户未出现在集群上 | 在 `ctl` 上查看 `journalctl -u slurm-portal-slave` |
| `getent` 查不到用户 | 执行 `sssctl domain-status example.org` 检查状态，或用 `sss_cache -E` 刷新缓存 |
| 缺少 Slurm 关联或主目录 | 查看 slave 日志中的触发器报错，并手动运行日志中的命令排查 |
| SSH 证书登录被拒绝 | 在 `login` 上查看 `journalctl -u ssh` 并检查 `ls /srv/slurm-portal-trust/current/ssh/` |
| 页面无实时状态数据 | 确认 `slave.yml` 中 `current-status.enabled: true`，并以 `slurm-portal` 用户测试 `scontrol ping` |

日常升级、备份与密钥轮换操作请参阅[日常运维指南](operations.md)。
