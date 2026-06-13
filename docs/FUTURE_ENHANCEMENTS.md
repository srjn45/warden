# Warden Future Enhancements & Feature Roadmap

**Last Updated:** 2026-06-13  
**Current Version:** v3.13.0

This document tracks potential improvements and new features for warden, organized by category and priority. Each item includes effort estimates and implementation notes.

---

## ✅ Recently Completed (v3.13.0)

- Shell completion (bash/zsh/fish/powershell)
- Agent names/aliases with `--name` flag
- Improved error messages with actionable hints
- 26 commits, 15 files changed, 200+ new test cases

---

## 🎯 Quick Wins (1-4 hours each)

### CLI & UX Improvements

#### 1. `warden ls --watch` ⭐ HIGH IMPACT
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

#### 2. `warden validate` for pipelines
**Effort:** 1 hour  
**Value:** Better DX, fewer errors

Validate pipeline YAML files before creating them.

```bash
warden pipeline validate -f pipeline.yaml
# Checks: DAG cycles, missing dependencies, invalid job IDs, required fields
```

**Implementation:**
- Extract validation logic from `pipeline create`
- Add new `validate` subcommand
- Return exit code 0 (valid) or 1 (invalid) for CI usage

---

#### 3. Pipeline templates
**Effort:** 2 hours  
**Value:** Faster pipeline authoring

Ship 3-4 common pipeline templates in `~/.warden/templates/`.

```bash
warden pipeline create --template analyze-implement-review
warden pipeline create --template parallel-tasks
warden pipeline create --template test-fix-verify
warden pipeline list-templates
```

**Templates:**
- `analyze-implement-review.yaml` - Sequential refactoring flow
- `parallel-tasks.yaml` - Fan-out independent work
- `test-fix-verify.yaml` - TDD cycle
- `research-synthesis.yaml` - Multiple research → one synthesis job

**Implementation:**
- Embed templates via `go:embed` in `internal/pipeline/`
- `list-templates` shows available templates with descriptions
- `--template` flag renders template with placeholder substitution

---

#### 4. `--version` enhancement
**Effort:** 30 minutes  
**Value:** Better debugging, support

Show detailed build info instead of just version number.

```bash
warden --version
# Output:
# warden v3.13.0
# Commit: e4bace6
# Built: 2026-06-13T16:15:35Z
# Go: go1.26.2
# Platform: linux/amd64
```

**Implementation:**
- Add build-time ldflags in goreleaser: `-X ...buildDate={{ .Date }}`
- Update `internal/cli/root.go` version display
- Add `warden version --json` for machine parsing

---

#### 5. Web dashboard keyboard shortcuts
**Effort:** 2 hours  
**Value:** Power-user productivity

Add keyboard navigation to web UI with `?` help overlay.

**Shortcuts:**
- `?` - Show/hide help overlay
- `n` - New agent
- `/` - Focus search/filter
- `r` - Refresh
- `Esc` - Close modal/cancel
- `1-9` - Quick-switch to tabs
- `j/k` - Navigate agent list

**Implementation:**
- Global keyboard event listener in root Astro component
- Modal component for help overlay
- Respect input focus (disable shortcuts when typing)

---

#### 6. Pre-commit hook auto-setup
**Effort:** 30 minutes  
**Value:** Fewer CI failures

Auto-wire `.githooks/` during installation.

**Implementation:**
- Add to `scripts/install.sh`:
  ```bash
  git config core.hooksPath .githooks
  ```
- Pre-push hook runs `make verify-fast` (gofmt, vet, web tests, build)
- Document in README

---

#### 7. Export/Import sessions
**Effort:** 2 hours  
**Value:** Backup, sharing, migration

Export and import session metadata.

```bash
warden export agent-123 > backup.json
warden export --all > all-sessions.json
warden import < backup.json
```

**Implementation:**
- Export: serialize Session struct to JSON
- Import: validate, insert into store (skip duplicates)
- **Does not** recreate worktrees (metadata only)
- Useful for: backing up, moving to new machine, sharing with team

---

#### 8. Improve CLI test coverage to 50%+
**Effort:** 3-4 hours  
**Value:** Better reliability

Current: 26.1% (lowest in codebase). Target: 50%+.

**Focus areas:**
- Table-driven tests for flag parsing
- Error path coverage
- Output formatting tests
- Mock daemon responses

---

## 📊 Observability & Monitoring

