# 记账服务

Slurm 记账服务（`slurmdbd`）负责记录每一笔作业历史，并强制校验用户是否有权在指定账户下提交作业。Slurm Portal 依赖该服务：标准触发器会为每位门户用户自动创建对应的 Slurm 记账账户与关联（Association），作业完成通知也会从中读取已结束的作业记录。

**执行节点：** `ctl`。**预计用时：** 10 分钟。

## 1. 部署 MariaDB 数据库 {#1-database}

在 `ctl` 上安装 MariaDB 并创建记账数据库与专用账户：

```sh
apt install -y mariadb-server
systemctl enable --now mariadb

SLURMDB_PW=$(openssl rand -hex 16)
mysql <<SQL
CREATE DATABASE slurm_acct_db;
CREATE USER 'slurm'@'localhost' IDENTIFIED BY '$SLURMDB_PW';
GRANT ALL ON slurm_acct_db.* TO 'slurm'@'localhost';
SQL
```

应用 SchedMD 推荐的 InnoDB 缓冲池与锁超时参数：

```ini title="/etc/mysql/mariadb.conf.d/60-slurmdbd.cnf"
[mysqld]
innodb_buffer_pool_size = 1G
innodb_log_file_size = 64M
innodb_lock_wait_timeout = 900
```

```sh
systemctl restart mariadb
```

## 2. 安装并配置 slurmdbd {#2-slurmdbd}

安装上一节构建的 `slurmdbd` 软件包并编写 `/etc/slurm/slurmdbd.conf`：

```sh
apt install -y /root/slurm-debs/slurm-smd-slurmdbd_*.deb
```

```sh
cat > /etc/slurm/slurmdbd.conf <<EOF
AuthType=auth/munge
DbdHost=localhost
SlurmUser=slurm
LogFile=/var/log/slurm/slurmdbd.log
PidFile=/run/slurmdbd/slurmdbd.pid
StorageType=accounting_storage/mysql
StorageHost=localhost
StorageUser=slurm
StoragePass=$SLURMDB_PW
StorageLoc=slurm_acct_db
EOF
chown slurm:slurm /etc/slurm/slurmdbd.conf
chmod 600 /etc/slurm/slurmdbd.conf
systemctl enable --now slurmdbd
```

由于 `slurmdbd.conf` 内含数据库密码，权限必须设为 `0600` 且属主为 `slurm:slurm`，否则 `slurmdbd` 会拒绝启动。

## 3. 在 Slurm 中启用记账与关联强制校验 {#3-turn-accounting-on-in-slurm}

在 `ctl` 的 `/etc/slurm/slurm.conf` 末尾追加以下配置：

```ini
AccountingStorageType=accounting_storage/slurmdbd
AccountingStorageHost=ctl
AccountingStorageEnforce=associations,limits,qos
JobAcctGatherType=jobacct_gather/cgroup
```

将更新后的 `slurm.conf` 同步至所有节点并重启服务：

```sh
for host in node1 login; do scp /etc/slurm/slurm.conf root@$host:/etc/slurm/; done
systemctl restart slurmctld
ssh root@node1 systemctl restart slurmd
sacctmgr show cluster format=cluster,controlhost
```

`slurmctld` 首次连接 `slurmdbd` 时会自动将集群注册到数据库中。对于已经启动过 `slurmctld` 的集群，**切勿**再手动执行 `sacctmgr add cluster`——否则数据库会分配一个新的集群 ID，导致 `slurmctld` 因 `CLUSTER ID MISMATCH` 报错退出。

`AccountingStorageEnforce=associations,limits,qos` 意味着用户必须拥有有效的 Slurm 关联（Association）才能提交作业；门户用户在创建时由 slave 触发器自动建立关联。

## 4. 为本地管理员添加记账关联 {#4-accounts-for-administrators}

`slurmdbd` 默认已为 `root` 创建关联，因此 `root` 可直接提交测试作业。如有其他非门户托管的本地管理员账号，请为其手动添加关联：

```sh
sacctmgr -i add account admins Description="Cluster administrators" Organization=it
sacctmgr -i add user <name> Account=admins
```

由 Slurm Portal 托管的用户无需手动添加，触发器会自动创建与用户名同名的 Slurm 账户及关联。

## 验证是否生效 {#check-it-works}

验证集群已注册且已完成的作业已写入记账数据库：

```sh
sacctmgr show cluster format=cluster,controlhost
srun hostname
sacct -X --format=JobID,User,Account,State
```

输出中应能看到由 `root` 在 `root` 账户下执行的作业状态为 `COMPLETED`。

| 常见问题 | 排查方法 |
| --- | --- |
| `slurmdbd` 无法启动 | 检查 `/var/log/slurm/slurmdbd.log`、`slurmdbd.conf` 的 `0600` 权限及 MariaDB 密码 |
| `Invalid account or account/partition combination` | 当前用户尚无 Slurm 关联：执行 `sacctmgr show assoc user=<name>` 查看 |
| `sacct` 无任何输出 | 确认 `ctl` 的 `slurm.conf` 已添加 `AccountingStorageType` 且已重启 `slurmctld` |
| `fatal: CLUSTER ID MISMATCH` | 集群曾被手动添加：执行 `sacctmgr -i delete cluster cluster1` 后重启 `slurmctld` |

## 下一步 {#next}

继续阅读 [slurmrestd](slurmrestd.md)。
