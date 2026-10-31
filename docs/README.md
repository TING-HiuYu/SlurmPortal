# Slurm Portal documentation

Start with the [project README](../README.md) for what Slurm Portal is. These pages go deeper.

**Deploying**

| Page | Read it when you want to… |
| --- | --- |
| [deployment.md](deployment.md) | choose between the two ways to run the master, and see the order of steps |
| [deploy-saas-master.md](deploy-saas-master.md) | run the master on Cloudflare (Worker, D1, R2, Access) |
| [deploy-container-master.md](deploy-container-master.md) | run the master on your own host with Docker Compose |
| [deploy-slaves.md](deploy-slaves.md) | connect a cluster: the slave container, sshd trust, triggers, status |

**Understanding and changing it**

| Page | Read it when you want to… |
| --- | --- |
| [architecture.md](architecture.md) | understand the components, how a request is handled and how clusters stay in sync |
| [plugins.md](plugins.md) | enable or configure a feature, or write a new plugin |
| [project-layout.md](project-layout.md) | find where something lives in the repository |
| [development.md](development.md) | run it locally, run the tests, follow the code conventions |
| [security.md](security.md) | know what protects each boundary |
| [operations.md](operations.md) | do everyday admin tasks or troubleshoot |
| [architecture.html](architecture.html) | explore the architecture diagram interactively (open it in a browser) |

Notes for each deployment part live next to its files in [`deploy/`](../deploy/README.md).
