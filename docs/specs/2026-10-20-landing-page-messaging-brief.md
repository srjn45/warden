# Landing page messaging brief (2026-10-20)

Copy-only brief for the marketing landing page rewrite (run `site-landing-page-rewrite`, task t1). For review; no `site/` files are changed here.

## Message order

1. **Plan it, Autopilot ships it.** (primary)
2. **One cockpit for every agent.** (secondary: the fleet cockpit)

## Hero

- **Headline:** Plan it. Autopilot ships it.
- **Subhead:** Write the goal as a plan. Warden's Autopilot breaks it into tasks, runs worker agents in isolated worktrees, gates each PR through CI, and lands it on an integration branch. Watch and steer the whole fleet from one cockpit, across any AI coding CLI.
- **CTAs:** Get started (install) · View on GitHub (unchanged).

## Value-prop tiles (replace "One binary, no database")

| Tile | Copy | Source |
|---|---|---|
| Autopilot, plan-driven | Describe a goal in a plan; a manager delegates tasks to workers and lands their PRs. Workers never touch `main`. | FEATURES §34 (§34.1 plan file, §34.2 topology, §34.7 land, §34.8 integration branch); `wd plan run <id> --mode autopilot`, `wd autopilot` |
| Any backend, one cockpit | Run Claude, Codex, Antigravity, Cursor, OpenCode, Aider and more side by side in a TUI and web cockpit. | FEATURES §8 (TUI), §9 (Web), §35 (backend registry); `wd backend`, `wd agent` |
| Cost-tier routing and recovery | Autopilot prefers free, then subscription, then opt-in pay-per-use backends, and recovers when a limit is hit. | FEATURES §34.4 (cost-tier), §35.4, §37.6 (plan recovery); `wd backend tier`, `wd usage` (`recover`) |
| Self-healing | A guardian restarts a wedged manager; the web cockpit rebuilds a wedged session. | FEATURES §34.3 (guardian and overwatch), §9 (self-healing cockpit) |
| Remote and mobile | Reach the dashboard from a phone or tablet with token auth and a mobile-responsive UI. | FEATURES §13 (remote access, mobile-responsive dashboard); `wd daemon token` |

Keep the existing Pipelines (`wd pipeline`) and Measured savings (FEATURES §29; `wd usage savings`) tiles only if space allows; both are shipped.

## Claim-to-source rule

Every line of landing copy must map to a FEATURES.md section or a `wd --help` command. Unverifiable claims are dropped.

**Not claimed (no verifiable source; do not use):**
- "No database" / "no database server" (storage is embedded ScrivaDB, §31; the claim is true only loosely, so leave it out).
- A native Android app (the shipped, documented mobile surface is the responsive web dashboard, §13). Existing `AndroidApp.astro` copy must be reviewed against docs before it is kept.
- Hard backend counts or "unattended and safe". Autopilot is documented as inherently risky (§34 warning); say "pause any time with `wd plan pause`".
- Any pricing or savings numbers not produced by `wd usage savings`.

## Install story (verified)

Source: `site/src/content/docs/start/install.md` and `scripts/install.sh` (exists). The official install is:

```sh
curl -fsSL https://raw.githubusercontent.com/srjn45/warden/main/scripts/install.sh | bash
```

It needs only curl, installs `~/.local/bin/warden` plus the `wd` alias, and wires the user daemon service. Pin a release with `WARDEN_VERSION=<x.y.z>`. Homebrew, apt, rpm and AUR are no longer published, so do not mention them on the landing page. The hero or quickstart must use the exact command above.
