# Slave triggers

Whenever the slave agent applies a snapshot to OpenLDAP, it emits lifecycle events for each change: `user.created`, `user.updated`, `user.deleted`, `membership.added`, and `membership.removed`. Local triggers subscribe to specific events and run host commands in response; see [triggers.yaml.example](triggers.yaml.example) for the configuration format and point `trigger-file` in `slave.yml` at your copy.

`slurm_trigger.py` is the standard provisioning trigger for Slurm clusters. On each event, it:

- creates, updates, or deletes the user's Slurm accounting association via `sacctmgr` and verifies the resulting state before reporting success;
- runs the cluster's configured `storageCommands`, such as creating the user's home directory or invoking the ACL reconciler in [../storage](../storage).

For safety, `slurm_trigger.py` refuses to delete a Slurm user whose POSIX identity still resolves through NSS, and all operations are idempotent so repeated deliveries of the same event are safe. See [slurm-trigger.json.example](slurm-trigger.json.example) for all available options.

## Slave on the host (systemd)

| File | Owner and mode | Location |
| --- | --- | --- |
| `slurm_trigger.py` | `root:root`, `0755` | `/usr/local/libexec/go-ldap-admin/` |
| `slurm-trigger.json` | `root:root`, `0600` (directory `0700`) | `/etc/go-ldap-admin/` |
| `triggers.yaml` | slave service user, `0600` | alongside `slave.yml` |

When the slave runs as an unprivileged systemd user, it invokes `slurm_trigger.py` via `sudo`. Grant only the exact command below in `/etc/sudoers.d/` (`root:root`, mode `0440`) and validate it with `visudo -cf`:

```text
slurm-portal ALL=(root) NOPASSWD: /usr/local/libexec/go-ldap-admin/slurm_trigger.py --config /etc/go-ldap-admin/slurm-trigger.json *
```

In the slave's systemd unit, set `NoNewPrivileges=false` and list any directories modified by `storageCommands` in `ReadWritePaths`.

## Slave in a container

Because a containerized slave cannot execute `sacctmgr` or modify host filesystems directly, it forwards trigger events over SSH using `slurm-portal-host-bridge` (built from [`cmd/host-bridge`](../../cmd/host-bridge) and bundled in the slave container image):

1. On the cluster host, install `slurm_trigger.py` and `slurm-trigger.json` as shown above, place `slurm-portal-host-bridge` in `/usr/local/libexec/go-ldap-admin/`, and install [host-dispatch](host-dispatch) alongside it.
2. Generate an SSH key pair for the bridge. Add the public key to `/root/.ssh/authorized_keys` on the host with a forced `command="..."` entry invoking `host-dispatch` (see the header comment in `host-dispatch`).
3. In the slave container's `secrets/` directory, save the private key as `host-bridge.key` and create `host-bridge.json` from [host-bridge.json.example](host-bridge.json.example). Enable the `slurm-account-via-host` trigger in `triggers.yaml`.

Trigger payloads are passed exclusively as JSON on standard input, and the SSH forced command executes only `host-dispatch`—no shell command is ever constructed from user attributes.

## Operating

Trigger events are delivered strictly in queue order. If a trigger command fails, the event remains at the head of the queue and blocks subsequent deliveries so account changes are never applied out of order. Inspect the slave logs to see the failing command, fix the underlying issue (permissions, NSS resolution, NFS mounts, or `slurmdbd`), and let the slave retry automatically. Never delete queued trigger files manually.

Run the trigger unit tests from this directory:

```sh
python3 -m unittest test_slurm_trigger
```
