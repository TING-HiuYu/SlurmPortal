# Initial portal setup

Once the master control plane is running ([Serverless Master](master-saas.md) or [Container Master](master-container.md)), sign in to the web console as `admin` to configure your SSH Certificate Authority, default POSIX group presets, RBAC roles, email delivery providers, and optional GitHub OAuth sign-in before connecting cluster slave agents.

**Estimated time:** 10 minutes.

## 1. Create an SSH Certificate Authority

Every cluster's `sshd` will trust the SSH Certificate Authority you configure here:

1. Navigate to **Admin → Certificate authorities**.
2. Click **Create CA** and select **SSH (`ssh`)**.
3. Choose **Generate Ed25519 key pair** (or import an existing OpenSSH CA private key) and give it a descriptive name such as `Cluster SSH CA`.
4. Ensure its status is **Enabled**.

The master immediately seals the CA private key with `PKI_MASTER_KEY` and includes the CA's public key in the next cluster snapshot so every slave publishes it to `current/ssh/trusted-user-ca-keys.pub`.

## 2. Create primary POSIX groups and home presets

Every user in Slurm Portal belongs to exactly one **primary POSIX group**, which determines their primary numeric GID, default `$HOME` directory path, and login shell:

1. Navigate to **Admin → Groups** and click **Create group**.
2. Select **POSIX group (`posixGroup`)**.
3. Enter a group name (for example, `research`), set **Home root** to `/home/research`, and set **Default shell** to `/bin/bash`.
4. Optionally create organizational units (`groupOfUniqueNames`) to model your institution's department or lab hierarchy.

When you create a user `alice` with primary group `research`, her home directory automatically defaults to `/home/research/alice`, and the cluster's `slurm_trigger.py` script creates that directory on the NFSv4 share during sync.

## 3. Configure roles and delegated administration

Slurm Portal ships with a rank-`1` super-administrator role. To delegate user or group management to lab leads without granting full administrative privileges:

1. Navigate to **Admin → Roles** and click **Create role**.
2. Assign a numeric **Rank (`sort`)** strictly greater than `1` (for example, `10` for cluster operators or `50` for group leads).
3. Grant only the `(object, action)` permissions required (such as `user:read`, `user:create`, `user:update`, and `group:update`).

Because of rank-enforced anti-escalation, a role at rank `50` can never modify users or roles at rank `1`–`50`, nor can it grant permissions it does not hold.

## 4. Configure email providers and templates

Job completion reports, cross-cluster file transfer alerts, and self-service password reset codes all route through the **Mail Hub**:

1. Navigate to **Admin → Mail service** and add an **SMTP** provider (or an HTTP **REST** mail API provider).
2. Click **Test** to send a verification email and confirm delivery.
3. *(Optional)* If you use multiple email providers with daily sending limits, create a **Pool** provider (`mail-pool`) to distribute messages by priority quota, round-robin, or weight with automatic failover.
4. Enable and customize the HTML templates under:
   - **Password codes** (enables the **Forgot password?** flow on the login screen);
   - **Job notifications** (sends job completion summaries);
   - **Transfer notifications** (sends cross-cluster `rsync` completion alerts).

## 5. Configure GitHub OAuth sign-in (optional)

To let users sign in with their GitHub accounts and automatically provision new accounts into specific POSIX groups:

1. In GitHub (**Settings → Developer settings → OAuth Apps**), create a new OAuth App with callback URL:
   `https://portal.example.org/api/v2/oauth/callback/github`
2. Store the client secret in your master environment (`OAUTH_GITHUB_CLIENT_SECRET`) or provide it directly when creating the connector.
3. Navigate to **Admin → OAuth connectors** (or run `deploy/worker/connector-github.mjs`) and define ordered access rules:
   - Match by **GitHub organization**, **username/email glob**, or **everyone**;
   - Assign the target **Primary POSIX group**, **Supplementary groups**, and **Roles** for newly created accounts.

## Next

Proceed to [Cluster Slave Agent](slaves.md) to connect your first Slurm cluster to the master.