#### 9. Metrics/Stats System ⭐ HIGH VALUE
**Effort:** 1-2 days  
**Value:** Critical for debugging, freeze investigation

**Design exists:** `docs/superpowers/specs/2026-06-09-warden-observability-design.md`

Per-agent resource tracking: RSS, CPU%, process count, uptime. System memory pressure. Daemon self-stats.

```bash
warden stats                    # live snapshot
warden stats --history          # recent history from JSONL log
warden stats --agent <id>       # per-agent detail
```

**Web UI:**
- Resources tab with live charts
- Per-agent resource breakdown
- Memory pressure indicator
- Historical graphs

**Endpoints:**
- `GET /metrics` - Current snapshot (JSON)
- `GET /metrics/history?limit=100` - Recent samples

**Implementation:**
- New `internal/metrics` package
- Collector samples every 10s, writes to JSONL
- Per-agent: aggregate process tree RSS/CPU via `ps`
- System: `vm_stat`, `sysctl` on macOS; `/proc/meminfo` on Linux
- Daemon: `runtime.NumGoroutine()`, own RSS

---

#### 10. Enhanced logging
**Effort:** 2-3 hours  
**Value:** Better debugging

Replace scattered `log.Print` with structured `slog` (stdlib).

```bash
warden daemon --log-level debug
warden daemon --log-format json
```

**Implementation:**
- Add `slog.Logger` to daemon
- Replace 30+ `log.Print` calls
- Levels: debug, info, warn, error
- Structured fields: `slog.Info("spawned", "agent", id, "type", typ)`

---

#### 11. Agent performance history
**Effort:** 4-6 hours  
**Value:** Trend analysis

Track and visualize agent metrics over time.

**Metrics:**
- Runtime duration
- Turn count
- Files modified
- Context token usage
- Status transitions

**Display:**
- Web dashboard timeline
- "This agent has been running 2 hours with 150 turns"
- Warn on anomalies (stuck, runaway context)

---

## 🌐 Remote Access & Multi-Device

#### 12. Remote access support ⭐ GAME CHANGER
**Effort:** 2-3 days  
**Value:** Access from anywhere

**Design exists:** `docs/superpowers/specs/2026-06-10-warden-remote-access-design.md`

Access warden web UI from phone, tablet, other machines.

**Features:**
- Bearer token authentication
- Configurable bind address: `WARDEN_BIND_ADDR=0.0.0.0:7979`
- Mobile-responsive web UI
- Recommended setup: Tailscale or Cloudflare Tunnel

**Implementation:**
- Auth middleware: check `Authorization: Bearer <token>` header
- WebSocket/SSE: accept token as query param (`?token=<t>`)
- Exempt loopback connections (local CLI works unchanged)
- `warden token generate` command
- Mobile CSS breakpoints in web UI
- Documentation for Tailscale/Cloudflare setup

**Security:**
- Daemon refuses non-loopback bind without `WARDEN_TOKEN` set
- 32-byte cryptographically random token
- Stateless validation (no token storage)

---

#### 13. Token generation command
**Effort:** 1 hour  
**Value:** Part of remote access

```bash
warden token generate
# Output: a3f8e2b4... (32-byte hex)

export WARDEN_TOKEN=$(warden token generate)
```

**Implementation:**
- `crypto/rand` for secure random
- Print to stdout, user exports manually
- Document in README

---

#### 14. Distributed warden - Multi-machine agent control ⭐ ENTERPRISE SCALE
**Effort:** 1-2 weeks  
**Value:** Scale across infrastructure, team-wide agent fleet

Control agents across multiple machines from a single dashboard.

**Features:**
- Central control plane aggregates multiple warden daemons
- Discover and register remote warden instances
- Route commands to appropriate machine
- Unified web dashboard showing all agents across all machines
- Load balancing: spawn agents on least-loaded machine
- Machine health monitoring

**Architecture:**
- Each machine runs local warden daemon (unchanged)
- New "warden control" service aggregates multiple daemons
- gRPC or HTTP for inter-daemon communication
- Service discovery via mDNS, Consul, or static config

**Commands:**
```bash
# Register a remote warden instance
warden cluster add machine-2 https://192.168.1.10:7979 --token <token>
warden cluster list

# Spawn on specific machine
warden start "..." --machine machine-2

# Spawn with auto-placement (load balancing)
warden start "..." --auto-place

# Dashboard shows agents from all machines
warden ls --all-machines
```

