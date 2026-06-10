# warden Public Website Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a public marketing landing page + searchable documentation site for warden, deployed to GitHub Pages at `srjn45.github.io/warden`.

**Architecture:** A single Astro 5 + Starlight project in `site/` (separate from the existing `web/` GUI app). A custom landing page (`src/pages/index.astro`) sells warden; Starlight renders the docs tree from `src/content/docs/`. Visuals (VHS terminal GIFs, Playwright/tmux screenshots, hand-authored SVG/Mermaid diagrams) live in `site/public/media/` and are committed. A GitHub Actions workflow builds and deploys to Pages.

**Tech Stack:** Astro 5, `@astrojs/starlight`, npm, GitHub Pages + Actions, VHS (`charmbracelet/vhs`), Playwright (already available via MCP), Mermaid/SVG for diagrams.

**Spec:** `docs/superpowers/specs/2026-06-10-warden-website-design.md`

---

## A note on "tests" for this plan

This is static-site work — there is no unit-test framework for page content. The **verification gate for every task is: `npm run build` succeeds with no broken internal links, plus a Playwright visual smoke where noted.** That is the honest equivalent of "tests pass" here. Commit after each green gate.

## Ordering rationale

Front-load the highest-value, lowest-risk wins and validate the one scary part (GitHub Pages base-path asset resolution) early:
scaffold → docs migration (a working searchable docs site fast) → deploy workflow (get it live, prove the base path) → landing shell → landing sections → media → final polish.

## File structure (locked)

```
site/
  package.json              # Task 1
  astro.config.mjs          # Task 1 (Starlight + base:'/warden'), sidebar grows in Task 3
  tsconfig.json             # Task 1
  .gitignore                # Task 1 (node_modules, dist, .astro)
  src/
    pages/index.astro       # Task 5 — custom landing page
    components/
      Hero.astro            # Task 6
      ValueProps.astro      # Task 6
      Showcase.astro        # Task 6 (reused for cockpit + web, mirrored)
      FeatureGrid.astro     # Task 6
      PipelineDiagram.astro # Task 6
      Quickstart.astro      # Task 6
      SiteFooter.astro      # Task 6
    content/
      docs/                 # Task 3 — Starlight markdown tree
    assets/                 # imported logos used by landing
  public/
    media/                  # Task 7 — committed GIFs/PNGs/SVGs
  tape/                     # Task 7 — VHS .tape sources
  scripts/
    shoot-web.mjs           # Task 7 — Playwright screenshot script
.github/workflows/site.yml  # Task 4
```

---

### Task 1: Scaffold the Astro + Starlight project

**Files:**
- Create: `site/package.json`
- Create: `site/astro.config.mjs`
- Create: `site/tsconfig.json`
- Create: `site/.gitignore`
- Create: `site/src/content/docs/index.md` (temporary placeholder so the build has one page)

- [ ] **Step 1: Create `site/package.json`**

```json
{
  "name": "warden-site",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "astro dev",
    "build": "astro build",
    "preview": "astro preview"
  },
  "dependencies": {
    "astro": "^5.4.0",
    "@astrojs/starlight": "^0.34.0"
  }
}
```

- [ ] **Step 2: Create `site/astro.config.mjs`**

```js
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

// GitHub Pages project site lives at https://srjn45.github.io/warden
export default defineConfig({
  site: 'https://srjn45.github.io',
  base: '/warden',
  integrations: [
    starlight({
      title: 'warden',
      description: 'Run a fleet of Claude Code agents from one cockpit.',
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/srjn45/warden' },
      ],
      // sidebar is populated in Task 3
      sidebar: [],
    }),
  ],
});
```

- [ ] **Step 3: Create `site/tsconfig.json`**

```json
{
  "extends": "astro/tsconfigs/strict",
  "include": [".astro/types.d.ts", "**/*"],
  "exclude": ["dist"]
}
```

- [ ] **Step 4: Create `site/.gitignore`**

```
node_modules/
dist/
.astro/
```

- [ ] **Step 5: Create a placeholder doc `site/src/content/docs/index.md`** (replaced in Task 3)

```md
---
title: warden docs
description: Placeholder home page, replaced in Task 3.
---

Placeholder.
```

- [ ] **Step 6: Install dependencies**

