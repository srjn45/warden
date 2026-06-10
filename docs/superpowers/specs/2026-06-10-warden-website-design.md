# warden public website — design

**Date:** 2026-06-10
**Status:** Approved (brainstorm), pending implementation plan
**Goal:** Replace "lots of markdown nobody reads" with a public, visual, marketing
landing page + searchable documentation site for [warden](https://github.com/srjn45/warden).

---

## 1. Summary

Build a combined **marketing landing page + documentation site** for warden,
hosted on **GitHub Pages** at `srjn45.github.io/warden`. The landing page sells
warden to GitHub visitors with a "show, don't tell" visual narrative (terminal
recordings, screenshots, diagrams). The docs site teaches setup and usage,
reorganizing existing markdown (`README.md`, `docs/FEATURES.md`, `docs/USAGE.md`)
into a searchable, navigable Starlight site.

The core problem being solved: the project's content is comprehensive but
text-only, and text-only docs do not get read. The fix is **visuals first** —
every landing section leads with a visual; docs embed diagrams and recordings.

---

## 2. Decisions (locked during brainstorm)

| Decision | Choice | Rationale |
|---|---|---|
| Format | Landing page **+** docs (combo) | Standard OSS shape; converts visitors *and* teaches. |
| Audience | Public (GitHub visitors / potential adopters) | warden is already public; this is its front door. |
| Framework | **Astro + Starlight** | Astro already used in `web/`; Starlight ingests existing markdown nearly verbatim and provides search/dark-mode/nav for free. |
| Location | Same repo, `site/` folder | Docs version alongside code; one PR updates both. |
| Hosting | **GitHub Pages**, `srjn45.github.io/warden` | Free, zero new infra. Custom domain attachable later without rebuild. |
| Build/deploy | GitHub Actions on push to `main` | Builds the Astro site, deploys to Pages. |
| Terminal demos | **VHS** (`charmbracelet/vhs`) | Scripted `.tape` files → GIF/WebM. Reproducible, committed, re-renderable as warden evolves. |
| TUI/web screenshots | Static PNGs; web GUI automated via **Playwright** | Repeatable captures of the cockpit and mission-control. |
| Diagrams | Hand-authored SVG / Excalidraw / Mermaid | Architecture + pipeline DAG can't be screenshotted. |
| asciinema | **Excluded** (YAGNI) | VHS covers terminal demos; avoid a second player dependency. |

---

## 3. Architecture

Single Astro project under `site/`:

```
site/
  astro.config.mjs        # base: '/warden', site: 'https://srjn45.github.io', Starlight integration
  package.json
  src/
    pages/
      index.astro          # the marketing landing page (custom, 8 sections)
    content/
      docs/                # Starlight markdown — the docs tree (see §5)
    assets/                # imported diagrams/screenshots used by pages
    components/            # landing-page section components (Hero, FeatureGrid, …)
  public/
    media/                 # rendered VHS GIFs/WebM, screenshots, brand assets
  tape/                    # VHS .tape source scripts (committed, regenerate media/)
```

- **Landing page** (`src/pages/index.astro`) is a custom Astro page composed of
  section components. It is *not* a Starlight page — it owns its own layout.
- **Docs** live in `src/content/docs/` and are rendered by Starlight with its
  sidebar, search, and theming.
- **Brand assets** (`brand/*.svg`, `og-image.png`, favicons) are reused as-is for
  the site logo, favicon, and social card.
- **Media pipeline:** `tape/*.tape` → `vhs` → `public/media/*.gif`. Screenshots
  land in `public/media/` too. Regenerating media is a documented manual step
  (not part of the CI build — CI consumes committed media).

### Isolation / boundaries

- **Landing components** are self-contained, presentational, and depend only on
  media files in `public/media/`. Each (Hero, ValueProps, CockpitShowcase,
  WebShowcase, FeatureGrid, PipelineDiagram, Quickstart, Footer) can be edited
  without touching the others.
- **Docs content** depends only on Starlight + embedded media. No coupling to
  landing components.
- **Media generation** (VHS tapes, screenshot scripts) is a separate concern from
  the site build; the site only consumes the committed output.

---

## 4. Landing page (`index.astro`) — 8 sections

1. **Hero** — wordmark, tagline ("Run a fleet of Claude agents"), one-line value
   prop, two CTAs (Get started → docs; Star on GitHub), and an **autoplaying
   looping VHS recording** of `warden start … → warden ls`.
2. **Why warden** — 3 value cards: one cockpit (TUI + web), pipelines (DAG), one
   binary (daemon/CLI/TUI/web/MCP, no DB).
3. **The cockpit** — large TUI screenshot/recording with text alongside (panes,
   live agent detail, scrollable sessions, approvals).
4. **Web mission control** — web GUI screenshot, mirrored layout (tabbed shell,
   live xterm terminals, attention queue, resource metrics).
5. **Feature grid** — icon tiles (spawn & classify, approvals inbox, supervised
   mode, self-rotation, worktrees, completion digest, observability, context
   guard); each tile links into the relevant docs page.
6. **Pipelines** — animated DAG diagram (analyze → implement → review).
7. **Quickstart** — copy-paste 3-step install (download/brew → `warden doctor` →
   `warden start "…"`).
8. **Footer** — Docs · GitHub · Apache-2.0 · "built with Claude Code".

---

## 5. Documentation IA (Starlight sidebar)

Content-source tags: 🟢 lifts from existing markdown · 🔵 new short page · 🟡 generated.

- **Start here**
  - What is warden? 🔵 (overview + architecture diagram)
  - Install & setup 🟢 (`README.md`)
  - Quickstart: your first agent 🔵
- **Concepts**
  - Architecture & the daemon 🟢 (`FEATURES` §1)
  - Agents & lifecycle 🟢 (`FEATURES` §2–3)
  - Worktrees & task types 🟢 (`FEATURES` §2)
- **Guides (how-to)**
  - Spawn & watch agents 🟢 (`USAGE`)
  - The TUI cockpit 🟢 (`FEATURES` §8)
  - Web mission control 🟢 (`FEATURES` §9)
  - Approvals & supervised mode 🟢 (`FEATURES` §5)
  - Self-rotation & digests 🟢 (`FEATURES` §7)
- **Multi-agent**
  - Pipelines (DAG) 🟢 (`FEATURES` §6)
  - Shared context & messages 🟢 (`FEATURES` §6)
  - Orchestration via MCP / the `/warden` skill 🟢 (`FEATURES` §10)
- **Reference**
  - CLI command reference 🟡 (generated from command definitions where feasible; otherwise hand-curated)
  - Environment variables 🟢 (`FEATURES` §12)
  - Observability & metrics 🟢 (`FEATURES` §11)
  - Troubleshooting / `warden doctor` 🔵

~80% of docs is reorganizing existing markdown; the rest is short new pages.

---

## 6. Visual asset production

| Asset | Tool | Source artifact | Output |
|---|---|---|---|
| Hero terminal demo | VHS | `tape/hero.tape` | `public/media/hero.gif` (+ webm) |
| CLI flow demos (spawn/ls/attach/pipeline) | VHS | `tape/*.tape` | `public/media/*.gif` |
| TUI cockpit | screenshot (tmux) | manual capture | `public/media/cockpit.png` |
| Web mission control | Playwright | screenshot script against local web GUI | `public/media/web-*.png` |
| Architecture diagram | hand-authored | SVG/Excalidraw/Mermaid | `public/media/architecture.svg` |
| Pipeline DAG | hand-authored | SVG/Mermaid | `public/media/pipeline-dag.svg` |

VHS `.tape` scripts and screenshot scripts are committed so any maintainer can
regenerate media as warden changes. CI does **not** render media; it consumes the
committed files (keeps the build fast and deterministic, avoids needing tmux/
claude in CI).

---

## 7. Build & deploy

- `astro.config.mjs`: `site: 'https://srjn45.github.io'`, `base: '/warden'`
  (project-pages subpath). All internal links/asset refs respect `base`.
- GitHub Actions workflow (`.github/workflows/site.yml`): on push to `main`
  touching `site/**`, run `npm ci && npm run build` in `site/`, upload the
  `dist/` artifact, deploy to GitHub Pages via the official Pages actions.
- Pages configured to deploy from Actions (not a branch).
- Social/OG: reuse `brand/og-image.png`; set canonical + OG meta in the layout.

---

## 8. Testing & verification

- **Build check:** `astro build` succeeds with zero broken internal links
  (Starlight/Astro link checking) under the `/warden` base path.
- **Visual check:** landing page and a sample of docs pages render correctly in
  light and dark mode (manual + Playwright screenshot smoke).
- **Deploy check:** the Actions workflow publishes and the live URL loads with
  working nav, search, and media (assets resolve under the subpath — the classic
  GitHub-Pages base-path failure mode, so this is explicitly verified).
- **Media check:** every embedded image/GIF referenced by a page exists in
  `public/media/`.

---

## 9. Scope / non-goals (YAGNI)

- **No custom domain** initially (github.io subpath; attachable later).
- **No asciinema** (VHS covers it).
- **No CI media rendering** (committed media only).
- **No blog, no versioned docs, no i18n** — single current-version docs.
- **No analytics/telemetry** in v1 (can add a privacy-friendly tracker later).
- Landing page does **not** duplicate full feature prose — it links into docs.

---

## 10. Open implementation details (resolve in plan)

- Whether the CLI reference can be truly auto-generated from the Go command
  definitions, or is hand-curated from `--help` output for v1.
- Exact VHS theme/dimensions to match warden's terminal aesthetic.
- Whether to keep `docs/FEATURES.md` etc. as the canonical source and have the
  site import them, or move canonical content into `site/src/content/docs/` and
  leave thin pointers in the old locations. (Leaning: move canonical into the
  site, keep README as the repo landing doc.)
