// Two wikis over the repository's docs/ folder. Page IDs correspond to file paths without ".md";
// a folder's README.md serves as its index (e.g., id "dev/README" is served at /docs/dev).

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  dev: [
    "dev/README",
    {
      type: "category",
      label: "Core Concepts",
      collapsed: false,
      items: ["dev/architecture", "dev/request-lifecycle", "dev/sync-protocol"],
    },
    {
      type: "category",
      label: "Architecture Layers",
      collapsed: false,
      items: [
        "dev/layer-http",
        "dev/layer-access",
        "dev/layer-domain",
        "dev/plugins",
        "dev/data-model",
        "dev/layer-storage",
      ],
    },
    {
      type: "category",
      label: "Agents & Frontend",
      collapsed: false,
      items: ["dev/agents", "dev/web-app"],
    },
    {
      type: "category",
      label: "Security & Cryptography",
      items: ["dev/security", "dev/cryptography"],
    },
    {
      type: "category",
      label: "Reference & Workflow",
      items: ["dev/api-reference", "dev/project-layout", "dev/development"],
    },
  ],
  deploy: [
    "deploy/README",
    "deploy/plan",
    {
      type: "category",
      label: "Cluster Foundation (Ubuntu 24.04)",
      collapsed: false,
      items: [
        "deploy/munge",
        "deploy/slurm",
        "deploy/accounting",
        "deploy/slurmrestd",
        "deploy/openldap",
        "deploy/sssd-ssh",
        "deploy/shared-home",
      ],
    },
    {
      type: "category",
      label: "Slurm Portal Control Plane",
      collapsed: false,
      items: [
        "deploy/master-saas",
        "deploy/master-container",
        "deploy/portal-setup",
        "deploy/slaves",
      ],
    },
    {
      type: "category",
      label: "Optional Cluster Integrations",
      collapsed: false,
      items: ["deploy/optional", "deploy/gateway"],
    },
    "deploy/verify",
    "deploy/operations",
  ],
};

export default sidebars;
