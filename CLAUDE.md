# CLAUDE.md

Guidance for Claude Code (and human contributors) working in this repo.

## Definition of Done — feature delivery

A feature is **not complete** when the code compiles and tests pass. Before a
feature is considered delivered, every item below must be done (or explicitly
ruled out as not applicable):

### 1. Tag & release

- A completed feature must be followed by a **tag and release**.
- Follow the repo's tagging style: **one tag per feature** — `minor` bump for a
  big feature, `patch` for a small one.
- Pushing a `v*` tag triggers the GoReleaser pipeline (see `.goreleaser.yaml`
  and `docs/SHIPPING.md`), so **confirm with the maintainer before pushing the
  tag** — the push is what cuts the public release.

### 2. Documentation — including the website

A feature is not done until **all** affected docs are updated. Check each of:

- **`README.md`** — the top-level feature/usage surface.
- **`docs/`** — `FEATURES.md`, `USAGE.md`, and any relevant spec under
  `docs/specs/`.
- **The website** — `site/` (Astro/Starlight). Update the matching page under
  `site/src/content/docs/` (`start/`, `guides/`, `concepts/`, `reference/`,
  `multi-agent/`). New user-facing capability usually needs both a guide and a
  reference entry (e.g. `reference/cli.md`).
- **The skill** — `skills/warden/` if the feature changes how agents should
  drive warden.

### 3. CLI help / manual

- Check whether the feature changes the **CLI tool's help/manual** and update it
  if so. Command help text lives with the cobra commands in `internal/cli/`
  (`Use`, `Short`, `Long`, flag descriptions).
- The CLI reference (`site/src/content/docs/reference/cli.md`) is **generated**
  from the cobra command tree by `cmd/gendocs` — never hand-edit it. After
  changing any command's `Use`/`Short`/`Long` or flags, run `make gendocs` and
  commit the result. CI (`make gendocs-check`) fails the build if the committed
  page is stale, so this can no longer drift.

---

When finishing a feature, explicitly walk this checklist and report which items
were updated and which were intentionally skipped.
