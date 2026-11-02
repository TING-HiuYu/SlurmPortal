// Everything below the hero: target audiences, feature tour with alternating screenshots,
// architecture overview, core advantages, quick start, and closing call-to-action.
import clsx from "clsx";
import Link from "@docusaurus/Link";
import Translate, { translate } from "@docusaurus/Translate";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import CodeBlock from "@theme/CodeBlock";
import shots, { architecture } from "./shots";
import s from "./landing.module.css";

function Eyebrow({ children }) {
  return <span className={s.eyebrow}>{children}</span>;
}

function Audiences() {
  const items = [
    {
      title: translate({ id: "aud.admin.title", message: "Cluster Administrators" }),
      body: translate({ id: "aud.admin.body", message: "Provision an account once and let every cluster sync its OpenLDAP directory, Slurm associations, home directories, and SSH trust automatically—no manual LDIF edits across sites." }),
    },
    {
      title: translate({ id: "aud.manager.title", message: "Group & Lab Leads" }),
      body: translate({ id: "aud.manager.body", message: "Delegate user and group management with rank-bounded RBAC that prevents privilege escalation, while keeping real-time visibility into queue activity and GPU utilization." }),
    },
    {
      title: translate({ id: "aud.researcher.title", message: "Researchers & Engineers" }),
      body: translate({ id: "aud.researcher.body", message: "Sign in with GitHub or SSO, download short-lived SSH certificates, launch a browser terminal, transfer datasets across clusters, and receive automated job completion reports." }),
    },
  ];
  return (
    <section className={s.audiences} id="features">
      <Eyebrow>SLURM PORTAL</Eyebrow>
      <h2 className={s.h2}><Translate id="aud.heading">Unified identity, access, and operations across every Slurm cluster</Translate></h2>
      <p className={s.lead}>
        <Translate id="aud.lead">
          Slurm schedules the workloads; Slurm Portal manages the people and credentials behind them. It centralizes users, POSIX groups, passwords, and SSH certificates in a single control plane, replicates state to each cluster over mutual TLS, and gives users a streamlined web workspace.
        </Translate>
      </p>
      <div className={s.cards}>
        {items.map((it) => (
          <div key={it.title} className={s.card3}>
            <span className={s.cardCorner} aria-hidden="true" />
            <h3>{it.title}</h3>
            <p>{it.body}</p>
          </div>
        ))}
      </div>
    </section>
  );
}

