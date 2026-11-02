# Public HTTPS renewal

These scripts manage a login node's public HTTPS certificate separately from master mTLS certificates, SSH user CAs, and browser JWT signing keys. Each site uses a root-owned runtime configuration file that targets a single DNS hostname, a single Cloudflare zone, and a single certificate lineage. Never store real API tokens or production private paths in this repository directory.

`cloudflare_dns01.py` handles ACME DNS-01 challenges strictly for the configured `CERTBOT_DOMAIN`. When creating a TXT record, it logs the record ID returned by Cloudflare and removes only that specific ID during cleanup after verifying its name, type, and value. Pre-existing TXT records are never modified or deleted. If a record creation call fails with an uncertain outcome, the hook leaves a journal file for operator inspection rather than blindly creating duplicate records. The Cloudflare credential must be a dedicated zone DNS Edit token stored in a root-owned regular file with mode `0600` or `0400`.

`renew.py` can run either a host-installed Certbot binary or a locally pinned Certbot Docker image. When using Docker, the container mounts only the site's configuration, ACME challenge state, hook scripts, and read-only API token file; it never mounts the host Docker socket or any gateway/master private keys. Existing host Certbot lineages can be selected via `cert_name` and `directories.config`, while unrelated lineages on the host are ignored.

Treat the `*.example` JSON files as templates. Before running in production, fill in the actual file ownership, protected master mTLS paths, immutable Docker image ID (if using Docker), and site-specific reload commands. Native configurations use host paths for the DNS hook, whereas the Docker template uses the fixed mount paths inside the container. Keep all runtime JSON files and state directories owned by `root` with private permissions. The gateway service user must be able to read the deployed TLS key, which stays at mode `0600`. Point the Go gateway and Nginx at the deployed file paths managed by `deploy.py`, never directly at Certbot's `live/` symlinks.

`renew.py` supports four subcommands:

```sh
python3 renew.py /private/site.json issue-only
python3 renew.py /private/site.json check
python3 renew.py /private/site.json issue
python3 renew.py /private/site.json renew
```

- `issue-only` acquires a certificate and verifies its cryptographic properties without touching the running gateway.
- `check` runs a dry-run renewal against the ACME staging server to test DNS-01 automation without deploying the resulting staging certificate.
- `issue` and `renew` obtain or renew a production certificate and then execute the atomic deployment step once Certbot succeeds. ACME account registration requires an explicit contact email or an explicit opt-out.

`deploy.py` validates the candidate private key, certificate chain, Extended Key Usage (`serverAuth`), SAN hostname, and remaining validity window before making any changes. While holding a per-site lock, it backs up the current certificate and key, atomically replaces the two deployed files, sends `SIGHUP` to the Go gateway, and reloads Nginx. Afterward, it connects to both local TLS listeners using the site's SNI to verify that each serves the expected leaf certificate (`DER` SHA-256) and confirms that the Go gateway PID did not change. If post-reload verification fails, `deploy.py` automatically restores the backup files and reloads both services without restarting the Go process. If a rollback itself fails, it exits with an error for operator intervention. Master mTLS certificate paths are explicitly blocklisted and can never be overwritten as deployment targets.

The deployment JSON optionally accepts `public_port` (default `19933`). When set, it must be an integer between `1024` and `65535` other than the Go gateway's internal HTTPS port (`19934`) or completion callback port (`19935`). The containerized gateway example sets `public_port` to `7000`. Note that `public_port` only tells `deploy.py` which local port to probe—it must match your actual Nginx listener configuration. The `endpoints` array must contain two literal loopback addresses (`127.0.0.1` or `::1`), one using `public_port` and the other using `19934`. Any non-loopback address, extra endpoint, callback port, or duplicate port is rejected before files are touched.

In containerized deployments, `reload_gateway.py` locates the single expected Go gateway process, refuses to signal PID 1, and sends `SIGHUP` via a Linux `pidfd`. Run it as the container's unprivileged gateway user so no extra supervisor control socket is needed. Host systemd deployments use `systemctl reload` (`ExecReload`) instead.

Enable the daily systemd timer (`web-access-acme-renew.timer`) only after you have verified a staging `check`, candidate validation, and a live gateway reload by hand. When `renew` runs and the existing certificate is not yet due for renewal, the wrapper still verifies the currently deployed TLS endpoints. Track your Cloudflare API token's expiration date and rotate the token file before it expires, and ensure systemd unit failures alert an operator. Simply enabling the timer does not guarantee that unattended renewal works.

Run the offline unit test suite (requires no credentials or network access):

```sh
python3 -m unittest discover -s deploy/gateway/acme -p 'test_*.py'
```
