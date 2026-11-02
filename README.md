# Slurm Portal

[![Website](https://img.shields.io/badge/website-docs%20%26%20wiki-3e5fd8)](https://ting-hiuyu.github.io/SlurmPortal/)
[![CI](https://github.com/TING-HiuYu/SlurmPortal/actions/workflows/ci.yml/badge.svg)](https://github.com/TING-HiuYu/SlurmPortal/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)
![Runs on](https://img.shields.io/badge/runs%20on-Cloudflare%20or%20Docker-F38020)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

Slurm Portal manages user accounts, groups, and SSH access across multiple Slurm HPC clusters from a single web portal. A lightweight agent on each cluster syncs users and POSIX groups into that cluster's local OpenLDAP directory and updates its SSH trust files. Users also get a browser terminal, cross-cluster file transfers, job completion emails, and live cluster telemetry.

![Sign-in page](docs/images/login.png)

## Advantage

- **One portal for every cluster.** Manage users, groups, and SSH certificates in one place and replicate them to each cluster's local OpenLDAP directory. UIDs, GIDs, and access rules stay consistent across your site, and each cluster keeps working independently if the portal is temporarily unreachable.
- **Serverless or self-hosted.** Run the same Go codebase as WebAssembly on Cloudflare Workers (with D1, R2, and Cloudflare Access) with no servers to maintain, or self-host it with Docker Compose on Linux.
- **Low overhead.** Cluster agents poll a compact snapshot hash and download the full snapshot only when data changes. Browsers fetch live cluster telemetry directly from object storage, keeping idle resource usage near zero.

## What it does

- **Accounts and groups.** Manage users, POSIX groups, organizations, and RBAC roles centrally while each cluster's OpenLDAP stays in sync. UIDs and GIDs are allocated once and remain consistent across all clusters.
- **SSH certificates.** A built-in SSH certificate authority issues 30-day user certificates so users can log in with SSH keys instead of passwords. Certificate revocations reach every login node through an OpenSSH Key Revocation List (KRL).
- **Sign-in.** Supports password login, GitHub OAuth, and Cloudflare Access. Users can reset forgotten passwords with an emailed verification code. Only one-way password hashes are stored.
- **Web access.** A gateway on each login node provides a browser terminal, file browser, and cross-cluster `rsync` transfers over SSH using 5-minute user certificates.
- **Status and usage.** Displays node states, available CPU, memory, and GPU capacity, the active job queue, and 7-day per-user GPU usage across all clusters on a single page.
- **Notifications.** Sends job completion, file transfer, and password reset emails through SMTP or HTTP mail providers, with provider pools, quotas, and delivery counters.
- **Operations.** Includes a hash-chained audit log, a dependency diagnostics view, and rank-constrained RBAC roles that prevent users from granting permissions they do not hold.

| Console overview | Current resources |
| --- | --- |
| ![Console overview](docs/images/dashboard.png) | ![Current resources](docs/images/resources.png) |
| **Cluster status** | **Service diagnostics** |
| ![Cluster status](docs/images/clusters.png) | ![Diagnostics](docs/images/diagnostics.png) |

## How it fits together

![Architecture](docs/images/architecture.png)

- **Master** is the central portal and control plane. You can run it in two ways:
  - **SaaS:** compiled to WebAssembly on Cloudflare Workers, backed by D1, R2, and Cloudflare Access.
  - **Containers:** running natively in Docker Compose with SQLite, an S3-compatible store (RustFS), and Caddy for HTTPS.
- **Slave** runs once per cluster alongside OpenLDAP. When the master's snapshot hash changes, the slave downloads the new snapshot over mutual TLS and:
  - applies the user and group diff to OpenLDAP;
  - writes the SSH CA public keys and revocation list read by `sshd`;
  - runs local provisioning triggers, such as creating Slurm accounting associations and home directories.
- **Gateway** runs on each login node and handles browser terminal sessions, file browsing, and transfers over SSH.
- **Lifecycle hook** runs from the Slurm controller's `PrologSlurmctld` and `EpilogSlurmctld` hooks to report job events for email notifications and GPU usage accounting.

See the [architecture guide](docs/dev/architecture.md) for a detailed overview, or explore the [interactive architecture diagram](https://ting-hiuyu.github.io/SlurmPortal/architecture.html).

## Deploy

The [deployment wiki](docs/deploy/README.md) walks through setting up a full cluster from fresh Ubuntu 24.04 servers—covering Munge, Slurm, accounting, `slurmrestd`, OpenLDAP, SSSD, and NFS—before deploying the portal. If you already run Slurm with OpenLDAP, you can start directly with the portal guides:

| Component | Guide |
| --- | --- |
| Master on Cloudflare (Worker, D1, R2, Access) | [Serverless master](docs/deploy/master-saas.md) |
| Master in containers (Docker Compose: master, SQLite, RustFS, Caddy) | [Container master](docs/deploy/master-container.md) |
| Slave agent on each cluster (works with either master) | [Cluster slave agent](docs/deploy/slaves.md) |
| Web terminal, file browser, job emails, and GPU usage | [Optional parts](docs/deploy/optional.md) and [Web access gateway](docs/deploy/gateway.md) |

## Try it on one machine

With Docker and the Compose plugin installed, you can start a local master and a slave synced to a demo OpenLDAP container:

```sh
git clone https://github.com/TING-HiuYu/SlurmPortal.git && cd SlurmPortal

cd deploy/compose/master
./init.sh localhost                      # prints the initial admin password
./add-cluster.sh cluster1 "Cluster 1"
docker compose up -d                     # open https://localhost (accept Caddy's local CA)

cd ../slave
./init.sh
cp ../master/secrets/clusters/cluster1/* secrets/
openssl rand -base64 24 > secrets/ldap-password && chmod 600 secrets/*
```

Update `deploy/compose/slave/slave.yml` with the settings below, then run `docker compose --profile demo up -d`:

```yaml
master-url: https://localhost:8443
dns-name: ""
cluster-id: cluster1
tls:
  server-name: localhost
  ca-file: /etc/slurm-portal/secrets/master-ca.crt
ldap:
  url: ldap://127.0.0.1:1389
```

Any user account you create in the portal will sync to the demo OpenLDAP server within a minute.

## Develop

Building and testing from source requires Go 1.26 and Node.js 22:

```sh
make check                               # gofmt, vet, Go tests, JS tests, web build, Wasm build
```

See the [development guide](docs/dev/development.md) for local setup instructions and coding conventions.

## Documentation

| Document | Contents |
| --- | --- |
| [Deployment](docs/deploy/README.md) | Choosing a master runtime and step-by-step installation guides |
| [Architecture](docs/dev/architecture.md) | System components, request pipeline, sync protocol, and storage layers |
| [Plugins](docs/dev/plugins.md) | Plugin architecture, built-in plugins, and how to write new ones |
| [Project layout](docs/dev/project-layout.md) | Directory structure of the repository |
| [Development](docs/dev/development.md) | Local development setup, test suites, and code conventions |
| [Security model](docs/dev/security.md) | Authentication, authorization, machine identity, and secret storage |
| [Operations](docs/deploy/operations.md) | Routine administration, backups, key rotation, and troubleshooting |

## Acknowledgements

Slurm Portal began as a fork of [go-ldap-admin](https://github.com/eryajf/go-ldap-admin) by [eryajf](https://github.com/eryajf) and its contributors. Its user, group, and role data model, LDAP schema layout, RBAC design, and initial web console all originated in `go-ldap-admin`, and Slurm Portal can still upgrade an existing `go-ldap-admin` v1 database in place. While v2 rewrote the application for Cloudflare Workers and containers and added the multi-cluster agents, `go-ldap-admin` remains the foundation of the project. Thank you to the original authors.

## License

Licensed under the [Apache License 2.0](LICENSE), the same license as `go-ldap-admin`.
