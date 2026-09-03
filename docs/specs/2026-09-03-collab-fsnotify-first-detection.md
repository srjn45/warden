# Collab monitor: fsnotify-first file-conflict detection

**Date:** 2026-09-03  
**Status:** Implementing  
**Problem:** The file-conflict monitor (`internal/collab`) runs `git diff --name-only HEAD` on every fsnotify debounce and every `collab.interval` poll, for every active agent worktree. In large monorepos this hammers shared `.git` state, contends with user `git pull` / `git add` (via `index.lock`), and leaves commands failing. The dashboard also polls `GET /collab/conflicts` every 5s, multiplying git subprocesses.

## Goals

1. **Real-time detection without per-edit git** — fsnotify records dirty file paths in memory; conflict checks compare those sets across agents.
2. **Git as reconcile backstop only** — `git diff` runs on a slow interval (default `2m`) to refresh state after commits/reverts and when fsnotify misses events.
3. **No git on API reads** — `Conflicts()` serves a cached snapshot updated by the monitor loop.
4. **Fail open on lock contention** — read-only git uses `GIT_OPTIONAL_LOCKS=1`; skip reconcile when `index.lock` is present.
5. **Graceful degradation** — when fsnotify is unavailable, fall back to git-on-poll (current behaviour).

## Non-goals (this change)

- Metrics collector `git status --porcelain` (separate follow-up).
- Dashboard poll interval reduction (cache makes it cheap).
- Hot-reload of `collab.git_reconcile_interval` (restart-only, like `collab.interval`).

## Design

### Data structures (`Monitor`)

```
dirty   map[worktree]set[repo-relative path]  // fsnotify + git reconcile
cached  []Conflict                             // last computed conflicts
cacheValid bool
```

### Cadences

| Trigger | Action |
|---------|--------|
| fsnotify event (debounced 300ms) | `noteFileChange` → `refreshConflicts` → warn |
| `collab.interval` poll (default 10s) | `reconcileWatches`; if no fsnotify → `gitReconcile`; else `refreshConflicts` + warn |
| `collab.git_reconcile_interval` (default 2m) | `gitReconcile` → `refreshConflicts` + warn (fsnotify mode only) |

### Path normalization

- `watcher.worktreeFor(absPath)` — longest matching watched root.
- `filepath.Rel(worktree, absPath)` → forward-slash repo-relative path.
- Skip `.git/` paths and directories.

### Git reconcile

```go
git diff --name-only HEAD   // with GIT_OPTIONAL_LOCKS=1, 5s timeout
```

- Skip worktree when `index.lock` exists (`git rev-parse --git-path index.lock`).
- Replace `dirty[worktree]` with git output (authoritative).
- Prune dirty entries for departed worktrees.

### API

`Conflicts(ctx)` returns a copy of `cached` when `cacheValid`; otherwise runs a cheap in-memory `refreshConflicts` (no git).

## Config

```yaml
collab:
  enabled: true
  interval: 10s                  # watch reconcile + in-memory scan
  git_reconcile_interval: 2m     # git diff backstop (0 = default 2m)
  hint: true
```

## Files

| File | Change |
|------|--------|
| `internal/collab/monitor.go` | dirty tracking, cache, reconcile loop |
| `internal/collab/git.go` | read-only git helpers |
| `internal/collab/watcher.go` | `worktreeFor` |
| `internal/config/config.go` | `git_reconcile_interval` |
| `internal/daemon/server.go` | `SetCollabGitReconcileInterval`, `Run` args |
| `internal/cli/daemon.go` | wire config |
| `internal/daemon/reconfigure.go` | restart-only key |
| `internal/cli/config.go` | show new key |
| Tests | monitor, watcher, existing integration |
| Docs | FEATURES.md, README.md, env-vars |

## Test plan

- [ ] `Conflicts` uses injected `diff` when dirty empty and fsnotify off (existing tests).
- [ ] fsnotify `noteFileChange` populates dirty; conflict without git.
- [ ] `gitReconcile` replaces dirty from diff stub; prunes departed worktrees.
- [ ] `Conflicts` returns cache without calling `diff` twice.
- [ ] `gitIndexLocked` / `GIT_OPTIONAL_LOCKS` on git subprocess (unit test git.go).
- [ ] Existing `TestWatchLoopDebounces` / `TestRealWatcherTriggersScanOnEdit` updated for seed reconcile.

## Rollout

Patch release. No migration. Monorepo operators can tune `collab.git_reconcile_interval` up (e.g. `5m`) if git load is still noticeable.
