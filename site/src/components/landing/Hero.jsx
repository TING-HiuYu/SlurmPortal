// The landing hero. Its visual language (black label boxes, crosshairs, a tilted card and a skewed
// link panel, staged reveal) follows the Rhine Lab theme index page by QingXia-Ela; the layout and
// content are our own and use no Arknights artwork.
import { useEffect, useState } from "react";
import clsx from "clsx";
import Link from "@docusaurus/Link";
import useBaseUrl from "@docusaurus/useBaseUrl";
import Translate, { translate } from "@docusaurus/Translate";
import useDocusaurusContext from "@docusaurus/useDocusaurusContext";
import shots from "./shots";
import { Book, GitHub, Pen, Rocket, Code } from "./Icons";
import s from "./landing.module.css";

const repo = "https://github.com/TING-HiuYu/SlurmPortal";

// The links shown together on the screenshot panel.
function usePanelLinks() {
  return [
    { to: "/docs/deploy/master-container", icon: Book, title: translate({ id: "hero.link.quickstart", message: "Quick start" }), note: translate({ id: "hero.link.quickstart.note", message: "Try it on one machine" }) },
    { to: "/docs/deploy", icon: Rocket, title: translate({ id: "hero.link.deploy", message: "Deployment guide" }), note: translate({ id: "hero.link.deploy.note", message: "From munge to a working portal" }) },
    { to: "/docs/dev", icon: Code, title: translate({ id: "hero.link.dev", message: "Developer wiki" }), note: translate({ id: "hero.link.dev.note", message: "Layers, protocols, API" }) },
  ];
}

function PanelLinks({ links }) {
  return (
    <div className={s.carousel}>
      {links.map((l) => {
        const Icon = l.icon;
        return (
          <Link key={l.title} className={s.carouselItem} to={l.to}>
            <span className={s.dot} data-pos="tl" /><span className={s.dot} data-pos="tr" />
            <span className={s.dot} data-pos="bl" /><span className={s.dot} data-pos="br" />
            <Icon />
            <span className={s.carouselTitle}>{l.title}</span>
            <span className={s.carouselNote}>{l.note}</span>
          </Link>
        );
      })}
    </div>
  );
}

export default function Hero() {
  const { i18n } = useDocusaurusContext();
  const img = shots(i18n.currentLocale);
  const logo = useBaseUrl("/img/logo.svg");
  const links = usePanelLinks();
  const [ready, setReady] = useState(false);
  useEffect(() => {
    const id = requestAnimationFrame(() => setReady(true));
    return () => cancelAnimationFrame(id);
  }, []);

  return (
    <header className={clsx(s.hero, ready && s.ready)}>
      <span className={clsx(s.cross, s.c1)} aria-hidden="true" />
      <span className={clsx(s.cross, s.c2)} aria-hidden="true" />
      <span className={clsx(s.cross, s.c3)} aria-hidden="true" />

      <div className={s.left}>
        <span className={clsx(s.tag, s.stage1)}><Translate id="hero.tag">Jump to...</Translate></span>
        <nav className={clsx(s.iconLinks, s.stage2)} aria-label={translate({ id: "hero.quicklinks", message: "Quick links" })}>
          <a href={repo}><GitHub /><span>{translate({ id: "hero.link.github", message: "GitHub" })}</span></a>
          <a href="https://blog.thy.icu"><Pen /><span>{translate({ id: "hero.link.blog", message: "Blog" })}</span></a>
        </nav>

        <div className={clsx(s.section, s.stage3)}>
          <span className={s.sectionLabel}><Translate id="hero.adv.label">Advantages</Translate></span>
          <ul className={s.sectionList}>
            <li><Translate id="hero.adv.1">Serverless deployment supported</Translate></li>
            <li><Translate id="hero.adv.2">Unified center for multiple clusters</Translate></li>
            <li><Translate id="hero.adv.3">Granular RBAC</Translate></li>
            <li><Translate id="hero.adv.4">OAuth supported</Translate></li>
            <li><Translate id="hero.adv.5">Immutable audit logs</Translate></li>
            <li><Translate id="hero.adv.6">Readable diagnostic tracing</Translate></li>
          </ul>
        </div>

        <p className={clsx(s.copyright, s.stage4)}>
          <span className={s.bar} />© 2026 SLURM PORTAL · APACHE-2.0
        </p>
      </div>

      <div className={s.middle}>
        <div className={clsx(s.card, s.stage2)}>
          <img src={logo} alt="" width="120" height="120" />
          <p className={s.cardSpaced}>S L U R M &nbsp; P O R T A L</p>
          <div className={s.cardBand} />
        </div>
        <div className={clsx(s.banner, s.stage3)}>
          <span className={s.bannerLabel}><Translate id="hero.banner.label">RELEASE</Translate></span>
          <p className={s.bannerTitle}>SLURM PORTAL 2.0</p>
          <ul>
            <li><Translate id="hero.banner.l2">Open source · Apache-2.0</Translate></li>
          </ul>
        </div>
        <div className={clsx(s.ctaRow, s.stage4)}>
          <Link className={clsx(s.cta, s.ctaDark)} to="/docs/deploy"><Translate id="hero.cta.deploy">Deploy it</Translate></Link>
          <Link className={s.cta} to="/docs/dev"><Translate id="hero.cta.dev">Read the design</Translate></Link>
        </div>
      </div>

      <div className={s.right}>
        <div className={clsx(s.panel, s.stage2)} style={{ backgroundImage: `url(${img.dashboard})` }}>
          <PanelLinks links={links} />
        </div>
        <div className={clsx(s.director, s.stage4)}>
          <span><Translate id="hero.director.label">FOR</Translate></span>
          <p><Translate id="hero.director.title">HPC TEAMS</Translate></p>
        </div>
        <p className={clsx(s.powered, s.stage4)}>
          POWERED BY <b>DOCUSAURUS</b> | STYLE AFTER <a href="https://github.com/QingXia-Ela/Rhine-Lab-Theme-Index-Page">RHINE LAB THEME</a>
          <span className={s.bar} />
        </p>
      </div>

      <a className={clsx(s.scrollHint, s.stage4)} href="#features">
        <Translate id="hero.scroll">SCROLL · FEATURES</Translate>
        <span aria-hidden="true">▾</span>
      </a>
    </header>
  );
}
