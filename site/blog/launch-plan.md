# warden — promotion plan & checklist

Companion to `launch-copy.md` (paste-and-go copy per channel).
Assets: GIFs in repo at `site/public/media/` (also copied to `~/warden-gifs/`).
Blog live: https://srjn45.github.io/warden/blog/running-a-fleet-of-claude-code-agents/

Track weekly: GitHub stars, Insights→Traffic referrers, release downloads,
stranger-opened issues.

## Phase 0 — Pre-flight (~1 hour, before posting anything)

- [ ] Walk the funnel in a private browser: repo → README (GIFs load) → docs
      site → blog → install (`go install` AND brew/deb/rpm). Fix anything broken.
- [ ] Repo metadata: description, homepage → docs site, topics — include
      backend names now that we're multi-backend: `ai-agents`, `claude-code`,
      `codex`, `aider`, `mcp`, `golang`, `self-hosted`, `tmux`, `devtools`.
- [ ] Social preview image set (Settings → Social preview) so links unfurl
      with the hero.
- [ ] Package-manager install one-liners near the top of the README.

## Phase 1 — Launch week

- [ ] **Day 1 (Tue–Thu, 8–10am ET): Show HN.** Link the REPO. First comment
      immediately (copy §1). Camp the thread 3 hours. Differentiators for the
      inevitable "why not tmux?": worktree isolation, dollar cost gate,
      MCP-driven fleet, multi-backend.
- [ ] Day 1 morning: r/ClaudeAI + r/ClaudeCode (copy §2, native GIF upload).
- [ ] Day 1–2: cross-post blog to dev.to + Hashnode, canonical URL → site.
- [ ] Day 2–3: r/selfhosted (§3), r/golang (§4), X/Bluesky thread (§5).
- [ ] Day 3–5: awesome-list PRs (§6): awesome-claude-code, awesome-mcp-servers,
      awesome-ai-agents, awesome-selfhosted + aider/codex/OpenCode ecosystem
      lists.
- [ ] All week: reply to every comment within hours; convert "does it do X?"
      into public GitHub issues.

## Phase 2 — Sustain (weeks 2–4, one item/week)

- [ ] Week 2 — blog: architecture deep-dive ("one binary, five faces:
      a single-writer daemon in Go") → r/golang + HN (regular submission).
- [ ] Week 3 — blog: the multi-backend story ("one cockpit for Claude Code,
      Codex, Aider + more") → each backend's community is its own channel.
- [ ] Week 4 — blog: real numbers — `warden savings --benchmark` + dollar
      figures from own fleet.
- [ ] Continuously: answer multi-agent-chaos questions on HN/Reddit; link
      warden only when it genuinely answers.

## Phase 3 — Compounding (month 2+)

- [ ] Newsletters: Golang Weekly, Console.dev, TLDR AI.
- [ ] Offer 10-min demo to agent-tooling YouTubers/podcasts (GIF rig can
      produce full screen recordings).
- [ ] Product Hunt — HOLD until v1.0 + quotable users.
- [ ] v1.0 release as its own news event ("warden 1.0: 8 agent backends"),
      second Show HN moment.
