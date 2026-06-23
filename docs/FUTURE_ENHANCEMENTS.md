# Warden Future Enhancements & Feature Roadmap

**Last Updated:** 2026-06-22
**Current Version:** v4.0.0 (+ unreleased: worktree GC & lifecycle hardening)

This document tracks potential improvements and new features for warden, organized
by category and priority. Each item includes effort estimates and implementation
notes.

> **Maintenance note:** This file is verified against the codebase, not just
> appended to. Before marking something "future," grep `internal/` first — several
> items below were already shipped while the roadmap still listed them as pending.
> When you finish a feature, move it to "Recently Completed" and delete its
> forward-looking entry.

---

## ✅ Recently Completed (since v3.13.0)

Verified present in `internal/` / `web/` as of 2026-06-22:

**Autonomy & resilience** (the major theme of recent work)
- **Auto-approve engine** — affirmative-option selection, destructive-action guard,
  allow/deny policy block, sticky-approve config, poller integration
  (`internal/approval`, `internal/poller`).
- **Auto-restart on failure** — `AutoRestart`/`RestartCount` on the session,
  capped resume (`internal/daemon/autorestart.go`, `internal/lifecycle`).
- **Rate-limit auto-resume** — detects the rate-limit banner, rolls reset times
  forward, resumes with a bare keypress / configurable prompt
  (`internal/daemon/ratelimit.go`, `internal/poller/detect.go`).
- **Context-token guard** — `token_guard` / `token_warn` / `token_critical` /
  `token_auto_compact` with warn + auto-compact (`internal/ctxtokens`).
- **Stuck / crash detection** — `stuckAfter` reclassification + crash-exit-code
  recording and events (`internal/poller/poller.go`).

**Lifecycle & storage**
- **Worktree GC & lifecycle hardening** — provenance tracking, `RemoveWorktree`
  provenance gating, `warden worktree ls`, `warden prune`, `worktree_keep_done` /
  `worktree_auto_prune` retention policy (`internal/lifecycle`, `internal/store`).
- **YAML config file** — migrated off env vars to a single config file with a
  documented key set (`internal/config`).

**Spawn & model**
- **Model selection per agent** — `--model` (CLI + MCP), aliases
  (`opus`/`sonnet`/`haiku`/`fable`), `model_default` config, MODEL column.
- **Per-agent permission mode** — `--permission-mode`, `set-permission-mode`,
  global `default_permission_mode`.
- **Shell-injection hardening** at spawn for model / permission-mode values.

**Observability**
- **Metrics / stats system** — `warden stats`, `internal/metrics`,
  `internal/pressure`, web Resources panel + Fleet stats
  (`web/src/components/ResourcesPanel.tsx`, `FleetStats.tsx`).

**Remote access & multi-device** (the flagship — access from a phone/tablet/anywhere)
- **Bearer-token auth** — `warden token generate` (256-bit `crypto/rand`),
  `WARDEN_TOKEN` env (kept off disk), constant-time compare, `?token=` for
  SSE/WS, non-loopback bind refused without a token, per-IP auth-failure
  rate-limiting (`internal/auth`, `internal/daemon/middleware.go`,
  `authlimit.go`).
- **Web UI auth** — token-entry modal on `401`, `localStorage` persistence,
  sign-out; the static SPA shell stays public so the modal can load
  (`web/src/lib/token.ts`, `api.ts`, `TokenModal.tsx`).
- **Mobile-responsive dashboard** — bottom nav, single-column grids, full-screen
  modal sheets (`web/src/styles/app.css`).
- **Setup docs** — LAN / Tailscale / Cloudflare Tunnel (`docs/USAGE.md`).

**Web UI**
- **Pipeline DAG visualization** — `web/src/components/PipelineDag.tsx`.
- **Timeline / activity view** — `EventTimeline.tsx`, `ActivityFeed.tsx`.
- **Cockpit / quick-spawn / attention queue** tabs.

