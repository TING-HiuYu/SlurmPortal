# Accounting

Slurm accounting (`slurmdbd`) records job history and enforces account/user associations before jobs are allowed to run. Slurm Portal relies on accounting: the slave's trigger script automatically creates a Slurm account and association for every portal user, and job completion notifications query finished job records from `slurmdbd`.

**Target host:** `ctl`. **Estimated time:** 10 minutes.

## 1. Database

Install MariaDB on `ctl` and create the `slurm_acct_db` database and `slurm` user:

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

Apply SchedMD's recommended InnoDB buffer and lock settings for `slurmdbd`:

```ini title="/etc/mysql/mariadb.conf.d/60-slurmdbd.cnf"
[mysqld]
innodb_buffer_pool_size = 1G
innodb_log_file_size = 64M
innodb_lock_wait_timeout = 900
```

```sh
systemctl restart mariadb
```

## 2. slurmdbd

Install the `slurmdbd` package built in the previous step and write `/etc/slurm/slurmdbd.conf`:

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

Because `/etc/slurm/slurmdbd.conf` contains the database password, `slurmdbd` refuses to start unless the file is owned by `slurm` with mode `0600`.

## 3. Turn accounting on in Slurm

Append the accounting settings to `/etc/slurm/slurm.conf` on `ctl`:

```ini
AccountingStorageType=accounting_storage/slurmdbd
AccountingStorageHost=ctl
AccountingStorageEnforce=associations,limits,qos
JobAcctGatherType=jobacct_gather/cgroup
```

Distribute the updated `slurm.conf` to all nodes and restart the Slurm daemons:

```sh
for host in node1 login; do scp /etc/slurm/slurm.conf root@$host:/etc/slurm/; done
systemctl restart slurmctld
ssh root@node1 systemctl restart slurmd
sacctmgr show cluster format=cluster,controlhost
```

When `slurmctld` connects to `slurmdbd` for the first time, it registers `cluster1` automatically. Do **not** run `sacctmgr add cluster` manually for a cluster whose `slurmctld` has already started—doing so assigns a conflicting cluster ID in the database and causes `slurmctld` to exit with `CLUSTER ID MISMATCH`.

Setting `AccountingStorageEnforce=associations,limits,qos` ensures that only users with a valid Slurm association may submit jobs. When Slurm Portal provisions a user, its trigger script creates that association automatically.

## 4. Accounts for administrators

`slurmdbd` pre-creates the `root` account and association so `root` can submit test jobs immediately. If you have local non-portal administrator accounts on the cluster, create associations for them manually:

```sh
sacctmgr -i add account admins Description="Cluster administrators" Organization=it
sacctmgr -i add user <name> Account=admins
```

Do not manually add users who will be managed by Slurm Portal—the slave trigger creates their Slurm accounts automatically.

## Check it works

Verify that the cluster is registered and that completed jobs are recorded in `sacct`:

```sh
sacctmgr show cluster format=cluster,controlhost
srun hostname
sacct -X --format=JobID,User,Account,State
```

You should see the test job listed as `COMPLETED` under user `root` and account `root`.

| Symptom | What to Check |
| --- | --- |
| `slurmdbd` fails to start | Check `/var/log/slurm/slurmdbd.log`, verify `chmod 600 /etc/slurm/slurmdbd.conf`, and confirm MariaDB credentials |
| `Invalid account or account/partition combination` | The submitting user lacks a Slurm association—check `sacctmgr show assoc user=<name>` |
| `sacct` returns no job records | Ensure `AccountingStorageType=accounting_storage/slurmdbd` is present in `slurm.conf` and `slurmctld` was restarted |
| `fatal: CLUSTER ID MISMATCH` | The cluster was added manually via `sacctmgr`—run `sacctmgr -i delete cluster cluster1` and restart `slurmctld` |

## Next

Proceed to [slurmrestd](slurmrestd.md).
