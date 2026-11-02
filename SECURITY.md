# Security policy

Because Slurm Portal manages POSIX accounts, passwords, and SSH certificate authorities for HPC clusters, security vulnerabilities are treated with high priority.

## Reporting a vulnerability

Please **do not** report security vulnerabilities in public GitHub issues. Use GitHub's private vulnerability reporting feature under **Security → Report a vulnerability** on this repository. If private reporting is unavailable to you, contact the maintainer ([@TING-HiuYu](https://github.com/TING-HiuYu)) privately to arrange a secure channel before sharing details.

Please include:

- the affected component (portal master, slave, gateway, lifecycle hook, or deployment script) and the commit SHA;
- clear steps to reproduce the issue or a working proof of concept;
- your assessment of the potential impact.

You will receive an acknowledgment within 7 days. Please allow reasonable time for a fix to be developed and released before disclosing the issue publicly.

## Supported versions

Only the current `main` branch is supported with security updates. The legacy `go-ldap-admin` v1 release line (tag `v1-final`) is retired and no longer receives fixes.

## Scope

The security architecture and trust boundaries of Slurm Portal are documented in [docs/dev/security.md](docs/dev/security.md). Vulnerabilities in upstream dependencies or infrastructure—such as Cloudflare, OpenLDAP, OpenSSH, or Slurm itself—should be reported directly to those respective projects.
