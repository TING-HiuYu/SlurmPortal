# Job lifecycle (Slurm controller)

These files connect Slurm to the slave's lifecycle runtime, which sends job-end e-mails and
per-job GPU usage to the portal. See [docs/architecture.md](../../docs/architecture.md#job-lifecycle).

| File | Install as | Purpose |
| --- | --- | --- |
| `prolog-slurmctld`, `epilog-slurmctld` | `PrologSlurmctld` / `EpilogSlurmctld` in `slurm.conf` | Call `slurm-lifecycle-hook` with `--event starting` / `ending`. They exit 0 if the hook is missing, so scheduling never waits |
| `slurm-notify-observer.service` | systemd unit on the controller | Runs [`cmd/slurm-notify-observer`](../../cmd/slurm-notify-observer) as `slurm`: the only holder of the Slurm JWT key; serves read-only job lookups to the slave over mTLS |

## Set up

1. Build `cmd/slurm-lifecycle-hook` and `cmd/slurm-notify-observer`. Install them in
   `/usr/local/libexec/go-ldap-admin/`.
2. Write the hook config (`/etc/go-ldap-admin/lifecycle-hook.yaml`) and the observer config
   (`/etc/go-ldap-admin/job-notify-observer.yaml`). Both must be mode 0600.
3. Install and start the observer unit. Add the two `slurm.conf` lines and run
   `scontrol reconfigure`.
4. Set `lifecycle-file` in the slave config to the lifecycle runtime config, and restart the
   slave.
5. In the portal, turn on 作业通知 (job e-mail) and check 近期资源汇总 (resource summaries)
   after the first job ends.
