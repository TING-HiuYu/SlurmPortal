// Project home page: Hero section followed by the product feature tour.
import Layout from "@theme/Layout";
import { translate } from "@docusaurus/Translate";
import Hero from "../components/landing/Hero";
import Features from "../components/landing/Features";

export default function Home() {
  return (
    <Layout
      title={translate({ id: "home.title", message: "Unified Identity and Access Portal for Slurm HPC Clusters" })}
      description={translate({ id: "home.description", message: "Centralize POSIX accounts, groups, SSH certificates, live telemetry, and browser terminal access across multiple Slurm clusters—deployed serverless on Cloudflare or self-hosted with Docker." })}
    >
      <Hero />
      <Features />
    </Layout>
  );
}
