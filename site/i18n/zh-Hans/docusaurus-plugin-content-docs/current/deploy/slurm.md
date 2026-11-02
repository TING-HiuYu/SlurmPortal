# Slurm

本页在控制节点（`ctl`）、计算节点（`node1`）与登录节点（`login`）上构建并安装 **Slurm 26.05**，并提交首个测试作业。

**执行节点：** `ctl`、计算节点、`login`。**预计用时：** 30 分钟（大部分时间为编译耗时）。

:::info Slurm 版本选择说明
Ubuntu 24.04 官方软件源自带的版本为 Slurm 23.11，可直接支持 Slurm Portal 的账号同步、SSH 证书、实时状态与 Web 终端功能；但**作业完成邮件**与**单作业 GPU 用量统计**依赖 Slurm 26.05 才引入的 `slurmrestd` `v0.0.45` 接口。因此本指南使用 SchedMD 官方源码包内置的 Debian 打包规则构建 26.05 软件包。如果您不需要这两项可选功能，也可以直接执行 `apt install slurmctld slurmd slurm-client slurmdbd`，下方的配置文件完全通用。
:::

## 1. 构建 DEB 软件包 {#1-build-the-packages}

在 `ctl`（或与集群节点 CPU 架构相同的任意 Ubuntu 24.04 主机）上编译构建一次：

```sh
apt install -y build-essential fakeroot devscripts equivs curl bzip2
cd /root
curl -fsSLO https://download.schedmd.com/slurm/slurm-26.05.5.tar.bz2
tar -xaf slurm-26.05.5.tar.bz2
cd slurm-26.05.5
mk-build-deps -i -t 'apt-get -y --no-install-recommends' debian/control
debuild -b -uc -us
mkdir -p /root/slurm-debs && mv ../*.deb /root/slurm-debs/
```

