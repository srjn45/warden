---
title: Acceptable use & trademarks
description: Your responsibilities when connecting agent backends to warden, and the trademark/affiliation notice.
---

## Your credentials, your responsibility

warden is a **local orchestrator**: it drives agent-backend CLIs and services
using credentials **you** supply. You are responsible for complying with the
terms of service, usage policies, and rate limits of every backend and account
you connect (Claude Code / Anthropic, OpenAI Codex, Cursor, Google Antigravity,
Aider, OpenCode, goose, crush, and any others).

warden grants no rights to any third-party service and makes no representation
that a given automation or concurrency pattern — including running many agents in
parallel — is permitted under your plan. Check each provider's terms before
scaling up a fleet.

## Privacy & outbound traffic

By default warden runs entirely on your machine — no telemetry, no SaaS. The only
traffic that leaves your machine is what you explicitly enable:

- **Webhook / Slack alerts** (`notify.webhook_url`) — POSTs alerts to a URL you set.
- **Remote access** — binds a non-loopback address and serves the API/dashboard.
- **`warden savings --calibrate`** — sends truncated token samples to Anthropic's
  `count_tokens` endpoint using your `ANTHROPIC_API_KEY`.
- **Branch tracking** (`branch_track.enabled`, default off) — reads each agent
  branch's CI status by running `gh run list` against your Git host with your
  own `gh` credentials.

## Trademarks & affiliation

warden is an independent open-source project. It is **not** affiliated with,
endorsed by, or sponsored by Anthropic, OpenAI, Google, Cursor, Block, or any
other agent-backend vendor. "Claude" and "Claude Code" are trademarks of
Anthropic, PBC; "Codex" of OpenAI; and all other product and company names are
trademarks of their respective owners, used here for identification only.

## Reporting security issues

See [`SECURITY.md`](https://github.com/srjn45/warden/blob/main/SECURITY.md) for the
private disclosure channel and threat model.