Run: `cd site && npm install`
Expected: installs Astro + Starlight, creates `package-lock.json`, exit 0.

- [ ] **Step 7: Build to verify the scaffold works**

Run: `cd site && npm run build`
Expected: PASS — "Complete!" with `dist/` produced, no errors.

- [ ] **Step 8: Commit**

```bash
git add site/package.json site/package-lock.json site/astro.config.mjs site/tsconfig.json site/.gitignore site/src/content/docs/index.md
git commit -m "feat(site): scaffold Astro + Starlight project for the public website"
```

---

### Task 2: Reuse brand assets (logo, favicon, social card)

**Files:**
- Create: `site/src/assets/warden-symbol.svg` (copied from `brand/`)
- Create: `site/public/favicon.svg` (copied from `brand/warden-symbol-mono.svg`)
- Create: `site/public/og-image.png` (copied from `brand/og-image.png`)
- Modify: `site/astro.config.mjs` (wire logo, favicon, head OG tags)

- [ ] **Step 1: Copy brand assets into the site**

```bash
cp brand/warden-symbol.svg site/src/assets/warden-symbol.svg
cp brand/warden-wordmark-dark.svg site/src/assets/warden-wordmark-dark.svg
cp brand/warden-wordmark-light.svg site/src/assets/warden-wordmark-light.svg
cp brand/warden-symbol-mono.svg site/public/favicon.svg
cp brand/og-image.png site/public/og-image.png
cp brand/apple-touch-icon-180.png site/public/apple-touch-icon.png
```

- [ ] **Step 2: Wire logo, favicon, and OG tags in `site/astro.config.mjs`**

Replace the `starlight({ ... })` options with:

```js
    starlight({
      title: 'warden',
      description: 'Run a fleet of Claude Code agents from one cockpit.',
      logo: {
        light: './src/assets/warden-wordmark-light.svg',
        dark: './src/assets/warden-wordmark-dark.svg',
        replacesTitle: true,
      },
      favicon: '/favicon.svg',
      head: [
        { tag: 'meta', attrs: { property: 'og:image', content: 'https://srjn45.github.io/warden/og-image.png' } },
        { tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
        { tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/warden/apple-touch-icon.png' } },
      ],
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/srjn45/warden' },
      ],
      sidebar: [],
    }),
```

- [ ] **Step 3: Build to verify assets resolve**

Run: `cd site && npm run build`
Expected: PASS. Confirm `dist/favicon.svg` and `dist/og-image.png` exist:
Run: `ls site/dist/favicon.svg site/dist/og-image.png`
Expected: both files listed.

- [ ] **Step 4: Commit**

```bash
git add site/src/assets site/public site/astro.config.mjs
git commit -m "feat(site): reuse warden brand assets for logo, favicon, OG card"
```

---

### Task 3: Migrate documentation content into Starlight

This is the bulk of the value. The page **bodies are lifted from existing markdown** — do not rewrite prose. For each page below, copy the named source section, then add the Starlight frontmatter shown. Mermaid/screenshots are added in Task 7 (leave a `<!-- media: ... -->` comment placeholder where the spec calls for a visual).

**Source files:** `README.md`, `docs/FEATURES.md`, `docs/USAGE.md` (all in repo root / `docs/`).

**Files (create all under `site/src/content/docs/`):**
- Delete: `site/src/content/docs/index.md` (placeholder from Task 1) → replace with the splash home below
- Create: `index.mdx` (docs landing / splash)
- Create: `start/what-is-warden.md`
- Create: `start/install.md`
- Create: `start/quickstart.md`
- Create: `concepts/architecture.md`
- Create: `concepts/agents-lifecycle.md`
- Create: `concepts/worktrees-task-types.md`
- Create: `guides/spawn-and-watch.md`
- Create: `guides/tui-cockpit.md`
- Create: `guides/web-mission-control.md`
- Create: `guides/approvals-supervised.md`
- Create: `guides/rotation-digests.md`
- Create: `multi-agent/pipelines.md`
- Create: `multi-agent/shared-context-messages.md`
- Create: `multi-agent/mcp-and-skill.md`
- Create: `reference/cli.md`
- Create: `reference/env-vars.md`
- Create: `reference/observability.md`
- Create: `reference/troubleshooting.md`
- Modify: `site/astro.config.mjs` (populate `sidebar`)

