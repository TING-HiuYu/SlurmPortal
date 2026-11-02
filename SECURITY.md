# Security policy

Slurm Portal manages accounts, passwords and SSH certificate authorities for HPC clusters, so
security reports get priority.

## Reporting a vulnerability

Please **do not** open a public issue. Use GitHub's private vulnerability reporting:
**Security → Report a vulnerability** on this repository. If that is not available to you,
contact the maintainer ([@TING-HiuYu](https://github.com/TING-HiuYu)) privately first and ask
for a secure channel.

Include:

- the affected component (portal, slave, gateway, lifecycle hook, deploy script) and commit;
- steps to reproduce, or a proof of concept;
- the impact as you understand it.

You will get an acknowledgement within 7 days. Please give a reasonable time for a fix before any
public disclosure.

## Supported versions

Only the current `main` branch is supported. go-ldap-admin v1 (tag `v1-final`) is retired and
gets no fixes.

## Scope

How the system is protected is described in [docs/security.md](docs/dev/security.md). Problems in
Cloudflare, OpenLDAP, OpenSSH or Slurm themselves should go to those projects.
