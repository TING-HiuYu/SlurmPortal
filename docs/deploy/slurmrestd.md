# slurmrestd

`slurmrestd` provides Slurm's HTTP REST API. Slurm Portal uses it (through the local read-only `slurm-notify-observer` proxy on `ctl`) to query finished job metadata and GPU allocations for job completion emails and 7-day GPU usage statistics. Because only the local observer communicates with `slurmrestd`, it is bound strictly to `127.0.0.1`.

**Target host:** `ctl`. **Estimated time:** 10 minutes. *(If you do not plan to use job completion emails or GPU usage monitoring, you may skip this page.)*

## 1. A JWT key for Slurm

`slurmrestd` authenticates requests using HS256 JSON Web Tokens verified by a shared secret key configured in both `slurmctld` and `slurmdbd`:

```sh
dd if=/dev/random of=/var/lib/slurm/slurmctld/jwt_hs256.key bs=32 count=1
chown slurm:slurm /var/lib/slurm/slurmctld/jwt_hs256.key
chmod 0600 /var/lib/slurm/slurmctld/jwt_hs256.key
```

Append the JWT authentication plugin settings to `/etc/slurm/slurm.conf`:

```ini
AuthAltTypes=auth/jwt
AuthAltParameters=jwt_key=/var/lib/slurm/slurmctld/jwt_hs256.key
```

Add the exact same two lines to `/etc/slurm/slurmdbd.conf`. Then redistribute `slurm.conf` to the other cluster hosts and restart the daemons:

```sh
for host in node1 login; do scp /etc/slurm/slurm.conf root@$host:/etc/slurm/; done
systemctl restart slurmdbd slurmctld
ssh root@node1 systemctl restart slurmd
```

Keep `jwt_hs256.key` on `ctl` only with permissions `0600`—anyone holding this key can mint tokens for any Slurm user.

## 2. Install and run slurmrestd

`slurmrestd` refuses to run as `root` or `slurm`. Create a dedicated unprivileged `slurmrestd` system user and install the package:

```sh
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin slurmrestd
apt install -y /root/slurm-debs/slurm-smd-slurmrestd_*.deb
```

Override the systemd unit so `slurmrestd` listens only on loopback (`127.0.0.1:6820`) rather than on all network interfaces:

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

## Check it works

Mint a 60-second JWT as `root` and query the `v0.0.45` ping endpoint:

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

In production, `slurm-notify-observer` automatically mints short-lived tokens using `jwt_hs256.key` whenever the slave queries job details.

| Symptom | What to Check |
| --- | --- |
| `slurmrestd` fails to start | Inspect `journalctl -u slurmrestd` and confirm the `slurmrestd` system user exists |
| `401 Unauthorized` even with a valid token | Verify `AuthAltTypes=auth/jwt` is present in both `slurm.conf` and `slurmdbd.conf` and that both daemons were restarted |
| `404 Not Found` on `/slurm/v0.0.45/ping` | Your Slurm build is older than 26.05—see [Slurm](slurm.md) |

## Next

Proceed to [OpenLDAP](openldap.md).
