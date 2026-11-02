# Web access gateway

The **Web Access Gateway** (`cmd/gateway`) runs on a cluster's login node (`login`) behind an Nginx reverse proxy. It provides an interactive browser terminal (`xterm.js`), an SFTP file manager, and background cross-cluster `rsync` file transfers.

Every session runs over a standard local SSH connection authenticated as the signed-in POSIX user using an ephemeral 5-minute SSH certificate generated in the browser via WebCrypto—ensuring Unix file permissions, group memberships, and storage quotas are enforced identically to a regular SSH login. See [Cluster Agents: Web Access Gateway](../dev/agents.md#web-access-gateway) for the security architecture.

**Target hosts:** Master and `login`. **Estimated time:** 25 minutes.

## 1. Authorize the gateway on the master

The gateway authenticates to the master's `/sync/*` endpoint using a client certificate, and verifies browser session tickets using an Ed25519 key pair (`PLUGIN_WEB_ACCESS_TICKET_KEY`).

### Container master

In `deploy/compose/master` on the master host, issue a client certificate for `cluster1-login` and enable the `web-access` plugin:

```sh
sh ../../certs/issue.sh client secrets/certs cluster1-login
FP=$(openssl x509 -in secrets/certs/clients/cluster1-login.crt -outform DER | sha256sum | cut -d' ' -f1)
cat >> .env <<ENV
PLUGIN_WEB_ACCESS_TICKET_KEY=$(openssl rand -base64 32)
WEB_ACCESS_CONFIG={"ticket-key-id":"web-access-1","gateways":[{"fingerprint":"$FP","cluster-id":"cluster1","gateway-id":"cluster1-login","instance-id":"cluster1-login"}]}
ENV
sed -i "s/^PLUGINS_ENABLED=.*/&,web-access/" .env
sed -i "s/^SYNC_CLIENT_FINGERPRINTS=.*/&,$FP/" .env
docker compose up -d
```

Copy `secrets/certs/clients/cluster1-login.crt`, `cluster1-login.key`, and `secrets/certs/ca.crt` to `login`.

### Serverless master

1. Create a client certificate for the gateway in the Cloudflare dashboard (**SSL/TLS → Client Certificates**) and compute its SHA-256 fingerprint.
2. Upload the ticket signing seed as a Worker secret:
   `printf '%s' "$(openssl rand -base64 32)" | npx wrangler@4 secret put PLUGIN_WEB_ACCESS_TICKET_KEY -c wrangler.toml`
3. Add `web-access` to `PLUGINS_ENABLED`, append the gateway fingerprint to `SYNC_CLIENT_FINGERPRINTS`, and add the `web-access` configuration block to `PLUGINS_CONFIG` in `wrangler.toml`, then redeploy.

### Retrieve the public ticket verification key

Sign in to the portal as an administrator and fetch `GET /api/v2/web-access/keys`. Copy the base64url `x` value for key ID `web-access-1`—you will place it in `ticket_public_keys` on `login`.

## 2. Install and configure the gateway on `login`

Copy the `gateway` binary built in [Cluster Slave Agent](slaves.md#1-build-the-programs) from `ctl` to `login`, create the `web-access-gateway` service account, and install the mTLS certificates:

```sh
useradd --system --user-group --no-create-home --shell /usr/sbin/nologin web-access-gateway
install -d -m 0755 /usr/local/libexec/go-ldap-admin
install -m 0755 gateway /usr/local/libexec/go-ldap-admin/web-access-gateway
# Container master only: trust the master's private sync CA
cp ca.crt /usr/local/share/ca-certificates/slurm-portal-sync-ca.crt && update-ca-certificates

D=/etc/go-ldap-admin/web-access-gateway
install -d -m 0750 -o root -g web-access-gateway $D
install -m 0640 -o root -g web-access-gateway cluster1-login.crt $D/master-client.crt.pem
install -m 0600 -o web-access-gateway -g web-access-gateway cluster1-login.key $D/master-client.key.pem
```

The gateway listens on loopback (`127.0.0.1:19934`) with TLS, while Nginx terminates public HTTPS on port `443`. Place a local TLS certificate and private key for the upstream loopback listener at `$D/https.crt.pem` and `$D/https.key.pem` (owned by `web-access-gateway`, key mode `0600`), then write `/etc/go-ldap-admin/web-access-gateway.yaml`:

```yaml title="/etc/go-ldap-admin/web-access-gateway.yaml"
gateway_id: cluster1-login
instance_id: cluster1-login
cluster_id: cluster1
listen: "127.0.0.1:19934"
tls_certificate: /etc/go-ldap-admin/web-access-gateway/https.crt.pem
tls_key: /etc/go-ldap-admin/web-access-gateway/https.key.pem
callback_listen: "127.0.0.1:19935"
allowed_origins:
  - https://portal.example.org
ticket_public_keys:
  web-access-1: <the x value from /api/v2/web-access/keys>
user_ca_keys_file: /srv/slurm-portal-trust/current/ssh/trusted-user-ca-keys.pub
file_root: /home
clusters:
  - id: cluster1
    ssh_address: "127.0.0.1:22"
state_directory: /var/lib/go-ldap-admin-web-access-gateway
master_origin: https://portal.example.org:8443
master_client_certificate: /etc/go-ldap-admin/web-access-gateway/master-client.crt.pem
master_client_key: /etc/go-ldap-admin/web-access-gateway/master-client.key.pem
```

*(For a serverless master, set `master_origin: https://sync.portal.example.org`.)*

Start the systemd service and verify that the gateway registers with the master:

```sh
chown web-access-gateway:web-access-gateway /etc/go-ldap-admin/web-access-gateway.yaml
chmod 0600 /etc/go-ldap-admin/web-access-gateway.yaml
cp /root/SlurmPortal/deploy/gateway/web-access-gateway.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now web-access-gateway
curl -sk https://127.0.0.1:19934/readyz        # returns {"code":0,"data":{"status":"ready"}}
```

## 3. Configure Nginx for public HTTPS and WebSockets

Users' browsers connect directly to `https://login.example.org`. Configure Nginx on `login` with a browser-trusted TLS certificate and proxy requests to `https://127.0.0.1:19934` using [deploy/gateway/nginx.conf.example](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/gateway/nginx.conf.example) as a template:

- Include `https://portal.example.org` in the Nginx CORS origin map (otherwise browser preflight requests fail with `"Failed to fetch"`).
- Enable `Upgrade` and `Connection` headers for `/v1/sessions/*/terminal` WebSocket streams.
- See [deploy/gateway/acme/](https://github.com/TING-HiuYu/SlurmPortal/tree/main/deploy/gateway/acme) for automated Let's Encrypt DNS-01 certificate renewal, or [deploy/gateway/container/](https://github.com/TING-HiuYu/SlurmPortal/tree/main/deploy/gateway/container) to run Nginx and the gateway together in a single container.

## 4. Enable web access in the portal

In the portal console, open **Admin → Cluster web access** and configure `cluster1`:

1. Set **Gateway URL** to `https://login.example.org`.
2. Select your **SSH Certificate Authority**.
3. Set the target **SSH host** and **port** (`127.0.0.1` and `22`).
4. Select the **Allowed roles** permitted to use the web terminal and file browser.

Once saved, the cluster status badge changes to **Ready**, and **Web terminal** and **File browser** appear in the sidebar.

## Check it works

Sign in as a portal user, open **Workspace → Web terminal**, and select `cluster1`. An interactive shell opens in `/home/<group>/<user>`, and **File browser** lists the directory contents over SFTP.

| Symptom | What to Check |
| --- | --- |
| `/readyz` reports `registration_unavailable` | The gateway cannot reach `master_origin`, or its certificate fingerprint is missing from the master's allowlist |
| `/readyz` reports `config_mismatch` | `gateway_id`, `instance_id`, or `cluster_id` in `web-access-gateway.yaml` does not match `WEB_ACCESS_CONFIG` / `PLUGINS_CONFIG` |
| Browser shows `"Failed to fetch"` | Nginx CORS map does not include the portal origin, or the browser does not trust `login.example.org`'s TLS certificate |
| `credential_invalid` / `ticket_invalid` | The SSH CA selected in **Cluster web access** is not present in `user_ca_keys_file`, or `ticket_public_keys` has the wrong key ID |

## Next

Proceed to [End-to-End Verification](verify.md).
