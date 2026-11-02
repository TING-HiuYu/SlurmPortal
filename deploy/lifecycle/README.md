# Job lifecycle (Slurm controller)

These components connect the Slurm controller (`slurmctld`) to the slave agent's job lifecycle runtime, powering job completion emails and per-job GPU usage accounting in the portal. See [docs/dev/architecture.md](../../docs/dev/architecture.md#job-lifecycle--gpu-telemetry-pipeline) for the end-to-end architecture.

| File | Installed as | Purpose |
| --- | --- | --- |
| `prolog-slurmctld`, `epilog-slurmctld` | `PrologSlurmctld` and `EpilogSlurmctld` in `slurm.conf` | Invokes `slurm-lifecycle-hook` with `--event starting` or `--event ending`. Always exits `0` if the binary is missing or fails so Slurm job scheduling is never blocked |
| `slurm-notify-observer.service` | systemd unit on the Slurm controller | Runs [`cmd/slurm-notify-observer`](../../cmd/slurm-notify-observer) as the `slurm` user—the only service that reads the Slurm JWT signing key—and exposes read-only job queries to the slave over mTLS |

## Set up

1. Build `cmd/slurm-lifecycle-hook` and `cmd/slurm-notify-observer`, and install both binaries in `/usr/local/libexec/go-ldap-admin/`.
2. Create the hook configuration (`/etc/go-ldap-admin/lifecycle-hook.yaml`) and observer configuration (`/etc/go-ldap-admin/job-notify-observer.yaml`), both with mode `0600`.
3. Install and start `slurm-notify-observer.service`, add `PrologSlurmctld` and `EpilogSlurmctld` to `slurm.conf`, and run `scontrol reconfigure`.
4. Point `lifecycle-file` in the slave's `slave.yml` at your lifecycle runtime configuration file and restart the slave agent.
5. In the portal web console, enable **Job notifications** and verify that completed jobs appear under **Recent resource summary**.
