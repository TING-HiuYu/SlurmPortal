# Munge

Slurm daemons and CLI utilities authenticate RPC messages across cluster nodes using **Munge**. Every host in the cluster shares an identical secret key (`/etc/munge/munge.key`), allowing a short-lived credential encoded on one node to be verified by any other node.

**Target hosts:** All cluster hosts (`ctl`, compute nodes, and `login`). **Estimated time:** 5 minutes.

## 1. Install

On every cluster host (`ctl`, `node1`, `login`):

```sh
apt install -y munge
```

Installing the package creates the `munge` system account and generates a random local key on each machine. In the next step, you will generate a single authoritative key on `ctl` and distribute it to every node.

## 2. Make one key and copy it everywhere

On `ctl`, generate a fresh Munge key and lock down its ownership and permissions:

```sh
mungekey --create --force
chown munge:munge /etc/munge/munge.key
chmod 0400 /etc/munge/munge.key
systemctl restart munge
```

Copy `/etc/munge/munge.key` from `ctl` to every other cluster host and restart the daemon:

```sh
for host in node1 login; do
  scp /etc/munge/munge.key root@$host:/etc/munge/munge.key
  ssh root@$host 'chown munge:munge /etc/munge/munge.key && chmod 0400 /etc/munge/munge.key && systemctl restart munge'
done
```

:::warning
`/etc/munge/munge.key` is a cluster-wide shared secret: anyone holding it can forge credentials for any UID on the cluster. Transfer it only over SSH, never commit it to version control, and keep its file permissions set to `0400`.
:::

## Check it works

Verify that a credential generated on `ctl` decodes cleanly on both `node1` and `login`:

```sh
munge -n | ssh node1 unmunge
munge -n | ssh login unmunge | grep STATUS
```

```text
STATUS:          Success (0)
ENCODE_HOST:     ctl (10.0.0.10)
```

| Error Message | Cause & Resolution |
| --- | --- |
| `Invalid credential` | Mismatched keys—compare `sha256sum /etc/munge/munge.key` across hosts |
| `Expired credential` / `Rewound credential` | Clock skew between hosts—verify NTP synchronization with `timedatectl` |
| `Failed to access "/run/munge/munge.socket.2"` | `munged` is not running—inspect `systemctl status munge` and `/var/log/munge/munged.log` |

## Next

Proceed to [Slurm](slurm.md).