**Content-source mapping (which existing text fills each page body):**

| Page | Body source |
|---|---|
| `start/what-is-warden.md` | NEW: 2–3 paras from `README.md` intro + FEATURES §1 overview para |
| `start/install.md` | `README.md` "Prerequisites" + "Install" + "Install the daemon" + "Wire in the Claude Code hooks" |
| `start/quickstart.md` | NEW: distilled from `README.md` "Typical workflow" (spawn → watch → attach → clean up) |
| `concepts/architecture.md` | `FEATURES.md` §1 (Core architecture) |
| `concepts/agents-lifecycle.md` | `FEATURES.md` §2 (Spawning) + §3 (Lifecycle) |
| `concepts/worktrees-task-types.md` | `FEATURES.md` §2 "Task types" + `README.md` "Task types and the `--type` flag" |
| `guides/spawn-and-watch.md` | `docs/USAGE.md` spawn/observe sections + `FEATURES.md` §4 |
| `guides/tui-cockpit.md` | `FEATURES.md` §8 + `README.md` "Terminal UI" |
| `guides/web-mission-control.md` | `FEATURES.md` §9 + `README.md` "Web GUI" |
| `guides/approvals-supervised.md` | `FEATURES.md` §5 + supervised-mode rows from §2 |
| `guides/rotation-digests.md` | `FEATURES.md` §7 (self-rotation) + completion-digest rows |
| `multi-agent/pipelines.md` | `FEATURES.md` §6 (pipelines) + `README.md` pipeline notes |
| `multi-agent/shared-context-messages.md` | `FEATURES.md` §6 (shared context + directed messages) |
| `multi-agent/mcp-and-skill.md` | `FEATURES.md` §10 + `README.md` "Orchestrator (MCP)" |
| `reference/cli.md` | Hand-curated from `warden --help` / per-command `--help` (see Step for capture command) |
| `reference/env-vars.md` | `FEATURES.md` §12 (Configuration) |
| `reference/observability.md` | `FEATURES.md` §11 |
| `reference/troubleshooting.md` | NEW: `warden doctor` output + common fixes from README |

- [ ] **Step 1: Replace the placeholder home with a splash page `site/src/content/docs/index.mdx`**

```mdx
---
title: warden
description: Run a fleet of Claude Code agents from one cockpit.
template: splash
hero:
  tagline: Spawn, watch, approve, and orchestrate Claude Code sessions — from one cockpit.
  actions:
    - text: Get started
      link: /warden/start/what-is-warden/
      icon: right-arrow
      variant: primary
    - text: View on GitHub
      link: https://github.com/srjn45/warden
      icon: external
---

import { Card, CardGrid } from '@astrojs/starlight/components';

<CardGrid>
  <Card title="One cockpit" icon="laptop">TUI + web. See every agent's status at a glance.</Card>
  <Card title="Pipelines" icon="random">Chain dependent agents into a DAG.</Card>
  <Card title="One binary" icon="seti:binary">Daemon, CLI, TUI, web, MCP. No database.</Card>
</CardGrid>
```

Then delete the old placeholder:
```bash
rm site/src/content/docs/index.md
```

- [ ] **Step 2: Create one fully-worked example page so the pattern is unambiguous — `site/src/content/docs/concepts/architecture.md`**

Frontmatter pattern (apply the same shape to every page, changing title/description):

```md
---
title: Architecture & the daemon
description: How warden is built — one binary, a local daemon, a file-based store, and Claude Code hooks.
---

<!-- BODY: lift FEATURES.md §1 "Core architecture" verbatim (the intro paragraph + the capability table). -->
<!-- media: architecture diagram (public/media/architecture.svg) embedded here in Task 7 -->
```

Copy the actual §1 text from `docs/FEATURES.md` into the body (intro paragraph + the table). Keep the markdown table as-is — Starlight renders it.

- [ ] **Step 3: Create the remaining pages using the mapping table above**

For each remaining file in the Files list, create it with the frontmatter pattern from Step 2 (a `title` and one-line `description`) and paste the body from the mapped source section. Where the spec calls for a visual, insert a `<!-- media: ... -->` comment placeholder (filled in Task 7). Do not invent new feature prose — only condense/reorganize existing text.