function useRows(img) {
  return [
    {
      eyebrow: translate({ id: "f.central.eyebrow", message: "Control Plane" }),
      title: translate({ id: "f.central.title", message: "One control plane, multiple autonomous clusters" }),
      body: translate({ id: "f.central.body", message: "Each cluster runs a lightweight slave agent alongside its local OpenLDAP server. Whenever accounts or credentials change, the master publishes a signed snapshot hash; agents pull and apply only the diff, converging every cluster within a minute while preserving local autonomy if the WAN goes down." }),
      img: img.dashboard,
    },
    {
      eyebrow: translate({ id: "f.identity.eyebrow", message: "Identity & RBAC" }),
      title: translate({ id: "f.identity.title", message: "POSIX accounts, organizational trees, and ranked roles" }),
      body: translate({ id: "f.identity.body", message: "UIDs and GIDs are allocated once and stay consistent across every cluster. Primary POSIX groups define default home directories and login shells, organizational units form a hierarchical tree, and rank-enforced roles ensure administrators can never grant permissions higher than their own." }),
      img: img.users,
    },
    {
      eyebrow: translate({ id: "f.trust.eyebrow", message: "Zero-Password SSH" }),
      title: translate({ id: "f.trust.title", message: "Built-in SSH & X.509 certificate authorities" }),
      body: translate({ id: "f.trust.body", message: "Replace static SSH keys and shared passwords with 30-day OpenSSH user certificates signed by the portal CA. Revocations are compiled into OpenSSH Key Revocation Lists (KRLs) and atomically published to every login node." }),
      img: img.pki,
    },
    {
      eyebrow: translate({ id: "f.live.eyebrow", message: "Live Telemetry" }),
      title: translate({ id: "f.live.title", message: "Real-time cluster capacity and job queues" }),
      body: translate({ id: "f.live.body", message: "Monitor available CPUs, memory, and GPUs across all clusters alongside a searchable job queue showing owners, states, resource allocations, and runtime. Status snapshots are served directly from S3/R2 object storage via presigned URLs for zero database overhead." }),
      img: img.resources,
    },
    {
      eyebrow: translate({ id: "f.nodes.eyebrow", message: "Infrastructure" }),
      title: translate({ id: "f.nodes.title", message: "Node health and partition status at a glance" }),
      body: translate({ id: "f.nodes.body", message: "Inspect per-node states—idle, allocated, mixed, draining, or down—together with login endpoints and heartbeat timestamps for every registered cluster." }),
      img: img.clusters,
    },
    {
      eyebrow: translate({ id: "f.web.eyebrow", message: "Browser Workspace" }),
      title: translate({ id: "f.web.title", message: "Web terminal, SFTP file manager, and cross-cluster transfers" }),
      body: translate({ id: "f.web.body", message: "A login-node gateway establishes real SSH sessions as the authenticated user using ephemeral 5-minute WebCrypto certificates—enabling in-browser shell access, SFTP file browsing, and background rsync transfers between clusters." }),
      img: img.terminal,
    },
    {
      eyebrow: translate({ id: "f.notify.eyebrow", message: "Notifications" }),
      title: translate({ id: "f.notify.title", message: "Customizable email templates and multi-provider mail pools" }),
      body: translate({ id: "f.notify.body", message: "Deliver job completion summaries, transfer alerts, and password reset codes through SMTP or REST mail APIs. Group providers into load-balanced pools with daily quotas, weighted routing, automatic failover, and live template previews." }),
      img: img.mail,
    },
    {
      eyebrow: translate({ id: "f.observe.eyebrow", message: "Observability & Audit" }),
      title: translate({ id: "f.observe.title", message: "Live dependency topology and tamper-evident audit trail" }),
      body: translate({ id: "f.observe.body", message: "Visualize the health of databases, cluster sync agents, gateways, and mail providers on a single diagnostics canvas, backed by a SHA-256 hash-chained audit log that records every state change and permission denial." }),
      img: img.diagnostics,
    },
  ];
}

function Tour() {
  const { i18n } = useDocusaurusContext();
  const rows = useRows(shots(i18n.currentLocale));
  return (
    <section className={s.tour}>
      {rows.map((r, i) => (
        <article key={r.title} className={clsx(s.row, i % 2 === 1 && s.rowFlip)}>
          <div className={s.rowText}>
            <Eyebrow>{r.eyebrow}</Eyebrow>
            <h3>{r.title}</h3>
            <p>{r.body}</p>
          </div>
          <figure className={s.shot}>
            <span className={s.clip} aria-hidden="true" />
            <img src={r.img} alt={r.title} loading="lazy" width="1440" height="900" />
          </figure>
        </article>
      ))}
    </section>
  );
}

function Architecture() {
  return (
    <section className={s.arch}>
      <Eyebrow><Translate id="arch.eyebrow">Architecture</Translate></Eyebrow>
      <h2 className={s.h2}><Translate id="arch.heading">One Go codebase: serverless on Cloudflare or self-hosted in Docker</Translate></h2>
      <p className={s.lead}>
        <Translate id="arch.lead">
          The master compiles to WebAssembly to run on Cloudflare Workers with D1 and R2, or builds as a native Linux binary running in Docker Compose with SQLite, RustFS, and Caddy. Cluster slave agents, web gateways, and Slurm lifecycle hooks work identically with either deployment model.
        </Translate>
      </p>
      <img className={s.archImg} src={architecture} alt={translate({ id: "arch.alt", message: "Slurm Portal architecture" })} loading="lazy" />
      <p className={s.center}>
        <Link className={s.cta} to="/docs/dev/architecture"><Translate id="arch.more">Explore the architecture</Translate></Link>
      </p>
    </section>
  );
}

