# Acknowledgements

Slurm Portal builds on the open-source work of many projects and contributors.

## go-ldap-admin

Slurm Portal began as a fork of [go-ldap-admin](https://github.com/eryajf/go-ldap-admin) created by [eryajf](https://github.com/eryajf) and its contributors. Its core account model, initial database schema (which v2 still upgrades seamlessly), and vision of managing OpenLDAP from a clean web console laid the foundation for this project. We are grateful for their work.

## Open-Source Dependencies

| Project | Role in Slurm Portal |
| --- | --- |
| [Slurm](https://slurm.schedmd.com/) (SchedMD) | The HPC workload manager and scheduling system integrated with this portal |
| [OpenLDAP](https://www.openldap.org/), [SSSD](https://sssd.io/), [OpenSSH](https://www.openssh.com/) | Cluster directory services, node-level identity resolution, and certificate-based SSH authentication |
| [Go](https://go.dev/) (`go-ldap`, `golang-jwt`, `casbin`, `modernc.org/sqlite`, `x/crypto`) | Master control plane, cluster slave agent, Web Access Gateway, and lifecycle daemons |
| [Cloudflare Workers](https://workers.cloudflare.com/), D1, R2 | Serverless runtime, distributed SQLite database, and object storage |
| [Caddy](https://caddyserver.com/), [RustFS](https://rustfs.com/) | Automatic HTTPS reverse proxy and S3-compatible object storage in the container stack |
| [React](https://react.dev/), [Ant Design](https://ant.design/), [xterm.js](https://xtermjs.org/), [Vite](https://vite.dev/) | Browser console, workspace UI, and interactive web terminal |
| [Docusaurus](https://docusaurus.io/) | Documentation website and localized search |
| [Archify](https://github.com/tt-a1i/archify) | Interactive architecture diagram renderer |

## Documentation Website Design

The landing page aesthetic is inspired by [Rhine-Lab-Theme-Index-Page](https://github.com/QingXia-Ela/Rhine-Lab-Theme-Index-Page) by QingXia-Ela (MIT License); no third-party artwork or visual assets from that project are used. Typography uses [Michroma](https://fonts.google.com/specimen/Michroma), [Saira](https://fonts.google.com/specimen/Saira), and [Noto Sans SC](https://fonts.google.com/noto/specimen/Noto+Sans+SC) under the SIL Open Font License.

## Maintainer

Slurm Portal is maintained by [Suzuki coupling](https://github.com/TING-HiuYu). Engineering notes and articles are published at [blog.thy.icu](https://blog.thy.icu).
