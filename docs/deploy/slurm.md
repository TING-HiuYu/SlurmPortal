# Slurm

This guide builds and installs **Slurm 26.05** across the controller (`ctl`), compute nodes (`node1`), and login node (`login`), then verifies job scheduling.

**Target hosts:** `ctl`, compute nodes, and `login`. **Estimated time:** 30 minutes (primarily package compilation).

:::info Which Slurm Version Do You Need?
Ubuntu 24.04 ships Slurm 23.11 in `apt`, which works with Slurm Portal for account syncing, SSH certificates, live status, and the web terminal. However, **job completion emails** and **per-job GPU usage statistics** query the `v0.0.45` OpenAPI schema in `slurmrestd`, introduced in **Slurm 26.05**. Below, we build official `.deb` packages from SchedMD's source tarball. If you do not need those two features, you can install Ubuntu's built-in `slurmctld`, `slurmd`, `slurm-client`, and `slurmdbd` packages instead and use the exact same configuration files.
:::

## 1. Build the packages

Compile the Debian packages once on `ctl` (or any Ubuntu 24.04 build host matching your cluster's CPU architecture):

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

Check [SchedMD's downloads page](https://www.schedmd.com/download-slurm/) for the latest `26.05.x` point release.

Distribute the built `.deb` archives to the compute and login nodes:

```sh
for host in node1 login; do
  ssh root@$host mkdir -p /root/slurm-debs
  scp /root/slurm-debs/*.deb root@$host:/root/slurm-debs/
done
```

## 2. The `slurm` user

Slurm daemons run as the `slurm` system user, which must share the **exact same UID and GID across every cluster host**. Because SchedMD's upstream packages do not pre-create this account, create it on every host (`ctl`, compute nodes, and `login`):

```sh
useradd --system --uid 64030 --user-group --no-create-home --home-dir /var/lib/slurm \
  --shell /usr/sbin/nologin slurm
```

UID `64030` is the standard Debian/Ubuntu reserved UID for `slurm`, avoiding collisions with other system packages. (The `useradd` warning about `64030` exceeding `SYS_UID_MAX` is harmless and expected.)

## 3. Install

Install the role-specific packages on each host:

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

The systemd units remain inactive until `/etc/slurm/slurm.conf` is in place.

## 4. Configure

On each compute node, run `slurmd -C` to inspect its detected CPU topology and physical memory:

```text
NodeName=node1 CPUs=2 Boards=1 SocketsPerBoard=1 CoresPerSocket=2 ThreadsPerCore=1 RealMemory=11924
```

On `ctl`, create `/etc/slurm/slurm.conf` and `/etc/slurm/cgroup.conf`. Set `RealMemory` slightly below the value reported by `slurmd -C` so the Linux kernel and system daemons have headroom:

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

| Parameter | Purpose |
| --- | --- |
| `select/cons_tres` + `CR_Core_Memory` | Allocates individual CPU cores, memory, and GPUs (`--gpus`) rather than whole nodes |
| `proctrack/cgroup` + `task/cgroup` | Enforces CPU/memory/device limits via Linux cgroups and guarantees clean process teardown when a job ends |
| `ReturnToService=2` | Automatically returns a rebooted compute node to `idle` once `slurmd` registers with a matching hardware configuration |

Create the state and log directories on `ctl`, then copy both configuration files to every other host:

```sh
chown slurm:slurm /var/lib/slurm/slurmctld /var/log/slurm
for host in node1 login; do scp /etc/slurm/slurm.conf /etc/slurm/cgroup.conf root@$host:/etc/slurm/; done
```

### GPUs

If your compute nodes have NVIDIA GPUs, install `libnvidia-ml-dev` before running `mk-build-deps` so Slurm compiles with NVML support. Then add `GresTypes=gpu` to `slurm.conf`, append `Gres=gpu:<count>` to each GPU node's `NodeName=` line, and create `/etc/slurm/gres.conf` on those nodes:

```ini title="/etc/slurm/gres.conf"
AutoDetect=nvml
```

## 5. Start

Enable and start `slurmctld` on `ctl` and `slurmd` on each compute node:

```sh
# on ctl
systemctl enable --now slurmctld
# on each compute node
systemctl enable --now slurmd
```

## Check it works

From `ctl` or `login`, verify that the partition is `up` and run a test job:

```sh
sinfo
srun -N1 hostname
```

```text
PARTITION AVAIL  TIMELIMIT  NODES  STATE NODELIST
cpu*         up   infinite      1   idle node1
node1
```

| Symptom | What to Check |
| --- | --- |
| Node shows `down` or `drain` | Run `scontrol show node node1` (inspect `Reason=`) and check `/var/log/slurm/slurmd.log` on the node |
| `Invalid authentication credential` | Munge key mismatch or clock skew—see [Munge](munge.md#check-it-works) |
| Node drained due to `Low RealMemory` or `Low CPUs` | The `NodeName=` line in `slurm.conf` specifies more RAM or cores than `slurmd -C` detected |
| `slurmctld` fails to start | Inspect `journalctl -u slurmctld` and verify ownership of `/var/lib/slurm/slurmctld` |

Whenever you edit `/etc/slurm/slurm.conf` later, redistribute it to all hosts and run `scontrol reconfigure`.

## Next

Proceed to [Accounting](accounting.md).