**Implementation:**
- New `internal/cluster` package
- Daemon registry in control plane
- Proxy layer for routing commands
- Aggregate SSE streams from all daemons
- Web UI: machine filter dropdown

**Use cases:**
- Large teams sharing agent infrastructure
- Dedicated high-memory machines for context-heavy agents
- Geographic distribution (spawn close to repo/data)
- Failover: if one machine goes down, spawn elsewhere

---

## ⏰ Scheduling & Automation

#### 15. Scheduled agents/tasks ⭐ AUTOMATION
**Effort:** 1-2 days  
**Value:** Unattended automation, recurring tasks

Run agents on a schedule via cron-like syntax.

**Features:**
- Cron expression support
- One-time scheduled runs
- Recurring scheduled runs
- Schedule templates (daily, weekly, etc.)
- Timezone support

**Commands:**
```bash
# Schedule a recurring agent (every day at 9am)
warden schedule create "Review pending PRs" \
  --cron "0 9 * * *" \
  --type pr-review

# One-time scheduled run
warden schedule create "Deploy to prod" \
  --at "2026-06-15 14:00" \
  --type deployment

# List scheduled tasks
warden schedule list

# Cancel a scheduled task
warden schedule delete <id>
```

**Implementation:**
- Store schedules in `~/.warden/schedules.json`
- Daemon runs scheduler loop (check every minute)
- Use cron parser library (`github.com/robfig/cron`)
- Spawn agent when schedule matches
- Track last run time, next run time
- Web UI: schedules tab with calendar view

**Use cases:**
- Daily PR review sweeps
- Nightly security scans
- Weekly dependency updates
- Monthly report generation

---

## 🔄 Auto-Restart & Resilience

#### 16. Auto-restart on failure
**Effort:** 1 day  
**Value:** Resilience, unattended operation

**Design exists:** `docs/superpowers/specs/2026-06-10-warden-auto-restart-design.md`

Automatically resume errored agents (with retry cap).

```bash
warden start "..." --auto-restart
warden start "..." --auto-restart --max-restarts 5
```

**Implementation:**
- Store `AutoRestart` and `RestartCount` in Session
- Hook: on `SessionEnd` with non-zero exit → check cap → spawn resume
- Exponential backoff between restarts
- Hard cap (default 3, configurable)
- Surface restart count in `ls`/web

---

#### 17. Crash detection improvements
**Effort:** 4 hours  
**Value:** Faster recovery

Better detection of hung/crashed agents, with recovery suggestions.

**Enhancements:**
- Detect infinite loops (pane unchanged for >30min while "working")
- Detect OOM kills (exit code analysis)
- Suggest `/compact` before crash
- Auto-notify on detected crash

---

## 🎨 Web UI Enhancements

#### 16. Dark mode toggle
**Effort:** 2 hours  
**Value:** Accessibility, user preference

```javascript
// Auto-detect system preference + manual toggle
<ThemeToggle />
```

**Implementation:**
- CSS custom properties for colors
- `prefers-color-scheme` media query
- LocalStorage for manual override
- Icon toggle in header

---

#### 17. Agent grouping/filtering
**Effort:** 4 hours  
**Value:** Fleet organization

Group agents by type, status, or custom tags. Save filter presets.

**UI:**
- Dropdown: "Group by: Type / Status / Tag"
- Collapsible groups
- Search within groups
- Save filter as preset

---

#### 18. Timeline view
**Effort:** 1 day  
**Value:** Visual history

Visual timeline of agent activity.

**Features:**
- Horizontal timeline with events
- Click event to see details
- Zoom in/out on time ranges
- Filter by agent

---

#### 19. Batch operations
**Effort:** 3 hours  
**Value:** Manage many agents

Select multiple agents for bulk actions.

**Operations:**
- Bulk terminate
- Bulk delete
- Send same message to multiple agents
- Checkbox selection UI

---

## 🤖 Model Selection & Configuration

#### 20. Model selection per agent
**Effort:** 4-6 hours  
**Value:** Flexibility, cost control

**Design exists:** `docs/superpowers/specs/2026-06-10-warden-model-selection-design.md`

Override default model per agent.

```bash
warden start "..." --model opus-4.8
warden start "..." --model haiku-4.5  # fast, cheap
warden start "..." --model fable-5    # long-running work
```

**Implementation:**
- Add `Model` field to Session
- Pass `--model <name>` to `claude` command
- Show model in `ls` output (new column)
- Web UI: model dropdown in spawn form
- Validate model name against known list