function Advantages() {
  const items = [
    [translate({ id: "adv.1.t", message: "Offline-resilient clusters" }), translate({ id: "adv.1.b", message: "Each cluster maintains its own local OpenLDAP and SSH trust store, so logins and job submissions continue uninterrupted even if the portal is unreachable." })],
    [translate({ id: "adv.2.t", message: "Short-lived certificates" }), translate({ id: "adv.2.b", message: "30-day SSH certificates with KRL revocation, 5-minute ephemeral certificates for browser sessions, and Argon2id / SHA-512 crypt password hashing." })],
    [translate({ id: "adv.3.t", message: "Dual deployment runtime" }), translate({ id: "adv.3.b", message: "Run zero-maintenance serverless infrastructure on Cloudflare Workers, or self-host the entire stack on your own hardware with Docker Compose." })],
    [translate({ id: "adv.4.t", message: "Defense in depth" }), translate({ id: "adv.4.b", message: "Mandatory route-level authorization, anti-escalation role ranks, hash-chained audit logs, and slave safety guards that block destructive mass deletions." })],
    [translate({ id: "adv.5.t", message: "Minimal operational cost" }), translate({ id: "adv.5.b", message: "Agents poll via lightweight DNS TXT records and download snapshots only on change, while browsers read live telemetry directly from object storage." })],
  ];
  return (
    <section className={s.adv}>
      <Eyebrow><Translate id="adv.eyebrow">Why Slurm Portal</Translate></Eyebrow>
      <div className={s.advGrid}>
        {items.map(([t, b], i) => (
          <div key={t} className={s.advItem}>
            <span className={s.advNum}>{String(i + 1).padStart(2, "0")}</span>
            <h3>{t}</h3>
            <p>{b}</p>
          </div>
        ))}
      </div>
    </section>
  );
}

const quick = `git clone https://github.com/TING-HiuYu/SlurmPortal.git && cd SlurmPortal
cd deploy/compose/master
./init.sh localhost                      # generates secrets and prints the initial admin password
./add-cluster.sh cluster1 "Cluster 1"
docker compose up -d                     # open https://localhost in your browser`;

function QuickStart() {
  return (
    <section className={s.quick}>
      <div className={s.quickText}>
        <Eyebrow><Translate id="quick.eyebrow">Quick Start</Translate></Eyebrow>
        <h2 className={s.h2}><Translate id="quick.heading">Spin up a local instance in minutes</Translate></h2>
        <p>
          <Translate id="quick.body">
            With Docker and Docker Compose installed, these commands launch a complete master stack with HTTPS, SQLite, and S3 status storage. Pair it with the demo slave and OpenLDAP container to see new accounts sync within a minute.
          </Translate>
        </p>
        <Link className={clsx(s.cta, s.ctaDark)} to="/docs/deploy/master-container"><Translate id="quick.more">Read the container deployment guide</Translate></Link>
      </div>
      <div className={s.quickCode}>
        <CodeBlock language="bash">{quick}</CodeBlock>
      </div>
    </section>
  );
}

function Closing() {
  return (
    <section className={s.closing}>
      <div className={s.closingBox}>
        <span className={s.sectionLabel}>NEXT STEPS</span>
        <h2><Translate id="closing.heading">Ready to deploy Slurm Portal?</Translate></h2>
        <div className={s.closingLinks}>
          <Link className={clsx(s.cta, s.ctaLight)} to="/docs/deploy"><Translate id="closing.deploy">Deployment Wiki</Translate></Link>
          <Link className={clsx(s.cta, s.ctaLight)} to="/docs/dev"><Translate id="closing.dev">Developer Wiki</Translate></Link>
          <a className={clsx(s.cta, s.ctaLight)} href="https://github.com/TING-HiuYu/SlurmPortal">GitHub</a>
        </div>
      </div>
    </section>
  );
}

export default function Features() {
  return (
    <main>
      <Audiences />
      <Tour />
      <Architecture />
      <Advantages />
      <QuickStart />
      <Closing />
    </main>
  );
}