**Inter-agent foundation** (the substrate for full collaboration, item #43)
- **Shared context store** — `ctx_set` / `ctx_cas` / `ctx_append` / `ctx_get` /
  `ctx_list` MCP tools with atomic CAS/append (`internal/ctxstore`).
- **Mailbox messaging** — `send_message` / `read_inbox` / `wait_for_message` MCP
  tools, corrupt-inbox resilience (`internal/mailbox`).
- **Approvals over MCP** — `list_approvals` / `approve`.

**From v3.13.0:** shell completion (bash/zsh/fish/powershell), agent
names/aliases (`--name`), improved actionable error messages.

---

## 🎯 Quick Wins (1-4 hours each)

### CLI & UX Improvements

#### 1. `warden ls --watch` ⭐ HIGH IMPACT — *not started*
**Effort:** 1-2 hours
**Value:** Immediate UX improvement

Live-updating agent list using the existing SSE endpoint (same one the web GUI uses).

```bash
warden ls --watch  # refreshes on every agent state change
```

**Implementation:**
- CLI client opens SSE connection to `/events`
- On each event, re-fetch `/sessions` and redraw the table
- Handle Ctrl+C gracefully
- Reuse existing SSE infrastructure from web GUI

---

#### 2. `warden pipeline validate` — *not started*
**Effort:** 1 hour
**Value:** Better DX, fewer errors

Validate pipeline YAML files before creating them. Today there is `create`, `list`,
`show`, `start`, `cancel`, `delete`, `emit`, `edit-job`, `retry` — but no `validate`.

```bash
warden pipeline validate -f pipeline.yaml
# Checks: DAG cycles, missing dependencies, invalid job IDs, required fields
```

**Implementation:**
- Extract validation logic from `pipeline create`
- Add new `validate` subcommand
- Return exit code 0 (valid) or 1 (invalid) for CI usage

---

#### 3. Pipeline templates — *not started*
**Effort:** 2 hours
**Value:** Faster pipeline authoring

Ship 3-4 common pipeline templates.

```bash
warden pipeline create --template analyze-implement-review
warden pipeline list-templates
```

**Templates:** `analyze-implement-review`, `parallel-tasks`, `test-fix-verify`,
`research-synthesis`.

**Implementation:** embed via `go:embed` in `internal/pipeline/`; `--template`
renders with placeholder substitution.

---

#### 4. `--version` build info — *not started*
**Effort:** 30 minutes
**Value:** Better debugging, support

Today `version` defaults to `"dev"` (`internal/cli/root.go`) with no commit/date.
Show detailed build info instead.

```bash
warden --version
# warden v4.0.0
# Commit: 3c57dca
# Built: 2026-06-22T...Z
# Go: go1.26.x  Platform: linux/amd64
```

**Implementation:** add build-time ldflags in goreleaser; `warden version --json`.

---

#### 5. Web dashboard keyboard shortcuts — *not started*
**Effort:** 2 hours
**Value:** Power-user productivity

`?` help overlay, `n` new agent, `/` focus filter, `r` refresh, `Esc` close,
`1-9` tab switch, `j/k` list nav. (Individual modals already handle local keydown;
there is no global shortcut layer yet.)

---

#### 6. Pre-commit hook auto-setup — *partial*
**Effort:** 30 minutes
**Value:** Fewer CI failures

`.githooks/` exists; auto-wire `git config core.hooksPath .githooks` in
`scripts/install.sh` and document it.

---

#### 7. Export/Import sessions — *not started*
**Effort:** 2 hours
**Value:** Backup, sharing, migration

```bash
warden export --all > all-sessions.json
warden import < backup.json
```

Serialize/insert `Session` structs (metadata only; does not recreate worktrees).

---

#### 8. Improve CLI test coverage to 50%+ — *ongoing*
**Effort:** 3-4 hours

CLI remains the lowest-covered package. Table-driven flag parsing, error paths,
output formatting, mocked daemon responses.

---

## 📊 Observability & Monitoring

#### 9. ~~Metrics/Stats system~~ — ✅ **DONE**
Shipped: `warden stats`, `internal/metrics`, `internal/pressure`, web Resources +
Fleet stats panels. See "Recently Completed."

---

#### 10. Enhanced structured logging (`slog`) — *not started*
**Effort:** 2-3 hours
**Value:** Better debugging

No `log/slog` usage in `internal/` yet — still scattered `log.Print`.

```bash
warden daemon --log-level debug --log-format json
```

Add a `slog.Logger` to the daemon; replace `log.Print` calls; structured fields.

---

#### 11. Agent performance history — *partial*
**Effort:** 4-6 hours

Live resource sampling exists via the metrics system; what's missing is persisted
per-agent *history* (runtime, turn count, files modified, context trend) and an
anomaly warning surface.

---

## 🌐 Remote Access & Multi-Device

#### 12 & 13. Remote access + `warden token generate` — ✅ DONE
Shipped: bearer-token auth, `warden token generate`, mobile-responsive UI, and
Tailscale / Cloudflare Tunnel docs. See "Recently Completed."

---

#### 14. Distributed warden (multi-machine) ⭐ ENTERPRISE — *not started*
**Effort:** 1-2 weeks

Central control plane aggregating multiple daemons; route/spawn by machine;
unified dashboard; load balancing. New `internal/cluster` package. Builds on the
now-shipped remote-access auth (#12).

---

## ⏰ Scheduling & Automation

#### 15. Scheduled agents/tasks ⭐ AUTOMATION — *not started*
**Effort:** 1-2 days
**Value:** Unattended, recurring runs

**Decision doc:** `docs/superpowers/specs/2026-06-10-warden-scheduled-pipelines-decision.md`

```bash
warden schedule create "Review pending PRs" --cron "0 9 * * *" --type pr-review
warden schedule create "Deploy" --at "2026-06-15 14:00"
warden schedule list / delete <id>
```

Store in `~/.warden/schedules.json`; daemon scheduler loop (check each minute);
`github.com/robfig/cron`. Note the existing "cron" hits in `internal/` are
rate-limit reset-time parsing, not a scheduler.

---

## 🔄 Auto-Restart & Resilience

#### 16. ~~Auto-restart on failure~~ — ✅ **DONE**
Shipped (`internal/daemon/autorestart.go`, capped resume). See "Recently Completed."

---

#### 17. Crash detection improvements — *partial*
**Effort:** 4 hours

Stuck-state reclassification (`stuckAfter`) and crash-exit-code recording exist.
Remaining: OOM-kill heuristics, "suggest `/compact` before crash," explicit
infinite-loop detection beyond the stuck timer.

---

## 🎨 Web UI Enhancements

#### 18. ~~Timeline view~~ — ✅ **DONE**
`web/src/components/EventTimeline.tsx`, `ActivityFeed.tsx`.

#### 19. Dark mode toggle — *not started*
**Effort:** 2 hours. CSS custom properties + `prefers-color-scheme` + LocalStorage
override + header toggle.

#### 20. Agent grouping/filtering — *not started*
**Effort:** 4 hours. Group by type/status/tag, collapsible groups, saved presets.
(`AgentGrid.tsx` exists but renders a flat grid.)

#### 21. Batch operations — *not started*
**Effort:** 3 hours. Multi-select + bulk terminate/delete/message.

---

## 🤖 Model Selection & Configuration

#### 22. ~~Model selection per agent~~ — ✅ **DONE**
`--model` (CLI + MCP), aliases, `model_default`, MODEL column.

#### 23. Agent templates/presets — *not started*
**Effort:** 3 hours
Save common spawn configs (`~/.warden/presets.yaml`):
```bash
warden preset save code-review --type pr-review --model opus --supervised
warden start --preset code-review --pr 1234
```

---

## 📱 Pipeline Enhancements

#### 24. ~~Pipeline visualization (DAG)~~ — ✅ **DONE**
`web/src/components/PipelineDag.tsx`.

#### 25. Pipeline MCP tools — *not started*
**Effort:** 4-6 hours
Pipelines are still CLI-only. Add `create_pipeline` / `start_pipeline` /
`show_pipeline` / `cancel_pipeline` to `internal/mcp/server.go` (which currently
exposes agent, ctx, mailbox, and approval tools — no pipeline tools).

#### 26. Pipeline pause/resume — *not started*
**Effort:** 4 hours. Add `paused` state; executor checks before spawning next job.

#### 27. Conditional pipeline steps — *not started*
**Effort:** 6 hours. `run_if: success|failure|always`; executor checks upstream
exit codes (default `always`).

---

## 🔍 Search & Discovery

#### 28. Full-text search — *not started*
**Effort:** 6-8 hours. In-memory search across subject/prompt/type/name/pane;
`warden search`, web search bar.

#### 29. Agent history/archive viewer — *not started*
**Effort:** 4 hours. Browse/search the `closed/` store. `warden history [--since]
[--type]`; web Archive tab.

#### 30. Tag system — *not started*
**Effort:** 3-4 hours. `Tags []string` on `Session`; `--tags`; filter/search by tag.

---

## 🔐 Security & Permissions

#### 31. Multi-user support — *not started* (complex)
**Effort:** 2-3 days. Per-user isolation, ACLs, shared pipelines (opt-in).

#### 32. Audit log — *not started*
**Effort:** 4 hours. `~/.warden/audit.jsonl`; who/what/when/where; `warden audit log`.

---

## 📦 Integrations

#### 33. Jira integration — *not started*
**Effort:** 1 day. Auto-fetch ticket summary on spawn; post digest on completion.

#### 34. Slack notifications — *not started*
**Effort:** 3-4 hours. Today `internal/notify` is desktop-only (osascript /
notify-send / log). Add a webhook channel that posts on attention-needed
transitions (`waiting_for_input`, `errored`, `orphaned`).

#### 35. GitHub PR auto-create on done — *not started*
**Effort:** 6 hours
```bash
warden done agent-123 --create-pr   # gh pr create --fill --body "$(warden digest <id>)"
```
Closes the development→PR loop; pairs well with the existing digest feature.

---

## ⚡ Performance & Scalability

#### 36. Goroutine-based concurrency — *partial*
**Effort:** 3-5 days remaining
The poller already runs background workers (approval worker, summarizer workers
draining off the tick loop, `wg`-tracked shutdown). Remaining: parallel batch
operations (bulk terminate/delete/status), worker-pool for resource-intensive ops,
parallel independent-job execution in the pipeline executor, load testing with
100+ agents.

---

## 🧪 Testing & Quality

#### 37. Integration test suite — *not started*
**Effort:** 1-2 days. End-to-end (spawn→work→terminate→cleanup, pipeline lifecycle,
restore, approvals) with a real daemon + tmux. No `*integration_test.go` yet.

#### 38. Benchmarking suite — *not started*
**Effort:** 4 hours. `Benchmark*` for spawn time, `ls` at scale, store I/O. None yet.

#### 39. Fuzz testing — *not started*
**Effort:** 4 hours. `go test -fuzz` for pipeline YAML, approvals prompt parser,
session JSON. No `Fuzz*` yet.

---

## 🌍 Platform Support

#### 40. Windows support — *not started*
**Effort:** 2-3 days (WSL2 for tmux). Service install + path handling.

#### 41. Docker/container support — *not started*
**Effort:** 2 days. Daemon Dockerfile, `~/.warden` volume, compose example.

---

## 📚 Documentation & Onboarding

#### 42. Interactive tutorial — *not started*
**Effort:** 1 day. First-run guided walkthrough (detect `~/.warden/tutorial-complete`).

#### 43. API documentation (OpenAPI) — *not started*
**Effort:** 4 hours. Generate `openapi.yaml`; serve Swagger UI at `/api/docs`.

---

## 🚀 Advanced Features

#### 44. Intelligent inter-agent collaboration ⭐ NEXT-GEN — *MVP done; advanced deferred*
**Design:** `docs/superpowers/specs/2026-06-14-intelligent-inter-agent-collaboration-design.md`

Shipped: shared context store + mailbox messaging + MCP tools (`ctx_*`,
`send_message`, `read_inbox`, `wait_for_message`); and the **file-conflict
detection MVP** — `internal/collab` polls active worktrees with `git diff` and
warns agents editing the same file via the mailbox, surfaced through
`warden collab conflicts` / `who-is-editing`, `GET /collab/conflicts`, and the
`get_collaboration_status` / `who_is_editing_file` MCP tools (`collab_enabled` /
`collab_interval` config). Deferred behind real usage (see the spec's
Appendix A): FSNotify real-time detection, work-overlap/dedup detection,
GitHub branch/CI tracking, collaboration groups.

#### 45. Agent chaining/handoff — *not started*
**Effort:** 1 day. `warden handoff <target-id> <message>` — spawn + seed context.
(Pipelines + mailbox cover some of this today.)

#### 46. Snapshot/checkpoint system — *not started*
**Effort:** 2 days. Checkpoint worktree (git stash) + transcript; restore.

#### 47. Plugin system — *not started*
**Effort:** 3-4 days. Custom task types + lifecycle hooks (Go plugin or WASM).

#### 48. AI-powered insights — *not started*
**Effort:** 2-3 days. Analyze historical patterns; suggest parallelization / hints.

---

## 📊 Priority Matrix

### 🔥 Do First (High Impact, Low Effort)
1. **`warden ls --watch`** — 1-2 h
2. **`--version` build info** — 30 min
3. **Pipeline `validate` + templates** — 3 h
4. **GitHub PR auto-create on done** — pairs with existing digest
5. **Export/import** — 2 h

### ⭐ Do Next (High Impact, Medium Effort)
6. ~~**Remote access + token generate**~~ — ✅ DONE (see "Recently Completed")
7. **Scheduled agents/tasks** — 1-2 days (decision doc exists)
8. **Slack/webhook notifications** — 3-4 h
9. **Pipeline MCP tools** — 4-6 h
10. **Structured logging (slog)** — 2-3 h

### 🎯 Nice to Have (Medium Impact)
11. Full-text search — 6-8 h
12. Tag system — 3-4 h
13. Agent history/archive viewer — 4 h
14. Dark mode — 2 h
15. Web keyboard shortcuts — 2 h
16. Batch operations — 3 h
17. Agent presets — 3 h

### 🔮 Future (High Effort or Complex)
18. Finish inter-agent collaboration — 1-2 weeks (foundation done)
19. Distributed warden — 1-2 weeks (after remote access)
20. Multi-user support — 2-3 days
21. Windows support — 2-3 days
22. Plugin system — 3-4 days
23. Snapshot/checkpoint — 2 days
24. AI-powered insights — 2-3 days

---

## 🎬 Recommended Implementation Order

1. **`warden ls --watch`** (1-2 h) — immediate UX win on existing SSE
2. ~~**Remote access + token generate**~~ ✅ DONE — the flagship; completed the
   unattended-operation story (run agents → watch/steer from anywhere)
3. **Scheduled agents/tasks** (1-2 days) — recurring automation
4. **GitHub PR auto-create on done** (6 h) — closes the dev→PR loop
5. **Slack/webhook notifications** (3-4 h) — remote awareness, pairs with remote access
6. **Pipeline `validate` + templates** (3 h) — lowers the barrier to pipelines
7. **Pipeline MCP tools** (4-6 h) — orchestrator can drive pipelines
8. **Structured logging / slog** (2-3 h) — debugging foundation
9. **Full-text search + history viewer** (~1.5 days) — manage larger fleets
10. **Finish inter-agent collaboration** (1-2 weeks) — next-gen, foundation already in

---

## 📝 Notes

- **Design specs** live in `docs/superpowers/specs/` — check for one before starting.
- Effort estimates are approximate.
- Some features are interdependent (remote access → token gen → distributed; metrics
  → performance history).
- Platform-specific work (macOS/Linux) may need separate implementations.

---

## 🤝 Contributing

When implementing features from this roadmap:

1. Check for a design spec in `docs/superpowers/specs/`
2. Write tests first (TDD where possible)
3. Update docs (README, FEATURES.md, USAGE.md)
4. Run `make verify` before committing
5. **Update this file:** move the finished item to "Recently Completed" and delete
   its forward-looking entry, so the roadmap stays honest

---

**Questions or suggestions?** Open an issue at https://github.com/srjn45/warden/issues