---

#### 21. Agent templates/presets
**Effort:** 3 hours  
**Value:** Repeatability

Save common spawn configurations as reusable presets.

```bash
warden preset save code-review \
  --type pr-review --model opus-4.8 --supervised

warden start --preset code-review --pr 1234
warden preset list
```

**Storage:** `~/.warden/presets.yaml`

---

## 📱 Pipeline Enhancements

#### 22. Pipeline MCP tools
**Effort:** 4-6 hours  
**Value:** Orchestrator integration

Expose pipelines to MCP so orchestrator Claude can manage them.

**Tools:**
- `create_pipeline` - Create from YAML spec
- `start_pipeline` - Start a created pipeline
- `show_pipeline` - Get status, job outputs
- `cancel_pipeline` - Cancel running pipeline

**Implementation:**
- Add tools to `internal/mcp/server.go`
- Wrap existing daemon pipeline routes
- Currently pipelines are CLI-only

---

#### 23. Pipeline visualization
**Effort:** 1-2 days  
**Value:** Understand complex pipelines

DAG graph in web UI showing job dependencies.

**Features:**
- Visual graph (D3.js or similar)
- Real-time status colors
- Click job → show details
- Highlight critical path

---

#### 24. Pipeline pause/resume
**Effort:** 4 hours  
**Value:** Control long pipelines

Pause a running pipeline, resume later.

```bash
warden pipeline pause <id>
warden pipeline resume <id>
```

**Implementation:**
- Add `paused` state to pipeline
- Executor checks before spawning next job
- Can edit pending jobs while paused

---

#### 25. Conditional pipeline steps
**Effort:** 6 hours  
**Value:** Advanced workflows

Jobs that only run if upstream succeeds/fails.

```yaml
jobs:
  - id: test
    prompt: "Run the test suite"
  - id: fix
    depends_on: [test]
    run_if: failure  # only if test fails
    prompt: "Fix the failing tests"
  - id: deploy
    depends_on: [test]
    run_if: success  # only if test passes
    prompt: "Deploy to production"
```

**Implementation:**
- Add `run_if: success|failure|always` to job spec
- Executor checks upstream job exit codes
- Default: `always`

---

## 🔍 Search & Discovery

#### 26. Full-text search
**Effort:** 6-8 hours  
**Value:** Find agents quickly

Search agent subjects, prompts, terminal output.

```bash
warden search "auth refactor"
warden search --type development --status done "api"
```

**Web UI:** Search bar in header

**Implementation:**
- In-memory search (no external index)
- Search across: subject, prompt, type, name, pane excerpt
- Fuzzy matching (levenshtein distance)

---

#### 27. Agent history/archive viewer
**Effort:** 4 hours  
**Value:** Browse completed work

Browse and search archived (closed) sessions.

```bash
warden history
warden history --since 2026-06-01
warden history --type pr-review
```

**Web UI:** Archive tab with search/filter

---

#### 28. Tag system
**Effort:** 3-4 hours  
**Value:** Organization

Tag agents with custom labels.

```bash
warden start "..." --tags bug,urgent,auth
warden ls --tag urgent
warden search --tag bug
```

**Implementation:**
- Add `Tags []string` to Session
- CLI: `--tags` flag (comma-separated)
- Web: tag input widget
- Filter/search by tag

---

## 🔐 Security & Permissions

#### 29. Multi-user support
**Effort:** 2-3 days (complex)  
**Value:** Team collaboration

Multiple users on same daemon with isolated sessions.

**Features:**
- Per-user session isolation
- User-specific permissions
- Shared pipelines (opt-in)

**Complexity:** High - requires auth system, ACLs

---

#### 30. Audit log
**Effort:** 4 hours  
**Value:** Compliance, debugging

Track all warden operations.

```bash
warden audit log
warden audit log --since 2026-06-01 --user srajan
```

**Log entries:**
- Who spawned/terminated/deleted what
- When
- From where (IP, hostname)

**Storage:** `~/.warden/audit.jsonl`

---

## 📦 Integrations

#### 31. Jira integration
**Effort:** 1 day  
**Value:** Project management sync

Auto-populate ticket info from Jira, update on completion.

```bash
warden start PROJ-350 --type development
# Auto-fetches ticket summary, description from Jira API
# On agent completion, posts comment with digest
```