For `reference/cli.md`, first capture the command surface:
```bash
go run ./cmd/warden --help > /tmp/warden-help.txt
for c in start ls attach talk stop digest rotate approvals approve pipeline stats doctor; do echo "## $c"; go run ./cmd/warden "$c" --help; echo; done >> /tmp/warden-help.txt
```
Then format `/tmp/warden-help.txt` into the page as one section per command with a fenced `text` block of its usage. Add a note at the top: "Generated from `warden --help` on <date>; regenerate when commands change."

- [ ] **Step 4: Populate the sidebar in `site/astro.config.mjs`**

Replace `sidebar: []` with:

```js
      sidebar: [
        { label: 'Start here', items: [
          { label: 'What is warden?', slug: 'start/what-is-warden' },
          { label: 'Install & setup', slug: 'start/install' },
          { label: 'Quickstart', slug: 'start/quickstart' },
        ]},
        { label: 'Concepts', items: [
          { label: 'Architecture & the daemon', slug: 'concepts/architecture' },
          { label: 'Agents & lifecycle', slug: 'concepts/agents-lifecycle' },
          { label: 'Worktrees & task types', slug: 'concepts/worktrees-task-types' },
        ]},
        { label: 'Guides', items: [
          { label: 'Spawn & watch agents', slug: 'guides/spawn-and-watch' },
          { label: 'The TUI cockpit', slug: 'guides/tui-cockpit' },
          { label: 'Web mission control', slug: 'guides/web-mission-control' },
          { label: 'Approvals & supervised mode', slug: 'guides/approvals-supervised' },
          { label: 'Self-rotation & digests', slug: 'guides/rotation-digests' },
        ]},
        { label: 'Multi-agent', items: [
          { label: 'Pipelines (DAG)', slug: 'multi-agent/pipelines' },
          { label: 'Shared context & messages', slug: 'multi-agent/shared-context-messages' },
          { label: 'Orchestration: MCP & skill', slug: 'multi-agent/mcp-and-skill' },
        ]},
        { label: 'Reference', items: [
          { label: 'CLI command reference', slug: 'reference/cli' },
          { label: 'Environment variables', slug: 'reference/env-vars' },
          { label: 'Observability & metrics', slug: 'reference/observability' },
          { label: 'Troubleshooting', slug: 'reference/troubleshooting' },
        ]},
      ],
```

- [ ] **Step 5: Build and verify no broken links / missing slugs**

Run: `cd site && npm run build`
Expected: PASS. Starlight errors on any `slug:` that has no matching file — a clean build means every sidebar entry resolves.

- [ ] **Step 6: Visual smoke (Playwright)**

Run: `cd site && npm run preview &` then open the preview URL (printed, e.g. `http://localhost:4321/warden/`) with Playwright; navigate to `/warden/concepts/architecture/` and confirm the page + sidebar + search box render. Kill the preview server after.
Expected: docs render with sidebar groups and content.

- [ ] **Step 7: Commit**

```bash
git add site/src/content/docs site/astro.config.mjs
git commit -m "feat(site): migrate README/FEATURES/USAGE into Starlight docs tree"
```

---

### Task 4: GitHub Actions deploy to Pages (validate the base path early)

**Files:**
- Create: `.github/workflows/site.yml`

- [ ] **Step 1: Create `.github/workflows/site.yml`**

```yaml
name: Deploy site

on:
  push:
    branches: [main]
    paths: ['site/**', '.github/workflows/site.yml']
  workflow_dispatch:

permissions:
  contents: read
  pages: write
  id-token: write

concurrency:
  group: pages
  cancel-in-progress: true

jobs:
  build:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: site
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: "22"
      - run: npm ci
      - run: npm run build
      - uses: actions/upload-pages-artifact@v3
        with:
          path: site/dist

  deploy:
    needs: build
    runs-on: ubuntu-latest
    environment:
      name: github-pages
      url: ${{ steps.deployment.outputs.page_url }}
    steps:
      - id: deployment
        uses: actions/deploy-pages@v4
```

- [ ] **Step 2: Enable Pages "Build and deployment → Source: GitHub Actions"**

This is a one-time manual setting in the GitHub repo (Settings → Pages). Document it; the workflow cannot set it. (If the executor lacks repo-admin access, flag this as a hand-off item.)