可在 [SchedMD 下载页面](https://www.schedmd.com/download-slurm/)查看最新的 `26.05.x` 版本号并替换命令中的 `26.05.5`。

将构建产出的 `.deb` 软件包分发至其余所有节点：

```sh
for host in node1 login; do
  ssh root@$host mkdir -p /root/slurm-debs
  scp /root/slurm-debs/*.deb root@$host:/root/slurm-debs/
done
```

## 2. 创建统一的 `slurm` 系统用户 {#2-the-slurm-user}

Slurm 守护进程以 `slurm` 用户身份运行，且该用户在**所有集群节点上必须拥有完全相同的 UID**。由于 SchedMD 官方源码包不会自动创建该用户，请在所有节点（`ctl`、计算节点、`login`）上执行：

```sh
useradd --system --uid 64030 --user-group --no-create-home --home-dir /var/lib/slurm \
  --shell /usr/sbin/nologin slurm
```

`64030` 是 Debian 与 Ubuntu 为 `slurm` 预留的标准 UID，不会与其他系统软件包冲突（`useradd` 提示该值大于 `SYS_UID_MAX` 属于正常现象）。

## 3. 安装软件包 {#3-install}

在各节点安装对应角色的 DEB 包：

```sh
# ctl
apt install -y /root/slurm-debs/slurm-smd_*.deb /root/slurm-debs/slurm-smd-client_*.deb \
  /root/slurm-debs/slurm-smd-slurmctld_*.deb
# each compute node
apt install -y /root/slurm-debs/slurm-smd_*.deb /root/slurm-debs/slurm-smd-client_*.deb \
  /root/slurm-debs/slurm-smd-slurmd_*.deb
# login
apt install -y /root/slurm-debs/slurm-smd_*.deb /root/slurm-debs/slurm-smd-client_*.deb
```

由于此时尚未编写 `/etc/slurm/slurm.conf`，守护进程暂不会启动。

## 4. 编写配置文件 {#4-configure}

在每个计算节点上运行 `slurmd -C` 查看探测到的硬件规格：

```text
NodeName=node1 CPUs=2 Boards=1 SocketsPerBoard=1 CoresPerSocket=2 ThreadsPerCore=1 RealMemory=11924
```

在 `ctl` 上创建 `/etc/slurm/slurm.conf` 与 `/etc/slurm/cgroup.conf`。注意将 `RealMemory` 设为略低于 `slurmd -C` 报告的值，为操作系统自身预留内存：

```ini title="/etc/slurm/slurm.conf"
ClusterName=cluster1
SlurmctldHost=ctl
SlurmUser=slurm
AuthType=auth/munge

StateSaveLocation=/var/lib/slurm/slurmctld
SlurmdSpoolDir=/var/spool/slurmd
SlurmctldPidFile=/run/slurmctld/slurmctld.pid
SlurmdPidFile=/run/slurm/slurmd.pid
SlurmctldLogFile=/var/log/slurm/slurmctld.log
SlurmdLogFile=/var/log/slurm/slurmd.log

ProctrackType=proctrack/cgroup
TaskPlugin=task/affinity,task/cgroup
SelectType=select/cons_tres
SelectTypeParameters=CR_Core_Memory
ReturnToService=2

NodeName=node1 CPUs=2 Boards=1 SocketsPerBoard=1 CoresPerSocket=2 ThreadsPerCore=1 RealMemory=11000 State=UNKNOWN
PartitionName=cpu Nodes=node1 Default=YES MaxTime=INFINITE State=UP
```

```ini title="/etc/slurm/cgroup.conf"
ConstrainCores=yes
ConstrainRAMSpace=yes
ConstrainDevices=yes
```

| 配置项 | 作用说明 |
| --- | --- |
| `select/cons_tres` + `CR_Core_Memory` | 按 CPU 核心与内存（而非整机节点）分配资源，并支持通过 `--gpus` 申请 GPU |
| `proctrack/cgroup` + `task/cgroup` | 使用 Linux cgroup 严格限制资源用量，并在作业结束时彻底清理残留进程 |
| `ReturnToService=2` | 计算节点重启后，只要硬件配置吻合即自动恢复为可用状态 |

所有节点必须持有完全相同的这两份配置文件。在 `ctl` 上创建状态与日志目录，并将配置文件复制到其余节点：

```sh
chown slurm:slurm /var/lib/slurm/slurmctld /var/log/slurm
for host in node1 login; do scp /etc/slurm/slurm.conf /etc/slurm/cgroup.conf root@$host:/etc/slurm/; done
```

### GPU 节点配置 {#gpus}

对于配备 NVIDIA GPU 的计算节点，请在执行 `mk-build-deps` 前先安装 `libnvidia-ml-dev` 以启用 NVML 支持；随后在 `slurm.conf` 中添加 `GresTypes=gpu`，在对应节点的 `NodeName=` 行追加 `Gres=gpu:<数量>`，并在该计算节点上创建 `/etc/slurm/gres.conf`：

```ini title="/etc/slurm/gres.conf"
AutoDetect=nvml
```

## 5. 启动服务 {#5-start}

分别在 `ctl` 与各计算节点上启用并启动服务：

```sh
# on ctl
systemctl enable --now slurmctld
# on each compute node
systemctl enable --now slurmd
```

## 验证是否生效 {#check-it-works}

在 `ctl` 或 `login` 上查看分区状态并提交测试作业：

```sh
sinfo
srun -N1 hostname
```

```text
PARTITION AVAIL  TIMELIMIT  NODES  STATE NODELIST
cpu*         up   infinite      1   idle node1
node1
```

| 常见问题 | 排查方法 |
| --- | --- |
| 节点状态为 `down` 或 `drain` | 执行 `scontrol show node node1` 查看 `Reason=`，并检查计算节点上的 `/var/log/slurm/slurmd.log` |
| `Invalid authentication credential` | Munge 认证失败：参阅 [Munge 验证章节](munge.md#check-it-works) |
| 节点因内存或 CPU 数量过低被排空 | `slurm.conf` 中 `NodeName=` 配置的内存或核心数超出了 `slurmd -C` 的实际报告值 |
| `slurmctld` 无法启动 | 查看 `journalctl -u slurmctld`，并确认 `/var/lib/slurm/slurmctld` 的属主为 `slurm:slurm` |

日后每次修改 `slurm.conf` 后，请重新同步至所有节点并执行 `scontrol reconfigure`。

## 下一步 {#next}

继续阅读[记账服务](accounting.md)。
