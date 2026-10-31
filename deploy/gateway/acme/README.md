# Public HTTPS renewal

These components manage a site's public HTTPS certificate separately from master
mTLS, SSH user CAs and browser JWT signing keys. A site's root-only runtime
configuration selects exactly one DNS hostname, one Cloudflare zone and one
certificate lineage. Token values and real private runtime paths do not belong
in this directory.

`cloudflare_dns01.py` accepts only the configured `CERTBOT_DOMAIN`. It records
the ID returned by its own TXT creation and cleans up only that ID after
checking its name, type and content. Existing TXT records remain untouched.
An unknown create outcome leaves a journal requiring inspection instead of
blindly creating another record. The token must be a new, explicitly authorized
zone DNS Edit token in a root-owned 0600/0400 regular file.

`renew.py` supports native Certbot and an existing locally pinned Certbot Docker
image. The Certbot container receives only its site's configuration, challenge
state, hooks and read-only token file. It does not receive the host Docker
socket or gateway/main credentials. Existing native lineages can be selected
with `cert_name` and `directories.config`; other lineages are not renewed by
this service.

Use the JSON examples as incomplete templates. Fill the actual runtime file
ownership, protected master mTLS paths, immutable image ID where relevant and
site-specific reload commands. Native DNS hook configuration uses host paths;
the Docker example uses its fixed container mount paths. Keep runtime JSON and
state root-owned/private. The service account must be able to read the ordinary
deployed key, which remains 0600. Do not point the Go server at Certbot's final
`live/` symlinks.

The host wrapper has four explicit actions:

```sh
python3 renew.py /private/site.json issue-only
python3 renew.py /private/site.json check
python3 renew.py /private/site.json issue
python3 renew.py /private/site.json renew
```

`issue-only` obtains and cryptographically checks a certificate without touching
a running gateway. `check` uses ACME staging to test DNS renewal without
deploying a staging certificate. `issue` and `renew` run the deployment
transaction only after Certbot succeeds. Registration requires an explicit
email or no-email choice.

`deploy.py` verifies the key pair, public trust chain, server purpose, hostname
and remaining lifetime. Under a site lock it saves private rollback copies,
atomically replaces two ordinary files, sends Go SIGHUP and reloads nginx. It
then verifies the actual leaf DER SHA-256 through both local TLS endpoints with
the site's SNI and confirms the Go process identity is unchanged. Failed
activation restores the old files and reloads them; it never restarts Go. A
failed rollback is reported for operator inspection. Master mTLS paths are an
explicit protected list and cannot be deployment targets.

The root-only deployment JSON may set `public_port`; omitting it preserves the
existing default of `19933`. It must be an integer from `1024` through `65535`,
excluding Go HTTPS `19934` and the completion callback `19935`. The container
example uses the alternate public port `7000`. This value must match the site's
actual Nginx listener; it does not change the listener itself. `endpoints` must
still contain exactly two literal loopback addresses (`127.0.0.1` or `::1`),
with one port equal to `public_port` and the other equal to `19934`. Remote
hosts, additional endpoints, callback probes and duplicate ports are rejected
before any certificate replacement or reload.

For containers, `reload_gateway.py` finds exactly one expected Go executable,
refuses PID1 and uses a Linux pidfd to send HUP. Run it as the container's normal
gateway user. It does not introduce another process-control service. Systemd
installations use the gateway unit's `ExecReload` instead.

Only enable the provided daily timer after a real staging renewal, candidate
validation and actual gateway reload have succeeded. The wrapper also checks
currently deployed TLS when no new certificate is needed. Record the DNS
token's expiry and rotate its private file before that date; service failure
must be visible to the operator. Installing a timer alone is not proof of
working automatic renewal.

Run the offline tests without credentials or public DNS writes:

```sh
python3 -m unittest discover -s deploy/gateway/acme -p 'test_*.py'
```
