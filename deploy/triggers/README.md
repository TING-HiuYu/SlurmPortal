# Slave triggers

When the slave applies a snapshot, it turns the changes into events: `user.created`,
`user.updated`, `user.deleted`, `membership.added` and `membership.removed`. A trigger runs a
local command for the events it subscribes to; [triggers.yaml.example](triggers.yaml.example)
lists them. Point `trigger-file` in the slave's config at your copy.

`slurm_trigger.py` is the trigger most sites want. For each event it:

- creates, updates or removes the user's Slurm association (`sacctmgr`), checking the result
  before reporting success;
- runs the cluster's `storageCommands`, such as the HOME ACL reconciler in [../storage](../storage).

It refuses to delete a user whose identity still resolves through NSS, and it is safe to run more
than once for the same event. Its settings are in [slurm-trigger.json.example](slurm-trigger.json.example).

## Slave on the host (systemd)

| File | Owner, mode | Location |
| --- | --- | --- |
| `slurm_trigger.py` | root:root, 0755 | `/usr/local/libexec/go-ldap-admin/` |
| `slurm-trigger.json` | root:root, 0600; directory 0700 | `/etc/go-ldap-admin/` |
| `triggers.yaml` | slave user, 0600 | next to `slave.yml` |

The slave runs the script through `sudo`. Allow only that exact form, and check the file with
`visudo -cf` (root:root, 0440):

```text
slurm-portal ALL=(root) NOPASSWD: /usr/local/libexec/go-ldap-admin/slurm_trigger.py --config /etc/go-ldap-admin/slurm-trigger.json *
```

The slave's unit then needs `NoNewPrivileges=false`, and `ReadWritePaths` must include anything
the storage commands write.

## Slave in a container

A container cannot run `sacctmgr` on the host, so its trigger uses the host bridge instead
(`slurm-portal-host-bridge`, built from [`cmd/host-bridge`](../../cmd/host-bridge) and included in
the slave image):

1. On the cluster host, install `slurm_trigger.py` and `slurm-trigger.json` as above, the bridge
   binary as `/usr/local/libexec/go-ldap-admin/slurm-portal-host-bridge`, and
   [host-dispatch](host-dispatch) next to it.
2. Make an SSH key for the bridge. On the host, add its public key to root's `authorized_keys`
   with the forced command shown in `host-dispatch`.
3. In the slave's `secrets/`, put the private key as `host-bridge.key` and a `host-bridge.json`
   (from [host-bridge.json.example](host-bridge.json.example)). Enable the `slurm-account-via-host`
   trigger.

The event travels on stdin, and the forced command only ever runs the dispatcher. No shell
command is built from user data.

## Operating

Deliveries run in queue order. A failed one stays queued and blocks the ones after it, so changes
are never applied out of order. Read the slave log for the failing command, then fix the cause:
permissions, NSS, mounts or Slurm. Do not delete queue entries by hand.

Run the tests with `python3 -m unittest test_slurm_trigger` in this directory.
