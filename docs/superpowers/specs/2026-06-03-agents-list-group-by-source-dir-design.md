# Group the agents list by source directory

**Date:** 2026-06-03
**Status:** Design (pending review)
**Scope:** In both the TUI agents list and the web agents list, group agents by
their source directory so it is obvious how many agents are working on the same
project. Purely presentational — no daemon, store, or config changes.

## Motivation

The fleet often has several agents running against the same project. Today both
lists are flat (TUI: `renderList`; web: a single `<table>` body), sorted by
`UpdatedAt` desc, so you cannot tell at a glance that, say, three of them are all
working in `~/workspace/personal/agentctl`. Grouping by the originating
directory makes that immediately visible.

This builds directly on the recent `feat(spawn): launch prompt agents in caller
cwd` change: prompt-spawned agents now record `Workdir` = the caller's cwd (the
"master shell" directory), and the old per-agent `~/agentctl-agents/<id>`
fallback is gone. So agents launched from the same project now share an
identical `Workdir`, which makes grouping meaningful.

## Source directory: the grouping key

`sourceDir(s)` = `s.Repo` if non-empty, else `s.Workdir`, else `"—"`.

This resolves to "the directory the `agentctl` command was triggered from" in
both spawn modes:

- **Prompt mode** (the common case): `Repo` is empty, `Workdir` = the caller's
  cwd → group by the launch dir.
- **Typed / worktree mode**: `Repo` = the repo root (defaults to the spawn cwd);
  `Workdir` may be a per-agent worktree under `.worktrees/`. Using `Repo` keeps
  all worktree agents for one repo in the same group, rather than splitting them
  by worktree path.

A prompt agent and a typed agent rooted at the same physical path collapse into
one group naturally, because `Repo || Workdir` yields the same string for both.

## Ordering

- **Groups** are ordered by their most-recently-active agent (max `UpdatedAt`,
  desc), so the project you are actively working in floats to the top.
- **Within a group**, agents keep the existing `UpdatedAt`-desc order.

Ordering is applied in each UI's presentation layer. The daemon `/sessions`
endpoint stays `UpdatedAt`-desc, so the CLI `list`, MCP, and any other consumer
are untouched.

## Display

Each group is preceded by a header showing the directory and the agent count.
The **TUI** abbreviates a leading `$HOME` to `~` (it runs locally and can read
`os.UserHomeDir`); the **web** UI shows the absolute path as-is (the browser has
no reliable `$HOME`). Example (TUI):

```
~/workspace/personal/agentctl (3)
  agent-4f98  …
  agent-c860  …
  agent-d01c  …
~/workspace/personal/other-repo (1)
  agent-a86c  …
```

## TUI (`internal/tui`)

### New helpers (`list.go`)
- `sourceDir(s *store.Session) string` — the grouping key above.
- `abbrevHome(path string) string` — replace a leading `$HOME` with `~` for the
  header label (pure; reads `os.UserHomeDir` once).
- `groupSort(sessions []*store.Session) []*store.Session` — stable re-order into
  grouped order: compute each group's max `UpdatedAt`, sort groups desc by that,
  and within each group keep input order (input is already `UpdatedAt`-desc from
  the daemon). Returns a new slice; pure and unit-testable.

### Model (`model.go`)
- In the `sessionsMsg` handler, run the incoming sessions through `groupSort`
  before assigning to `m.sessions` (and before `repin`). The cursor continues to
  index directly into `m.sessions`, so it must reflect grouped order. Because
  `repin` re-pins by id after assignment, selection survives re-sorting.

### Rendering (`renderList`)
Rework `renderList` to interleave non-selectable group-header rows with agent
rows:

1. Build an ordered `[]listRow`, where each row is either a header (carrying the
   group label) or an agent (carrying its index into `m.sessions`). Walking the
   already-grouped `m.sessions`, emit a header whenever `sourceDir` changes.
2. Locate the row holding the agent at `m.cursor`, and window over the rows with
   the existing `listWindow` logic so the selected agent stays visible.
3. **Sticky header:** if the first visible row is an agent (its header scrolled
   off the top of the window), prepend the current group's header (dimmed) so the
   group label is never lost.
4. Render header rows via `stMuted` (bold-muted), agent rows exactly as today
   (badge, age, subject, cursor `›` highlight). Indent agent rows by two spaces
   under their header.
5. The `▲/▼ N more` overflow indicators continue to count hidden **agents**, not
   header rows.

Navigation keys (`j/k/↑/↓`) are unchanged — they move `m.cursor` over agents
only, so headers are skipped for free. The list title stays `Agents (N)` where N
is the agent count.

This change lives entirely in `renderList` + the new helpers + the `sessionsMsg`
sort. It is forward-compatible with the approved master-pane cockpit re-arch,
which reuses `renderList` in its `--pane=list` `ListModel`.

## Web (`web/src/components/AgentList.tsx`)

- Compute groups from the `sessions` prop in render: key = `repo || workdir ||
  '—'`, group order by max `updated_at` desc, agents within a group in input
  order.
- Replace the single `<tbody>` with one `<tbody>` per group. Each group starts
  with a header row: a `<tr class="group">` containing a single `<td
  colSpan={6}>` showing `{dir} ({count})`, where `dir` is the absolute path as
  reported by the daemon (no home abbreviation — the browser has no `$HOME`).
- Selection stays id-based via `onSelect`, so `Dashboard.tsx` is unchanged.
- Add minimal styling for the `.group` header row (muted, slightly emphasized);
  reuse existing list CSS conventions.

## Error handling / edge cases

- **Empty `sourceDir`** (no `Repo` and no `Workdir`) → grouped under a literal
  `"—"` header. Should be rare now that prompt mode requires a cwd.
- **Single group** → one header over the whole list; behavior degrades to "flat
  list with one label", which is fine.
- **Empty list** → unchanged empty-state messages in both UIs.
- **Cursor clamping** on shrink → unchanged (`repin` handles it).

## Testing (TDD)

Go (`internal/tui`):
- `sourceDir`: Repo-set, Workdir-only, both-empty cases.
- `abbrevHome`: home-prefixed path, non-home path, exact-home path.
- `groupSort`: groups ordered by most-recent activity; within-group order
  preserved; multiple agents per group; single group.
- `renderList` (`list_test.go`): a header line appears per group; agent rows
  render under the right header; cursor highlight lands on an agent (never a
  header); sticky header appears when the top group's header is scrolled off;
  `▲/▼ N more` still counts agents.
- `model_test.go`: a `sessionsMsg` produces grouped `m.sessions` and `repin`
  keeps the selected id.

Web (`AgentList` vitest):
- Renders one group header per distinct source dir with the correct count.
- Agent rows appear under their group; group order is most-recent-first.
- Clicking an agent still calls `onSelect` with its id.

## Out of scope (YAGNI)

No grouping toggle, no collapse/expand, no alphabetical-vs-activity sort option,
no persisted sort preference, no server-side grouping. The daemon's
`UpdatedAt`-desc ordering and all non-UI consumers are untouched.
