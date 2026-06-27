# warden — launch copy (paste-and-go)

Assets: hero.gif, tui-cockpit.gif, web-monitor.gif live at
https://srjn45.github.io/warden/media/<name>
Repo: https://github.com/srjn45/warden  ·  Docs: https://srjn45.github.io/warden/

---

## 1. Hacker News (Show HN)

**Title** (80-char limit; link to the REPO, not the blog):

    Show HN: Warden – run a fleet of Claude Code agents without losing your mind

Alt titles if you want to A/B:
- `Show HN: Warden – a cockpit for running many Claude Code agents at once`
- `Show HN: Warden – self-hosted orchestrator for Claude Code agent fleets`

**First comment** (post this yourself, immediately after submitting):

> Author here. I built this because I got good enough at Claude Code that one
> agent stopped being enough — I'd have a refactor, a test pass, and a PR review
> running in three terminals, then a fourth, and I'd lose track of which window
> was which, which one was blocked waiting for me to approve something, how much
> context each had burned, and what it was all costing.
>
> Warden is a single Go binary that spawns each agent in its own isolated git
> worktree (so parallel agents don't collide on the same tree), then gives you
> one cockpit — a tmux TUI or a self-hosted web dashboard — to watch and talk to
> all of them. It prices each agent's real Claude usage in dollars with a budget
> gate, and it exposes the whole fleet as MCP tools so an orchestrator Claude
> session can drive the other agents.
>
> It's a local daemon writing to a file-based JSON store. No database, no SaaS,
> no telemetry — runs on your laptop or a box you SSH into. Apache-2.0.
>
> Happy to answer anything about the architecture (single-writer daemon, loopback
> REST API, the worktree isolation, how cost/token accounting works). What I'd
> most like to know: if you run multiple agents today, what's the part that
> actually hurts?

**Tips:** submit Tue–Thu ~8–10am ET. Don't editorialize the title. Reply to
every early comment within the first 2 hours — that engagement is what keeps it
on the front page.

---

## 2. r/ClaudeAI  and  r/ClaudeCode

**Title:**

    I built a self-hosted cockpit for running a whole fleet of Claude Code agents (open source)

**Body:**

> I kept spinning up more Claude Code agents than I could keep track of — a
> refactor here, a test pass there, a PR review in a third terminal — and I was
> spending more energy managing the windows than the agents were saving me.
>
> So I built **warden**: a single Go binary that spawns, watches, and tears down
> Claude Code agents, each in its own isolated git worktree, from one place.
>
> The TUI cockpit — agents list, an approvals row for anything blocked on you,
> and the selected agent's live session, all in one tmux view:
>
> [tui-cockpit.gif]
>
> Same fleet as a self-hosted web dashboard with per-agent + fleet CPU/memory:
>
> [web-monitor.gif]
>
> A few things that make it different from just running tmux yourself:
> - **Cost visibility** — `warden spend` prices each agent's real usage in
>   dollars with a budget gate; `warden savings` tracks tokens it keeps out of
>   context with an A/B benchmark.
> - **It drives Claude itself** — `warden mcp` exposes the fleet as MCP tools, so
>   an orchestrator session can spawn and coordinate the other agents.
> - **No SaaS, no telemetry** — local daemon + JSON file store. Apache-2.0.
>
> Repo: https://github.com/srjn45/warden
> Docs: https://srjn45.github.io/warden/
>
> It's free and I'm building it in the open — if you run more than one agent at a
> time I'd love feedback on where it falls short.

**Note:** upload the two GIFs directly into the Reddit post (drag them in) rather
than hotlinking — Reddit-native media gets far more reach.

---

## 3. r/selfhosted

**Title:**

    Warden – self-hosted orchestrator for Claude Code AI agents. One Go binary, JSON file store, no telemetry.

**Body:**

> If you're running Claude Code (or any agentic coding setup) and you've started
> running more than one at a time, warden gives you a single cockpit for the
> whole fleet — spawn, monitor, talk to, and tear down agents, each in its own
> isolated git worktree.
>
> The selfhosted-relevant bits:
> - **One static Go binary.** No runtime, no container required.
> - **Local daemon + file-based JSON store.** No Postgres, no Redis, no cloud
>   account. The web dashboard is embedded and served over a loopback REST API.
> - **Nothing leaves the machine.** No SaaS, no telemetry, Apache-2.0. Run it on
>   your laptop or a homelab box you SSH/Tailscale into.
> - Tracks real cost in dollars per agent with a budget gate, so an agent can't
>   quietly run up a bill.
>
> [web-monitor.gif]
>
> Repo: https://github.com/srjn45/warden

---

## 4. r/golang

**Title:**

    Warden: orchestrating a fleet of AI coding agents from a single-writer Go daemon

**Body:**

> Sharing a Go project I've been building: warden orchestrates fleets of Claude
> Code agents. Posting here for the architecture rather than the AI angle, since
> a few of the design decisions might be interesting:
>
> - **Single-writer daemon.** One `warden daemon` process is the only writer to
>   an on-disk JSON session store, serving a loopback REST API + a background
>   poller. All the other subcommands (`ls`, `start`, `attach`, `send`, …) are
>   thin HTTP clients. No DB, which keeps the whole thing a single binary.
> - **One binary, multiple faces.** The same binary is the daemon, the CLI, a
>   tmux-composited TUI, an embedded web SPA, and a stdio MCP server.
> - **Worktree isolation** for parallel write agents so they don't fight over one
>   git tree.
> - **Spec-first API** — OpenAPI → generated strict server, no hand-written
>   handlers.
>
> Single static binary, Apache-2.0. Repo: https://github.com/srjn45/warden —
> happy to talk through any of the trade-offs.

---

## 5. X / Twitter  (and Bluesky — same text)

**Thread:**

**1/**
> I kept running more Claude Code agents than I could track — a refactor, a test
> pass, a PR review, all in different terminals — and lost track of which was
> which, which was blocked, and what it all cost.
>
> So I built warden: one cockpit for the whole fleet. 🧵
>
> [hero.gif]

**2/**
> Spawn an agent against a task and walk away. Each write agent gets its own
> isolated git worktree, so parallel agents never collide on the same tree.
>
>     warden start "review the auth module for security issues"

**3/**
> Watch the whole fleet from one tmux cockpit — agents list, an approvals row for
> anything blocked on you, and the selected agent's live session side by side.
>
> [tui-cockpit.gif]

**4/**
> Not at your terminal? Same fleet as a self-hosted web dashboard with per-agent
> and fleet-total CPU/memory + real cost.
>
> No SaaS. No telemetry. One Go binary + a JSON file store.
>
> [web-monitor.gif]

**5/**
> The part I didn't expect to care about: it prices each agent's real Claude
> usage in dollars with a budget gate. Running a fleet, cost stops being abstract.

**6/**
> And `warden mcp` exposes the whole fleet as MCP tools — so an orchestrator
> Claude session can spawn and coordinate the other agents. Claude managing a
> team of Claudes.

**7/**
> It's open source (Apache-2.0) and free. If you run more than one coding agent
> at a time, I'd love for you to try it:
>
> https://github.com/srjn45/warden

**Single-tweet version** (if you don't want a thread):

> Running more than one Claude Code agent and losing track of them all?
>
> warden = one cockpit for the whole fleet. Spawn → watch → price in $ → tear
> down. Each agent in its own git worktree. One Go binary, self-hosted, no
> telemetry. Open source.
>
> [tui-cockpit.gif]
> https://github.com/srjn45/warden

---

## 6. awesome-list PR entry (one-liner)

> **[warden](https://github.com/srjn45/warden)** — Self-hosted cockpit for
> running a fleet of Claude Code agents: spawn each in an isolated git worktree,
> watch them in a TUI or web dashboard, track real cost in dollars, and drive the
> whole fleet over MCP. Single Go binary, no SaaS, Apache-2.0.

Targets: awesome-claude, awesome-claude-code, awesome-mcp-servers,
awesome-ai-agents, awesome-selfhosted (open a PR adding the line above).
