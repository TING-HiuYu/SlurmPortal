# Check it works

This page runs an end-to-end verification walkthrough using a single test account (`alice`). Each step confirms a specific layer of your Slurm Portal and cluster deployment. Completing the entire checklist takes about five minutes.

## 1. Prepare the portal

Sign in to the portal as `admin` and verify the baseline configuration from [Initial Portal Setup](portal-setup.md):

1. **Certificate authorities:** Confirm an **SSH (`ssh`)** CA exists and is enabled.
2. **Groups:** Confirm a POSIX group named `research` exists with **Home root** `/home/research` and **Default shell** `/bin/bash`.
3. *(Optional)* On `infra`, create `/srv/nfs/home/research/shared` if you want `~/SHARED` inside `research` users' home directories to point to a group-specific shared directory rather than `/home/shared`.

## 2. Create a user

Under **Admin → Users**, create user `alice` with primary POSIX group `research` and a password.

Within 60–90 seconds, run the following checks on `ctl`:

```sh
curl -s 127.0.0.1:8081/readyz; echo                  # "ready":true, a new hash
getent passwd alice                                    # from LDAP, through sssd
sacctmgr -nP show assoc where user=alice format=cluster,account,user
ls -ld /home/research/alice                            # made by the trigger
```

**What this verifies:** The slave detected the new snapshot hash, reconciled `alice` into OpenLDAP, SSSD resolved the new POSIX account, and `slurm_trigger.py` created both her Slurm accounting association and her NFSv4 home directory.

## 3. Log in with a certificate

In **Admin → Users**, open `alice` and issue an SSH certificate under your SSH CA (or sign in as `alice` and issue one under **Workspace → Certificates & credentials**). Save the private key and signed certificate (returned **once**) as `~/.ssh/alice` and `~/.ssh/alice-cert.pub`, then connect to `login`:

```sh
chmod 600 ~/.ssh/alice
ssh -i ~/.ssh/alice -o CertificateFile=~/.ssh/alice-cert.pub alice@login.example.org
```

Inside the SSH session on `login`, run:

```sh
id                       # uid=10000(alice) gid=10000(research)
pwd                      # /home/research/alice
srun -N1 hostname        # node1
sacct -X --format=JobID,User,Account,State
```

**What this verifies:** `sshd` on `login` trusts the portal's SSH CA via `/srv/slurm-portal-trust/current/ssh/trusted-user-ca-keys.pub`, the shared NFSv4 home directory is mounted, and `alice` can schedule and record Slurm jobs under her own accounting association.

## 4. Live status

Open **Current resources** and **Cluster status** in the portal. Confirm `cluster1` displays its node states, available CPUs and memory, and active job queue. Submit `sbatch --wrap "sleep 120"` as `alice` and verify that the running job appears in the portal within 60 seconds.

**What this verifies:** The slave's `current-status` collector is querying `scontrol`/`squeue`, uploading status objects to R2/S3, and the browser is reading them via presigned URLs.

## 5. Revocation

In **Admin → Users**, revoke `alice`'s SSH certificate. Within ~90 seconds (one 60-second slave poll cycle plus the 5-second NFS attribute cache TTL on `/srv/slurm-portal-trust`), retry the same SSH command:

```text
alice@login.example.org: Permission denied (publickey).
```

Inspecting `journalctl -u ssh` on `login` confirms `revoked by file …/revoked.krl`.

**What this verifies:** Certificate revocations are compiled into OpenSSH KRLs and enforced by `sshd` across login nodes without manual intervention.

## 6. Optional parts

If you deployed the optional integrations:

- **Job completion emails & GPU monitoring:** As `alice`, submit `sbatch --wrap "sleep 90"`. When the job completes, confirm `alice` receives a summary email and the job is listed under **Recent resource summary**.
- **Web Access Gateway:** Sign in to the portal as `alice`, open **Workspace → Web terminal**, and select `cluster1` to launch an interactive shell in `/home/research/alice`.

## 7. Clean up

Delete `alice` under **Admin → Users**. On the next sync cycle, the slave removes her entry from OpenLDAP and the trigger disables her Slurm association (`deletePolicy: "disable"`). Her `/home/research/alice` directory remains untouched on NFS until you archive or remove it manually.

## When something fails

| Symptom | Where to Start |
| --- | --- |
| New user does not appear on the cluster | Inspect `journalctl -u slurm-portal-slave` on `ctl` |
| `getent passwd alice` returns nothing | Check `sssctl domain-status example.org` and flush cache with `sss_cache -E` |
| Missing Slurm association or home directory | Inspect trigger execution lines in the slave log and test the logged `slurm_trigger.py` command manually |
| SSH certificate login rejected | Check `journalctl -u ssh` on `login` and verify `ls /srv/slurm-portal-trust/current/ssh/` |
| Empty or stale cluster status | Confirm `current-status.enabled: true` in `slave.yml` and test `scontrol ping` as user `slurm-portal` |

See [Day-2 Operations](operations.md) for ongoing maintenance, backup, and key rotation procedures.