**Configuration:**
```yaml
# ~/.warden/integrations.yaml
jira:
  url: https://company.atlassian.net
  token: $JIRA_API_TOKEN
```

---

#### 32. Slack notifications
**Effort:** 3-4 hours  
**Value:** Team awareness

Post to Slack when agents need attention.

```bash
# Notify on: waiting_for_input, errored, orphaned
WARDEN_SLACK_WEBHOOK=https://...
```

**Implementation:**
- Poller checks configured webhook
- On status transition to attention-needed state → POST to webhook
- Configurable per-state

---

#### 33. GitHub integration enhancements
**Effort:** 6 hours  
**Value:** Streamlined PR workflow

Auto-create PR from development agents, comment with digest.

```bash
warden done agent-123 --create-pr
# Creates PR on GitHub, posts digest as first comment
```

**Implementation:**
- Use `gh` CLI
- Extract branch, repo from session
- Run `gh pr create --fill --body "$(warden digest <id>)"`

---

## ⚡ Performance & Scalability

#### 34. Goroutine-based concurrency for agent management
**Effort:** 3-5 days  
**Value:** Better performance, scalability for large fleets

Refactor daemon to use goroutines for concurrent agent operations.

**Current state:**
- Some operations are synchronous
- Potential bottlenecks with many agents
- Sequential processing of certain operations

**Improvements:**
- Concurrent agent monitoring (one goroutine per agent)
- Parallel status checks via goroutine pool
- Non-blocking agent spawn/terminate
- Concurrent pipeline job execution
- Worker pool for resource-intensive operations

**Implementation areas:**

**1. Agent monitoring (HIGH PRIORITY)**
```go
// Current: sequential polling
// Improved: one goroutine per agent
for _, agent := range agents {
    go func(a *Session) {
        for {
            checkAgentStatus(a)
            time.Sleep(pollInterval)
        }
    }(agent)
}
```

**2. Batch operations**
```go
// Parallel terminate, delete, status fetch
var wg sync.WaitGroup
for _, agentID := range ids {
    wg.Add(1)
    go func(id string) {
        defer wg.Done()
        terminateAgent(id)
    }(agentID)
}
wg.Wait()
```

**3. SSE event broadcasting**
```go
// Non-blocking event fan-out to all web clients
for _, client := range sseClients {
    go func(c *Client) {
        c.Send(event)
    }(client)
}
```

**4. Pipeline executor**
```go
// Parallel job execution for independent jobs
for _, job := range readyJobs {
    go executor.RunJob(job)
}
```

**Architecture decisions:**
- Worker pool size (bounded vs unbounded goroutines)
- Channel-based communication vs mutex-protected state
- Context cancellation for graceful shutdown
- Error aggregation from parallel operations

**Testing:**
- Race detector (`go test -race`)
- Load testing with 100+ concurrent agents
- Benchmark suite for throughput improvements

