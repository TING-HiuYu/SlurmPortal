# Documentation website

The website uses Docusaurus and reads the English wiki pages from `../docs/`.

## Build

From this directory:

```sh
npm ci
BASE_URL=https://thy.icu/sth/ npm run build
```

`BASE_URL` is the public **parent URL**, including `https://`. The build appends
`SlurmPortal/`, so this example produces a site at
`https://thy.icu/sth/SlurmPortal/`. A trailing slash on the input is optional.
Without `BASE_URL`, the site keeps its existing GitHub Pages address:
`https://ting-hiuyu.github.io/SlurmPortal/`.

The value is read during compilation, not at runtime. It sets the public origin
and path used by navigation, assets, locale links, search, canonical URLs and the
sitemap. Rebuild whenever the public address changes.

## Cloudflare and reverse proxies

For a Cloudflare Pages build, set:

- Root directory: `site`
- Build command: `npm run build`
- Build output directory: `build`
- Build environment variable: `BASE_URL=https://thy.icu/sth/`

The build output itself is flat: `build/index.html`, `build/assets/`, and so on.
Your hosting or proxy configuration must map the public prefix
`/sth/SlurmPortal/` to that output directory. Setting `BASE_URL` does not create
a Cloudflare route or mount the files at that path automatically.

If a reverse proxy strips the prefix before forwarding to a static origin,
keep `BASE_URL` set to the external URL, not the origin's address. Forward the
whole prefix, including asset requests and the `zh-Hans/` locale. Direct requests
to documentation pages must also resolve to their generated HTML files; an
SPA fallback to the homepage is not a substitute for serving those files.
Avoid origin redirects that send visitors outside the public prefix.

For example, the build above expects these public URLs to work:

- `/sth/SlurmPortal/`
- `/sth/SlurmPortal/docs/dev`
- `/sth/SlurmPortal/zh-Hans/`
- `/sth/SlurmPortal/img/logo.svg`

## Check URL configuration

```sh
node --test scripts/site-url.test.mjs
```

Before deploying, also build with the intended `BASE_URL` and check navigation,
locale switching, search, and direct page reloads through the actual proxy.