- [ ] **Step 3: Commit and push to trigger the workflow**

```bash
git add .github/workflows/site.yml
git commit -m "ci(site): build and deploy the website to GitHub Pages"
git push
```

- [ ] **Step 4: Verify the live deploy and base-path resolution**

After the workflow succeeds, open `https://srjn45.github.io/warden/` with Playwright. Confirm:
- the page loads (not 404),
- CSS/logo/favicon load (no broken assets — the classic base-path failure),
- sidebar nav links work and resolve under `/warden/...`,
- search opens.

Expected: all pass. If assets 404, the `base`/`site` config in `astro.config.mjs` is wrong — fix and re-push before continuing.

---

### Task 5: Landing page shell (`index.astro`)

The landing page is a **custom Astro page**, not a Starlight page. It uses its own minimal layout so it can be fully designed. Sections are stubbed here and filled in Task 6.

**Files:**
- Create: `site/src/pages/index.astro`
- Create: `site/src/styles/landing.css`

- [ ] **Step 1: Create `site/src/styles/landing.css`** (dark, terminal-leaning aesthetic)

```css
:root { --bg:#0d1117; --panel:#161b22; --border:#30363d; --fg:#c9d1d9; --muted:#8b949e; --accent:#3fb950; --link:#58a6ff; }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--fg); font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif; }
.wrap { max-width: 960px; margin: 0 auto; padding: 0 20px; }
.section { padding: 56px 0; border-bottom: 1px solid var(--border); }
a { color: var(--link); }
code, .mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
```

- [ ] **Step 2: Create `site/src/pages/index.astro` shell** (imports section components built in Task 6)

```astro
---
import '../styles/landing.css';
import Hero from '../components/Hero.astro';
import ValueProps from '../components/ValueProps.astro';
import Showcase from '../components/Showcase.astro';
import FeatureGrid from '../components/FeatureGrid.astro';
import PipelineDiagram from '../components/PipelineDiagram.astro';
import Quickstart from '../components/Quickstart.astro';
import SiteFooter from '../components/SiteFooter.astro';
const base = import.meta.env.BASE_URL; // '/warden/'
---
<html lang="en">
  <head>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <title>warden — run a fleet of Claude Code agents</title>
    <meta name="description" content="Spawn, watch, approve, and orchestrate Claude Code sessions from one cockpit." />
    <link rel="icon" href={`${base}favicon.svg`} />
    <meta property="og:image" content="https://srjn45.github.io/warden/og-image.png" />
  </head>
  <body>
    <Hero />
    <ValueProps />
    <Showcase side="left"  title="The cockpit" img="cockpit.png"
      blurb="Framed panes, live agent detail, scrollable sessions, approvals inbox — all in the terminal." />
    <Showcase side="right" title="Web mission control" img="web-overview.png"
      blurb="Tabbed shell, live xterm terminals, attention queue, resource metrics — in the browser." />
    <FeatureGrid />
    <PipelineDiagram />
    <Quickstart />
    <SiteFooter />
  </body>
</html>
```

- [ ] **Step 3: Create minimal stubs for the seven components so the build passes**

For each of `Hero.astro`, `ValueProps.astro`, `Showcase.astro`, `FeatureGrid.astro`, `PipelineDiagram.astro`, `Quickstart.astro`, `SiteFooter.astro` in `site/src/components/`, create a stub:

```astro
---
// Showcase.astro takes props; the others take none. Showcase stub:
const { side = 'left', title = '', img = '', blurb = '' } = Astro.props;
---
<section class="section"><div class="wrap"><h2>{title || 'stub'}</h2></div></section>
```

(The non-prop components can be the same minus the frontmatter props line.)

- [ ] **Step 4: Build to verify the shell compiles**

Run: `cd site && npm run build`
Expected: PASS. `dist/index.html` exists at the site root.

- [ ] **Step 5: Commit**

```bash
git add site/src/pages/index.astro site/src/styles/landing.css site/src/components
git commit -m "feat(site): landing page shell + section component stubs"
```

---

### Task 6: Build out the landing page sections

Fill each stub with real markup. All media is referenced from `${base}media/...` and may 404 until Task 7 — that is expected; the build still passes.