**Expected benefits:**
- 2-5x faster `warden ls` with many agents
- Sub-second response for batch operations
- Better CPU utilization on multi-core systems
- Foundation for distributed warden (item #14)

---

## 🧪 Testing & Quality

#### 35. Integration test suite
**Effort:** 1-2 days  
**Value:** Confidence in releases

End-to-end tests for common workflows.

**Workflows to test:**
- Spawn → work → terminate → cleanup
- Pipeline create → start → completion
- Restore orphaned agent
- Approvals inbox

**Implementation:**
- `internal/daemon/integration_test.go`
- Start real daemon, real tmux sessions
- Cleanup after each test

---

#### 36. Benchmarking suite
**Effort:** 4 hours  
**Value:** Performance regression tracking

Performance benchmarks for critical paths.

**Benchmarks:**
- Agent spawn time
- `ls` query performance (100, 1000 agents)
- Pipeline execution overhead
- Store read/write speed

**Implementation:**
- Go benchmark tests (`Benchmark*`)
- CI runs benchmarks, tracks trends

---

#### 37. Fuzz testing
**Effort:** 4 hours  
**Value:** Security, robustness

Fuzz critical parsers.

**Targets:**
- Pipeline YAML parser
- Approvals prompt parser
- Session JSON deserializer

**Implementation:**
- Go native fuzzing (`go test -fuzz`)

---

## 🌍 Platform Support

#### 38. Windows support
**Effort:** 2-3 days  
**Value:** Broader audience

Run warden on Windows (WSL2 required for tmux).

**Requirements:**
- WSL2 with tmux
- Windows service installation
- Path handling (Windows vs WSL paths)

**Challenges:**
- tmux not native on Windows
- Service management (Task Scheduler vs systemd)

---

#### 39. Docker/container support
**Effort:** 2 days  
**Value:** Portable, reproducible

Run warden daemon in container.

**Features:**
- Dockerfile for daemon
- Volume mounts for `~/.warden` persistence
- docker-compose example
- Agents in containers (nested tmux)

---

## 📚 Documentation & Onboarding

#### 40. Interactive tutorial
**Effort:** 1 day  
**Value:** Better onboarding

First-run tutorial in TUI/web.

**Steps:**
1. Welcome screen
2. Spawn your first agent
3. Attach and interact
4. Check status with `ls`
5. Terminate and clean up

**Implementation:**
- Detect first run (no `~/.warden/tutorial-complete`)
- Interactive walkthrough with guided prompts
- Skip option

---

#### 41. Video demos/screencasts
**Effort:** Varies  
**Value:** Visual learning

Record common workflows, embed in docs.

**Videos:**
- Quick start (5 min)
- Pipelines deep-dive (10 min)
- Web dashboard tour (5 min)
- Advanced tips (10 min)

---

#### 42. API documentation
**Effort:** 4 hours  
**Value:** Integration developers

OpenAPI/Swagger for REST API.

**Implementation:**
- Generate `openapi.yaml` from code
- Serve Swagger UI at `/api/docs`
- Auto-generated from comments

---

## 🚀 Advanced Features

#### 43. Intelligent inter-agent communication & collaboration ⭐ NEXT-GEN
**Effort:** 1-2 weeks  
**Value:** Prevent conflicts, enable true multi-agent collaboration

Agents become aware of each other, detect overlapping work, and coordinate automatically.

**Features:**
- **File conflict detection**: Agents detect when another agent is modifying the same files
- **Feature overlap detection**: Agents analyze each other's work to detect related/overlapping features
- **Auto-coordination**: Agents can negotiate who handles what, merge strategies
- **Shared context**: Agents can query "what is agent X working on?" and adjust their approach
- **Conflict resolution**: Agents propose merge strategies when edits overlap
- **Mutual awareness**: Each agent knows what others are doing in real-time

**Commands:**
```bash
# Enable collaboration mode for related agents
warden collab create auth-feature --agents agent-123,agent-456
warden collab list

# Agent queries from inside session
warden collab status  # "agent-456 is editing auth.go:45-80"
warden collab ask agent-456 "Are you handling the JWT validation?"
```

**Implementation:**
- New `internal/collab` package
- Collaboration groups (explicit or auto-detected by git branch/files)
- File watch system: track which agent has which files open
- Git diff analysis: detect overlapping line ranges
- MCP tools for agents:
  - `list_collaborating_agents` - see other agents in same collaboration space
  - `get_agent_status` - what files/features is another agent working on
  - `send_coordination_message` - ask another agent a question
  - `propose_work_split` - negotiate work division
- Real-time file lock hints: "Warning: agent-456 is currently editing this file"
- Smart merge suggestions when both agents commit

**Use cases:**
- Multiple agents refactoring same codebase component
- Parallel feature development with shared files
- Agent A implements, agent B reviews - coordinated handoff
- Auto-detect when agents duplicate work and merge efforts

**Challenges:**
- Performance overhead of cross-agent awareness
- Conflict resolution complexity
- Determining when collaboration is needed vs overhead

---

#### 44. Agent chaining/handoff
**Effort:** 1 day  
**Value:** Flexible workflows

One agent explicitly hands off to another (more flexible than pipelines).

```bash
# Inside agent A:
warden handoff agent-B "Context: I finished the auth refactor. Next: review the changes."
# Agent B receives context, starts working
```

**Implementation:**
- `warden handoff <target-id> <message>`
- Creates new agent, sends message with context
- Original agent can terminate or continue

---

#### 45. Shared worktree pool
**Effort:** 2-3 days (complex)  
**Value:** Collaboration

Multiple agents work in same worktree with conflict resolution.

**Challenges:**
- Git merge conflicts
- File locking
- Coordination overhead

**Use case:** Multiple agents work on different files in same large refactor

---

#### 46. Snapshot/checkpoint system
**Effort:** 2 days  
**Value:** Experimentation safety

Checkpoint agent state, restore to checkpoint.

```bash
warden checkpoint create agent-123 "before-risky-change"
# ... agent does risky work ...
warden checkpoint restore agent-123 "before-risky-change"
```

**Implementation:**
- Save: worktree state (git stash), conversation transcript
- Restore: reset worktree, resume conversation from checkpoint

---

#### 47. Plugin system
**Effort:** 3-4 days  
**Value:** Extensibility

Load custom plugins for new task types, lifecycle hooks.

**Plugin API:**
- Register custom task types
- Hook into lifecycle events (pre-spawn, post-terminate)
- Custom commands

**Implementation:**
- Go plugin system or WASM
- Plugin manifest in `~/.warden/plugins/`

---

#### 48. AI-powered insights
**Effort:** 2-3 days  
**Value:** Optimization suggestions

Analyze agent patterns, suggest optimizations.

**Insights:**
- "This pipeline could be 30% faster if jobs X and Y run in parallel"
- "Agent A often gets stuck at the same step - consider adding a hint"
- "Your agents average 200k tokens - consider using pipelines to keep contexts smaller"

**Implementation:**
- Analyze historical data
- Pattern detection (common stuck points, bottlenecks)
- `claude -p` to generate insights from patterns

---

## 📊 Priority Matrix

### 🔥 Do First (High Impact, Low Effort)

1. **`warden ls --watch`** - 1-2 hours
2. **Pipeline templates** - 2 hours
3. **`--version` enhancement** - 30 min
4. **Pre-commit hook** - 30 min
5. **Export/import** - 2 hours

### ⭐ Do Next (High Impact, Medium Effort)

6. **Metrics/stats system** - 1-2 days
7. **Remote access** - 2-3 days
8. **Model selection** - 4-6 hours
9. **Auto-restart** - 1 day
10. **Pipeline visualization** - 1-2 days
11. **Scheduled agents/tasks** - 1-2 days

### 🎯 Nice to Have (Medium Impact)

12. **Tag system** - 3-4 hours
13. **Agent grouping/filtering** - 4 hours
14. **Timeline view** - 1 day
15. **Dark mode** - 2 hours
16. **Jira/Slack integrations** - 1-2 days
17. **Goroutine-based concurrency** - 3-5 days

### 🔮 Future (High Effort or Complex)

18. **Distributed warden (multi-machine)** - 1-2 weeks
19. **Intelligent inter-agent communication** - 1-2 weeks
20. **Multi-user support** - 2-3 days
21. **Windows support** - 2-3 days
22. **Plugin system** - 3-4 days
23. **Shared worktree pool** - 2-3 days
24. **AI-powered insights** - 2-3 days

---

## 🎬 Recommended Implementation Order

1. **`warden ls --watch`** (1-2 hours) - Immediate UX win, builds on existing SSE
2. **Metrics/stats system** (1-2 days) - Critical for observability, spec already exists
3. **Remote access** (2-3 days) - Game-changing feature, spec exists
4. **Scheduled agents/tasks** (1-2 days) - High value automation feature
5. **Model selection** (4-6 hours) - High user demand, straightforward
6. **Pipeline templates** (2 hours) - Makes pipelines more accessible
7. **Auto-restart** (1 day) - Resilience improvement, spec exists
8. **Goroutine-based concurrency** (3-5 days) - Performance foundation for scaling
9. **Pipeline visualization** (1-2 days) - Better understand complex workflows
10. **Full-text search** (6-8 hours) - Helps manage large fleets
11. **Web keyboard shortcuts** (2 hours) - Power-user productivity
12. **Dark mode** (2 hours) - Quick win, high user satisfaction
13. **Distributed warden** (1-2 weeks) - Enterprise-scale multi-machine control
14. **Intelligent inter-agent communication** (1-2 weeks) - Next-gen collaboration

---

## 📝 Notes

- **Design specs exist** for several features (marked above) in `docs/superpowers/specs/`
- Effort estimates are approximate; actual time may vary based on complexity
- Some features are interdependent (e.g., remote access requires token generation)
- Platform-specific features (macOS/Linux) may require separate implementations
- Integration features require external service credentials/setup

---

## 🤝 Contributing

When implementing features from this roadmap:

1. Check if a design spec exists in `docs/superpowers/specs/`
2. Write tests first (TDD where possible)
3. Update documentation (README, FEATURES.md, USAGE.md)
4. Run `make verify` before committing
5. Update this file with progress/learnings

---

**Questions or suggestions?** Open an issue at https://github.com/srjn45/warden/issues
