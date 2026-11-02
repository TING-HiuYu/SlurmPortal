# Slurm Portal Documentation

These pages power the published documentation website—including full-text search and a Simplified Chinese translation—at **https://ting-hiuyu.github.io/SlurmPortal/**. For a high-level introduction to the project, start with the [project README](https://github.com/TING-HiuYu/SlurmPortal/blob/main/README.md).

## Deployment Wiki

Step-by-step guides that take you from bare Ubuntu 24.04 servers to a production Slurm cluster managed by Slurm Portal.

| Page | Topics Covered |
| --- | --- |
| [Deployment Overview](deploy/README.md) | System components, serverless vs. container master comparison, and deployment roadmap |
| [Plan Your Site](deploy/plan.md) | Host topology, DNS naming, firewall ports, and POSIX UID/GID ranges |
| [Munge](deploy/munge.md), [Slurm](deploy/slurm.md), [Accounting](deploy/accounting.md), [slurmrestd](deploy/slurmrestd.md) | Building and configuring Slurm 26.05, MariaDB accounting, and the JWT-authenticated REST API |
| [OpenLDAP](deploy/openldap.md), [SSSD & SSH](deploy/sssd-ssh.md), [Shared Home & Trust](deploy/shared-home.md) | Directory services, node-level POSIX identity resolution, and NFSv4 exports for `$HOME` and SSH trust |
| [Serverless Master](deploy/master-saas.md), [Container Master](deploy/master-container.md) | Deploying the Slurm Portal control plane on Cloudflare Workers or Docker Compose |
| [Initial Portal Setup](deploy/portal-setup.md) | Configuring SSH Certificate Authorities, POSIX group presets, RBAC roles, mail providers, and OAuth |
| [Cluster Slave Agent](deploy/slaves.md) | Connecting a cluster to the master, rehearsing sync plans, and enabling account triggers |
| [Job Notifications & GPU Monitoring](deploy/optional.md) | Deploying `slurm-lifecycle-hook`, `slurm-notify-observer`, and the slave lifecycle runtime |
| [Web Access Gateway](deploy/gateway.md) | Deploying the login-node gateway and Nginx reverse proxy for browser terminal, SFTP, and `rsync` |
| [End-to-End Verification](deploy/verify.md) | Validating user provisioning, SSH certificate login, live telemetry, and revocation |
| [Day-2 Operations](deploy/operations.md) | Routine administration, backups, certificate rotation, and troubleshooting |

## Developer Wiki

Architectural and implementation guides explaining how Slurm Portal works across every layer.

| Page | Topics Covered |
| --- | --- |
| [Developer Overview](dev/README.md) | System summary, architecture map, reading guide, and terminology glossary |
| [Architecture](dev/architecture.md) | Binaries, serverless vs. container runtimes, network topology, and serverless cost model |
| [Request Lifecycle](dev/request-lifecycle.md) | End-to-end walkthrough of an HTTP request from the edge to the database |
| [Sync Protocol](dev/sync-protocol.md) | Snapshot generation, hash publishing, slave reconciliation, safety guards, and triggers |
| [HTTP & Edge](dev/layer-http.md), [Identity & Access](dev/layer-access.md), [Domain Modules](dev/layer-domain.md), [Plugins](dev/plugins.md), [Data Model](dev/data-model.md), [Storage](dev/layer-storage.md) | Deep dives into each architectural layer and database schema |
| [Cluster Agents](dev/agents.md), [Web Application](dev/web-app.md) | Slave, Web Access Gateway, Slurm lifecycle hooks, and the React single-page application |
| [Security Model](dev/security.md), [Cryptography & Signing](dev/cryptography.md) | Trust boundaries, authentication flows, key hierarchy, and encryption at rest |
| [API Reference](dev/api-reference.md) | Complete list of HTTP endpoints generated directly from the router |
| [Project Layout](dev/project-layout.md), [Development Guide](dev/development.md) | Repository directory structure, local setup, test suites, and coding conventions |

## Additional Resources

- [FAQ & Troubleshooting](faq.md)
- [Changelog](changelog.md)
- [Acknowledgements](acknowledgements.md)
- [Interactive Architecture Diagram](https://ting-hiuyu.github.io/SlurmPortal/architecture.html)

Deployment templates and helper scripts are documented alongside their configuration files in [`deploy/`](https://github.com/TING-HiuYu/SlurmPortal/blob/main/deploy/README.md).