**Files (modify all in `site/src/components/`):** `Hero.astro`, `ValueProps.astro`, `Showcase.astro`, `FeatureGrid.astro`, `PipelineDiagram.astro`, `Quickstart.astro`, `SiteFooter.astro`

- [ ] **Step 1: `Hero.astro`**

```astro
---
const base = import.meta.env.BASE_URL;
---
<section class="section" style="text-align:center">
  <div class="wrap">
    <h1 style="font-size:2.6rem;margin:0">Run a fleet of Claude agents</h1>
    <p style="color:var(--muted);font-size:1.15rem">Spawn, watch, approve, and orchestrate Claude Code sessions — from one cockpit.</p>
    <p>
      <a href={`${base}start/what-is-warden/`} style="background:var(--accent);color:#06210f;padding:10px 18px;border-radius:6px;text-decoration:none;font-weight:600">Get started</a>
      &nbsp;
      <a href="https://github.com/srjn45/warden" style="background:var(--panel);padding:10px 18px;border-radius:6px;text-decoration:none">★ Star on GitHub</a>
    </p>
    <img src={`${base}media/hero.gif`} alt="warden start and warden ls in a terminal"
      style="max-width:640px;width:100%;border:1px solid var(--border);border-radius:8px;margin-top:18px" loading="eager" />
  </div>
</section>
```

- [ ] **Step 2: `ValueProps.astro`** — three cards (one cockpit / pipelines / one binary)

```astro
<section class="section">
  <div class="wrap" style="display:grid;grid-template-columns:repeat(3,1fr);gap:14px">
    <div style="background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:16px">
      <h3>🧭 One cockpit</h3><p style="color:var(--muted)">TUI + web. See every agent's status at a glance.</p></div>
    <div style="background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:16px">
      <h3>🔗 Pipelines</h3><p style="color:var(--muted)">Chain dependent agents into a DAG.</p></div>
    <div style="background:var(--panel);border:1px solid var(--border);border-radius:8px;padding:16px">
      <h3>📦 One binary</h3><p style="color:var(--muted)">Daemon, CLI, TUI, web, MCP. No database.</p></div>
  </div>
</section>
```

- [ ] **Step 3: `Showcase.astro`** — reusable, mirrors layout via `side` prop

```astro
---
const { side = 'left', title = '', img = '', blurb = '' } = Astro.props;
const base = import.meta.env.BASE_URL;
const rowDir = side === 'right' ? 'row-reverse' : 'row';
---
<section class="section">
  <div class="wrap" style={`display:flex;flex-direction:${rowDir};gap:24px;align-items:center;flex-wrap:wrap`}>
    <img src={`${base}media/${img}`} alt={title} style="flex:1 1 360px;min-width:300px;border:1px solid var(--border);border-radius:8px" />
    <div style="flex:1 1 240px"><h2>{title}</h2><p style="color:var(--muted);font-size:1.1rem">{blurb}</p></div>
  </div>
</section>
```

- [ ] **Step 4: `FeatureGrid.astro`** — tiles linking into docs

```astro
---
const base = import.meta.env.BASE_URL;
const tiles = [
  ['Spawn & classify', 'guides/spawn-and-watch'],
  ['Approvals inbox', 'guides/approvals-supervised'],
  ['Supervised mode', 'guides/approvals-supervised'],
  ['Self-rotation', 'guides/rotation-digests'],
  ['Worktrees', 'concepts/worktrees-task-types'],
  ['Completion digest', 'guides/rotation-digests'],
  ['Observability', 'reference/observability'],
  ['Context guard', 'reference/observability'],
];
---
<section class="section">
  <div class="wrap">
    <h2 style="text-align:center">Everything warden does</h2>
    <div style="display:grid;grid-template-columns:repeat(4,1fr);gap:10px">
      {tiles.map(([label, slug]) => (
        <a href={`${base}${slug}/`} style="background:var(--panel);border:1px solid var(--border);border-radius:6px;padding:14px;text-decoration:none;color:var(--fg)">{label}</a>
      ))}
    </div>
  </div>
</section>
```

- [ ] **Step 5: `PipelineDiagram.astro`** — references the SVG built in Task 7

