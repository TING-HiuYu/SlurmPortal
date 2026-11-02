// Custom 404 page styled to match the landing page aesthetic.
import React from "react";
import Link from "@docusaurus/Link";
import Translate from "@docusaurus/Translate";
import Heading from "@theme/Heading";
import styles from "@site/src/components/landing/landing.module.css";
import notFound from "./styles.module.css";

export default function NotFoundContent() {
  return (
    <main className={notFound.page}>
      <div className={notFound.label}>ERROR 404 · PAGE NOT FOUND</div>
      <Heading as="h1" className={notFound.title}>
        <Translate id="notfound.title">This page could not be found</Translate>
      </Heading>
      <p className={notFound.lead}>
        <Translate id="notfound.lead">The URL you requested may have moved or no longer exists. Use one of the links below to navigate the documentation.</Translate>
      </p>
      <div className={styles.ctaRow}>
        <Link className={`${styles.cta} ${styles.ctaDark}`} to="/">
          <Translate id="notfound.home">Home</Translate>
        </Link>
        <Link className={styles.cta} to="/docs/dev">
          <Translate id="notfound.dev">Developer Wiki</Translate>
        </Link>
        <Link className={styles.cta} to="/docs/deploy">
          <Translate id="notfound.deploy">Deployment Wiki</Translate>
        </Link>
      </div>
    </main>
  );
}