```astro
---
const base = import.meta.env.BASE_URL;
---
<section class="section" style="text-align:center">
  <div class="wrap">
    <h2>Pipelines</h2>
    <p style="color:var(--muted)">Chain dependent agents into a DAG — analyze → implement → review.</p>
    <img src={`${base}media/pipeline-dag.svg`} alt="A three-stage agent pipeline" style="max-width:640px;width:100%" />
  </div>
</section>
```

- [ ] **Step 6: `Quickstart.astro`** — copy-paste install

```astro
<section class="section">
  <div class="wrap">
    <h2 style="text-align:center">Quickstart</h2>
    <pre class="mono" style="background:#010409;border:1px solid var(--border);border-radius:8px;padding:16px;overflow:auto"><code># download a release binary (see Install for brew / source)
warden doctor          # preflight checks
warden start "fix the flaky nightly test"</code></pre>
  </div>
</section>
```

- [ ] **Step 7: `SiteFooter.astro`**

```astro
---
const base = import.meta.env.BASE_URL;
---
<footer style="padding:28px 0;text-align:center;color:var(--muted)">
  <div class="wrap">
    <a href={`${base}start/what-is-warden/`}>Docs</a> ·
    <a href="https://github.com/srjn45/warden">GitHub</a> ·
    Apache-2.0 · built with Claude Code
  </div>
</footer>
```

- [ ] **Step 8: Build and visual-smoke the landing page**

Run: `cd site && npm run build && npm run preview &`
Open `http://localhost:4321/warden/` with Playwright; confirm all 8 sections render and the docs links navigate. Broken `media/` images are expected until Task 7.
Expected: build PASS, all sections present, internal links work.

- [ ] **Step 9: Commit**

```bash
git add site/src/components
git commit -m "feat(site): build out landing page sections (hero, value props, showcases, grid, pipeline, quickstart, footer)"
```

---

### Task 7: Produce and wire in the visual media

**Files:**
- Create: `site/tape/hero.tape`
- Create: `site/scripts/shoot-web.mjs`
- Create: `site/public/media/hero.gif` (generated)
- Create: `site/public/media/cockpit.png` (captured)
- Create: `site/public/media/web-overview.png` (captured)
- Create: `site/public/media/architecture.svg` (authored)
- Create: `site/public/media/pipeline-dag.svg` (authored)
- Modify: `site/src/content/docs/concepts/architecture.md` (embed the diagram where the `<!-- media -->` placeholder is)

- [ ] **Step 1: Install VHS**

Run: `brew install vhs`
Expected: `vhs` on PATH (`which vhs`). (VHS needs `ttyd` + `ffmpeg`; Homebrew pulls them as deps.)

- [ ] **Step 2: Write `site/tape/hero.tape`**

```tape
Output ../public/media/hero.gif
Set FontSize 18
Set Width 1200
Set Height 600
Set Theme "Dracula"
Type "warden start 'fix the flaky nightly test'"  Sleep 500ms  Enter
Sleep 2s
Type "warden ls"  Sleep 500ms  Enter
Sleep 3s
```

- [ ] **Step 3: Render the hero GIF**

Run: `cd site/tape && vhs hero.tape`
Expected: `site/public/media/hero.gif` created. Open it to confirm it shows the two commands.

- [ ] **Step 4: Capture the TUI cockpit screenshot**

With the daemon running and at least one agent spawned, open the cockpit (`warden tui` / `wd`) in a terminal sized ~1200×750, take a screenshot, save as `site/public/media/cockpit.png`. (Manual capture — there is no headless path for the full-screen TUI. Crop to the terminal window.)

- [ ] **Step 5: Write `site/scripts/shoot-web.mjs`** (Playwright screenshot of the web GUI)

```js
// Usage: node site/scripts/shoot-web.mjs
// Requires the warden web GUI running locally (see README "Web GUI"), default :4321 dev or the daemon's embedded build.
import { chromium } from 'playwright';
const URL = process.env.WARDEN_WEB_URL || 'http://localhost:4321/';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
await page.goto(URL, { waitUntil: 'networkidle' });
await page.screenshot({ path: 'site/public/media/web-overview.png' });
await browser.close();
console.log('wrote site/public/media/web-overview.png');
```

- [ ] **Step 6: Capture the web GUI screenshot**

Start the web GUI locally (per `README.md` "Web GUI": daemon on :8765 + `npm run dev` in `web/` on :4321), then:
Run: `npx playwright install chromium && node site/scripts/shoot-web.mjs`
Expected: `site/public/media/web-overview.png` exists. (The Playwright MCP browser may be used instead of a local `playwright` dep if preferred.)

- [ ] **Step 7: Author the two diagrams**

Create `site/public/media/architecture.svg` and `site/public/media/pipeline-dag.svg`. Author each as an SVG (or render from Mermaid and export to SVG). Minimum content:
- `architecture.svg`: boxes for **CLI / TUI / Web / MCP** → **daemon** → **file store (~/.warden)**, and **daemon** → **tmux** → **claude** (one per agent), with **hooks** arrow back to daemon.
- `pipeline-dag.svg`: three linked nodes `analyze → implement → review`.

Keep them legible on dark and light backgrounds (use `currentColor`-safe strokes or a neutral palette).

- [ ] **Step 8: Embed the architecture diagram in the docs**

In `site/src/content/docs/concepts/architecture.md`, replace the `<!-- media: architecture diagram ... -->` placeholder with:

```md
![warden architecture](../../../../public/media/architecture.svg)
```

(Use a public-path image reference that resolves under the base: prefer `![warden architecture](/warden/media/architecture.svg)` if the relative import is awkward in MDX — verify which resolves in the build.)

- [ ] **Step 9: Build and full visual smoke**

Run: `cd site && npm run build && npm run preview &`
Open `http://localhost:4321/warden/` with Playwright; confirm: hero GIF animates, both showcase screenshots load, the pipeline SVG and feature grid render, and the architecture diagram appears on the architecture docs page.
Expected: no broken images anywhere.

- [ ] **Step 10: Commit**

```bash
git add site/tape site/scripts site/public/media site/src/content/docs/concepts/architecture.md
git commit -m "feat(site): add VHS hero demo, TUI/web screenshots, architecture + pipeline diagrams"
```

---

### Task 8: Final verification + cross-links from the repo

**Files:**
- Modify: `README.md` (add a link to the live site near the top)

- [ ] **Step 1: Add a site link to `README.md`**

Near the top of `README.md`, add:

```md
> 📖 **Docs & guide:** https://srjn45.github.io/warden/
```

- [ ] **Step 2: Push and verify the live site end-to-end**

```bash
git add README.md
git commit -m "docs: link the public website from the README"
git push
```

After the `site.yml` workflow finishes, open `https://srjn45.github.io/warden/` with Playwright and verify the full acceptance checklist (spec §8):
- landing page renders with all media (hero GIF, screenshots, diagrams) — no 404s under the `/warden/` base path,
- docs search works,
- light/dark toggle works,
- a sample of sidebar links across all five groups resolve,
- "Edit this page on GitHub" links point at the repo.

Expected: all pass.

- [ ] **Step 3: Final commit (if any link/asset fixes were needed)**

```bash
git add -A
git commit -m "fix(site): resolve final base-path / asset issues from live verification"
git push
```

---

## Self-review notes (completed by plan author)

- **Spec coverage:** every spec section maps to a task — §3 architecture→Task 1/2/5, §4 landing→Task 5/6, §5 docs IA→Task 3, §6 media→Task 7, §7 build/deploy→Task 4, §8 testing→Tasks 3/4/6/7/8 smokes. ✅
- **§10 open item resolved:** canonical content **moves into** `site/src/content/docs/` (Task 3); `README` stays the repo landing doc and links out (Task 8). CLI reference is **hand-curated** v1 (Task 3 Step 3), with a "regenerate" note leaving the cobra-autogen seam for later.
- **Placeholder scan:** the only `<!-- media -->` markers are intentional, time-ordered placeholders filled in Task 7 — not plan placeholders. No TBD/TODO left.
- **Type/name consistency:** component names, props (`Showcase` `side`/`title`/`img`/`blurb`), media filenames (`hero.gif`, `cockpit.png`, `web-overview.png`, `architecture.svg`, `pipeline-dag.svg`), and sidebar `slug`s match across Tasks 3, 5, 6, 7. ✅
- **Known hand-off items (need repo-admin / a live environment, not codeable):** enabling Pages "Source: GitHub Actions" (Task 4 Step 2); capturing the TUI/web screenshots requires a running daemon + agents (Task 7 Steps 4, 6).
