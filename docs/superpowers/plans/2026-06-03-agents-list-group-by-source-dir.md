# Group Agents List by Source Directory — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Group the agents list — in both the TUI and the web UI — by source directory (`Repo`, else `Workdir`), so it is obvious how many agents share a project.

**Architecture:** Purely presentational. The TUI re-sorts `m.sessions` into grouped order on each `sessionsMsg` and `renderList` interleaves dimmed (sticky) group-header rows; the cursor keeps indexing `m.sessions` so navigation is unchanged. The web UI groups the `sessions` prop via a small pure `lib/group.ts` (unit-tested) and renders one `<tbody>` per group. No daemon, store, or config changes.

**Tech Stack:** Go (bubbletea/lipgloss TUI), React 19 + Astro (web), `testify` (Go tests), `vitest` (web tests).

**Spec:** `docs/superpowers/specs/2026-06-03-agents-list-group-by-source-dir-design.md`

> **Implementation note (as shipped):** this plan was written against the pre-cockpit TUI. During execution, master merged the tmux-composited cockpit re-architecture, so the branch was rebuilt on it and the TUI tasks were adapted: `renderList` is now a **free function** `renderList(sessions, cursor, width, height)` (not a `Model` method), `buildRows` takes `sessions`, and `groupSort` is wired into **both** session-storage sites — the classic `model.go` *and* the cockpit `list_pane.go` `sessionsMsg` handlers. The grouping logic, ordering, sticky-header windowing, and web changes are otherwise as described below.

---

## File Structure

- `internal/tui/list.go` — **modify.** Add `sourceDir`, `abbrevHome`, `groupSort`, `listRow`, `buildRows`, `headerAbove`, `cursorRowIndex`; rework `renderList`. New imports: `os`, `sort`.
- `internal/tui/list_test.go` — **modify.** Add tests for `sourceDir`, `abbrevHome`, `groupSort`, and grouped `renderList`; existing tests must keep passing.
- `internal/tui/model.go` — **modify** (1 line): wrap incoming sessions with `groupSort` in the `sessionsMsg` case.
- `internal/tui/model_test.go` — **modify.** Add a test that `sessionsMsg` produces grouped order and `repin` keeps the selected id.
- `web/src/lib/group.ts` — **create.** Pure `sourceDir` + `groupSessions`.
- `web/src/lib/group.test.ts` — **create.** Vitest unit tests for the above.
- `web/src/components/AgentList.tsx` — **modify.** Render one `<tbody>` per group with a header row.
- `web/src/styles/app.css` — **modify.** Add `.list tr.group` styling.

---

## Task 1: `sourceDir` and `abbrevHome` helpers (Go)

**Files:**
- Modify: `internal/tui/list.go`
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/list_test.go`:

```go
func TestSourceDir(t *testing.T) {
	require.Equal(t, "/repo/root", sourceDir(&store.Session{Repo: "/repo/root", Workdir: "/repo/root/.worktrees/x"}), "Repo wins when set")
	require.Equal(t, "/work/proj", sourceDir(&store.Session{Workdir: "/work/proj"}), "falls back to Workdir")
	require.Equal(t, "—", sourceDir(&store.Session{}), "dash when both empty")
}

func TestAbbrevHome(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.Equal(t, "~", abbrevHome(home), "exact home → ~")
	require.Equal(t, "~/work/proj", abbrevHome(filepath.Join(home, "work", "proj")), "home prefix → ~")
	require.Equal(t, "/etc/thing", abbrevHome("/etc/thing"), "non-home path unchanged")
}
```

Add `"os"` and `"path/filepath"` to the `list_test.go` import block (alongside the existing `fmt`, `strings`, `testing`, `time`, `store`, `require`).

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run 'TestSourceDir|TestAbbrevHome' -v`
Expected: FAIL — `undefined: sourceDir`, `undefined: abbrevHome`.

- [ ] **Step 3: Implement the helpers**

In `internal/tui/list.go`, add `"os"` to the import block so it reads (note: `"sort"` is added later, in Task 2, when `groupSort` first uses it — adding it now would fail the build as an unused import):

```go
import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)
```

Then add these helpers (place them near the top, after the imports):

```go
// sourceDir is the grouping key: the directory the agentctl command was
// triggered from. Repo (set for typed/worktree agents) wins; otherwise Workdir
// (the caller cwd for prompt agents); "—" when neither is known.
func sourceDir(s *store.Session) string {
	if s.Repo != "" {
		return s.Repo
	}
	if s.Workdir != "" {
		return s.Workdir
	}
	return "—"
}

// abbrevHome replaces a leading $HOME with ~ for display (TUI runs locally).
func abbrevHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run 'TestSourceDir|TestAbbrevHome' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): sourceDir + abbrevHome helpers for list grouping

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: `groupSort` — re-order sessions into grouped order (Go)

**Files:**
- Modify: `internal/tui/list.go`
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/list_test.go`:

```go
func TestGroupSortOrdersGroupsByRecencyAndKeepsWithinOrder(t *testing.T) {
	now := time.Now()
	// Daemon order is UpdatedAt desc. Two groups: /b (most recent) and /a.
	in := []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
		{ID: "a2", Workdir: "/a", UpdatedAt: now.Add(-4 * time.Minute)},
	}
	out := groupSort(in)
	got := []string{out[0].ID, out[1].ID, out[2].ID, out[3].ID}
	// /b group first (its newest -1m beats /a's newest -2m); within each group,
	// the input (UpdatedAt-desc) order is preserved.
	require.Equal(t, []string{"b1", "b2", "a1", "a2"}, got)
}

func TestGroupSortStableForSingleOrEmpty(t *testing.T) {
	require.Empty(t, groupSort(nil))
	one := []*store.Session{{ID: "x", Workdir: "/a"}}
	require.Equal(t, one, groupSort(one))
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run TestGroupSort -v`
Expected: FAIL — `undefined: groupSort`.

- [ ] **Step 3: Implement `groupSort`**

First add `"sort"` to the `internal/tui/list.go` import block (it is now used), so it reads:

```go
import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)
```

Then add to `internal/tui/list.go`:

```go
// groupSort returns sessions re-ordered so agents sharing a sourceDir are
// contiguous. Groups are ordered by their most-recently-active agent (desc);
// within a group the input order is preserved (the daemon already sorts
// UpdatedAt-desc). Pure: returns a new slice, leaves the input untouched.
func groupSort(sessions []*store.Session) []*store.Session {
	if len(sessions) < 2 {
		return sessions
	}
	type grp struct {
		max  time.Time
		seen int // first-seen index — stable tiebreak
		rank int
	}
	groups := map[string]*grp{}
	var keys []string // first-seen order
	for i, s := range sessions {
		k := sourceDir(s)
		g := groups[k]
		if g == nil {
			groups[k] = &grp{max: s.UpdatedAt, seen: i}
			keys = append(keys, k)
			continue
		}
		if s.UpdatedAt.After(g.max) {
			g.max = s.UpdatedAt
		}
	}
	sort.SliceStable(keys, func(a, b int) bool {
		ga, gb := groups[keys[a]], groups[keys[b]]
		if ga.max.Equal(gb.max) {
			return ga.seen < gb.seen
		}
		return ga.max.After(gb.max)
	})
	for r, k := range keys {
		groups[k].rank = r
	}
	out := make([]*store.Session, len(sessions))
	copy(out, sessions)
	sort.SliceStable(out, func(a, b int) bool {
		return groups[sourceDir(out[a])].rank < groups[sourceDir(out[b])].rank
	})
	return out
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run TestGroupSort -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): groupSort — order sessions into contiguous source-dir groups

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: Rework `renderList` to render group headers (Go)

**Files:**
- Modify: `internal/tui/list.go` (replace the `renderList` function; add `listRow`, `buildRows`, `headerAbove`, `cursorRowIndex`)
- Test: `internal/tui/list_test.go`

The current `renderList` windows directly over `m.sessions`. The new version builds a row stream (headers + agents), windows over rows keeping the selected agent visible, and shows a dimmed sticky header when the window starts mid-group. The four existing `renderList` tests must keep passing — the windowing below is designed to preserve "exactly height lines", "cursor always visible", and "more hint when hidden".

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/list_test.go`:

```go
func TestRenderListGroupsBySourceDir(t *testing.T) {
	m := New(&fakeAPI{})
	m.sessions = groupSort([]*store.Session{
		{ID: "a1", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: time.Now()},
		{ID: "a2", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: time.Now().Add(-time.Minute)},
		{ID: "b1", Workdir: "/work/beta", Status: store.StatusWorking, UpdatedAt: time.Now().Add(-2 * time.Minute)},
	})
	out := m.renderList(120, 12)
	require.Contains(t, out, "/work/alpha (2)", "alpha group header with count")
	require.Contains(t, out, "/work/beta (1)", "beta group header with count")
	require.Contains(t, out, "a1")
	require.Contains(t, out, "b1")
	// The header line must not carry the cursor caret.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "/work/alpha (2)") {
			require.NotContains(t, line, "›", "group header is never the cursor row")
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run TestRenderListGroupsBySourceDir -v`
Expected: FAIL — the header strings `/work/alpha (2)` are not yet rendered.

- [ ] **Step 3: Replace `renderList` and add the row helpers**

In `internal/tui/list.go`, replace the entire existing `renderList` function with the following, and add the three helpers and the `listRow` type below it:

```go
// listRow is one rendered line in the list: a group header (header != "") or an
// agent (header == "", idx points into m.sessions). buildRows assumes m.sessions
// is already in grouped order (groupSort), so groups are contiguous.
type listRow struct {
	header string
	idx    int
}

func (m Model) buildRows() []listRow {
	var rows []listRow
	prev := ""
	for i := range m.sessions {
		dir := sourceDir(m.sessions[i])
		if i == 0 || dir != prev {
			// Count the contiguous run sharing this dir for the header.
			count := 0
			for j := i; j < len(m.sessions) && sourceDir(m.sessions[j]) == dir; j++ {
				count++
			}
			rows = append(rows, listRow{header: fmt.Sprintf("%s (%d)", abbrevHome(dir), count)})
			prev = dir
		}
		rows = append(rows, listRow{idx: i})
	}
	return rows
}

func cursorRowIndex(rows []listRow, cursor int) int {
	for i, r := range rows {
		if r.header == "" && r.idx == cursor {
			return i
		}
	}
	return 0
}

func headerAbove(rows []listRow, at int) string {
	for i := at; i >= 0; i-- {
		if rows[i].header != "" {
			return rows[i].header
		}
	}
	return ""
}

func countAgents(rows []listRow) int {
	n := 0
	for _, r := range rows {
		if r.header == "" {
			n++
		}
	}
	return n
}

// renderList renders the agent list windowed to exactly `height` lines and
// `width` columns of inner content. Agents are grouped by source directory with
// dimmed header rows; the selected agent is always kept visible, and a sticky
// header is shown when the window starts in the middle of a group.
func (m Model) renderList(width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(m.sessions) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	rows := m.buildRows()
	n := len(rows)
	cursorRow := cursorRowIndex(rows, m.cursor)

	visible := height
	hidden := n > height
	if hidden {
		if visible = height - 1; visible < 1 {
			visible = 1
		}
	}

	// Window over rows; if the window starts mid-group, reserve a line for the
	// sticky header so the selected agent stays visible.
	top := listWindow(n, cursorRow, visible)
	sticky := rows[top].header == "" && height > 1 && headerAbove(rows, top) != ""
	bodyCap := visible
	if sticky {
		top = listWindow(n, cursorRow, visible-1)
		sticky = rows[top].header == "" // re-check after the shift
		if sticky {
			bodyCap = visible - 1
		}
	}

	var b strings.Builder
	used := 0
	if sticky {
		b.WriteString("  " + stMuted.Render(headerAbove(rows, top)) + "\n")
		used++
	}
	end := top
	for i := top; i < top+bodyCap && i < n; i++ {
		r := rows[i]
		if r.header != "" {
			b.WriteString("  " + stMuted.Render(r.header) + "\n")
		} else {
			s := m.sessions[r.idx]
			label, st := badge(s.Status)
			line := fmt.Sprintf("%-12s %-9s %-11s %-5s %s",
				trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label), age(s.UpdatedAt),
				trunc(s.Subject, max(0, width-44)))
			cur := "    "
			if r.idx == m.cursor {
				cur = "  " + stCursor.Render("› ")
				line = stCursor.Render(line)
			}
			b.WriteString(cur + line + "\n")
		}
		used++
		end = i + 1
	}

	if hidden && height > 1 {
		var parts []string
		if a := countAgents(rows[:top]); a > 0 {
			parts = append(parts, fmt.Sprintf("▲ %d more", a))
		}
		if a := countAgents(rows[end:]); a > 0 {
			parts = append(parts, fmt.Sprintf("▼ %d more", a))
		}
		b.WriteString(stMuted.Render(strings.Join(parts, "   ")) + "\n")
		used++
	}
	for ; used < height; used++ {
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}
```

Note: the old `renderList` body (the `for i := top; i < top+visible …` loop and its surrounding window/overflow/pad code) is fully replaced — do not leave the previous version behind. Keep `age`, `trunc`, `padTo`, `typeOr`, `max`, and `listWindow` exactly as they are.

- [ ] **Step 4: Run the new test AND the full existing list test suite**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run 'TestRenderList|TestListWindow' -v`
Expected: PASS for all — `TestRenderListGroupsBySourceDir`, `TestRenderListContainsAgeColumn`, `TestRenderListClampsToHeightAndKeepsCursor`, `TestRenderListShortListPadsToHeight`, `TestRenderListHeightOneRendersSingleLine`, `TestListWindow`.

- [ ] **Step 5: Commit**

```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): render group headers in the agents list

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: Wire `groupSort` into the model (Go)

**Files:**
- Modify: `internal/tui/model.go` (the `sessionsMsg` case in `Update`)
- Test: `internal/tui/model_test.go`

- [ ] **Step 1: Write the failing test**

First add `"time"` to the `internal/tui/model_test.go` import block — it is not currently imported. The block should read:

```go
import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)
```

Then add this test (it uses the existing `step(m, msg)` helper, which calls `Update` and returns the new `Model`):

```go
func TestSessionsMsgGroupsBySourceDir(t *testing.T) {
	now := time.Now()
	m := step(New(&fakeAPI{}), sessionsMsg{sessions: []*store.Session{
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-1 * time.Minute)},
		{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-2 * time.Minute)},
		{ID: "b2", Workdir: "/b", UpdatedAt: now.Add(-3 * time.Minute)},
	}})
	ids := []string{m.sessions[0].ID, m.sessions[1].ID, m.sessions[2].ID}
	require.Equal(t, []string{"b1", "b2", "a1"}, ids, "sessions stored in grouped order")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run TestSessionsMsgGroupsBySourceDir -v`
Expected: FAIL — sessions stored in raw `[b1, a1, b2]` order, not grouped.

- [ ] **Step 3: Wrap incoming sessions with `groupSort`**

In `internal/tui/model.go`, in the `case sessionsMsg:` branch, change the assignment line:

```go
		m.sessions = msg.sessions
```

to:

```go
		m.sessions = groupSort(msg.sessions)
```

Leave the surrounding lines (`prevID := m.selectedID()` before it and `m.repin(prevID)` after it) unchanged — `repin` re-pins selection by id after the re-sort.

- [ ] **Step 4: Run the test, plus the full model suite to confirm repin still works**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/ -run 'TestSessionsMsg|TestCursorMovesAndClamps|TestSpawnDoneSelectsNewAgent' -v`
Expected: PASS for all (including the existing `TestSessionsMsgRepinsByID`).

- [ ] **Step 5: Run the entire tui package and commit**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./internal/tui/`
Expected: `ok  github.com/srajanpathak/agentctl/internal/tui`

```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
git add internal/tui/model.go internal/tui/model_test.go
git commit -m "feat(tui): store sessions in grouped order on refresh

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: Web grouping logic — `lib/group.ts` (TypeScript)

**Files:**
- Create: `web/src/lib/group.ts`
- Test: `web/src/lib/group.test.ts`

- [ ] **Step 1: Write the failing test**

Create `web/src/lib/group.test.ts`:

```ts
import { describe, it, expect } from 'vitest';
import { groupSessions, sourceDir } from './group';
import type { Session } from './types';

function sess(p: Partial<Session>): Session {
  return {
    id: '', type: '', ticket: '', tmux_session: '', repo: '', worktree: '',
    branch: '', pr: '', prompt: '', workdir: '', subject: '', status: 'idle',
    pid: 0, created_at: '', updated_at: '', events: null, last_pane_excerpt: '',
    ...p,
  };
}

describe('sourceDir', () => {
  it('prefers repo, then workdir, then dash', () => {
    expect(sourceDir(sess({ repo: '/r', workdir: '/r/.worktrees/x' }))).toBe('/r');
    expect(sourceDir(sess({ workdir: '/w' }))).toBe('/w');
    expect(sourceDir(sess({}))).toBe('—');
  });
});

describe('groupSessions', () => {
  it('groups by source dir, orders groups by recency, preserves within-group order', () => {
    const out = groupSessions([
      sess({ id: 'b1', workdir: '/b', updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'a1', workdir: '/a', updated_at: '2026-06-03T09:00:00Z' }),
      sess({ id: 'b2', workdir: '/b', updated_at: '2026-06-03T08:00:00Z' }),
      sess({ id: 'a2', workdir: '/a', updated_at: '2026-06-03T07:00:00Z' }),
    ]);
    expect(out.map((g) => g.dir)).toEqual(['/b', '/a']);
    expect(out[0].sessions.map((s) => s.id)).toEqual(['b1', 'b2']);
    expect(out[1].sessions.map((s) => s.id)).toEqual(['a1', 'a2']);
  });

  it('returns one group when all share a dir', () => {
    const out = groupSessions([
      sess({ id: 'x', workdir: '/w', updated_at: '2026-06-03T10:00:00Z' }),
      sess({ id: 'y', workdir: '/w', updated_at: '2026-06-03T09:00:00Z' }),
    ]);
    expect(out).toHaveLength(1);
    expect(out[0]).toEqual({ dir: '/w', sessions: out[0].sessions });
    expect(out[0].sessions).toHaveLength(2);
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl/web && npx vitest run src/lib/group.test.ts`
Expected: FAIL — cannot resolve `./group` / `groupSessions` is undefined.

- [ ] **Step 3: Implement `lib/group.ts`**

Create `web/src/lib/group.ts`:

```ts
import type { Session } from './types';

export interface SessionGroup {
  dir: string;
  sessions: Session[];
}

// sourceDir is the grouping key: the directory the agentctl command was
// triggered from. repo (typed/worktree agents) wins; otherwise workdir (prompt
// agents' caller cwd); '—' when neither is known.
export function sourceDir(s: Session): string {
  return s.repo || s.workdir || '—';
}

// groupSessions buckets sessions by sourceDir and orders the groups by their
// most-recently-updated agent (desc). Within each group the input order is
// preserved (the daemon already returns updated_at-desc). Array sort is stable
// (ES2019+), so equal-recency groups keep first-seen order.
export function groupSessions(sessions: Session[]): SessionGroup[] {
  const groups = new Map<string, Session[]>();
  for (const s of sessions) {
    const k = sourceDir(s);
    const arr = groups.get(k);
    if (arr) arr.push(s);
    else groups.set(k, [s]);
  }
  const maxTs = (ss: Session[]) =>
    ss.reduce((m, s) => Math.max(m, new Date(s.updated_at).getTime() || 0), 0);
  return [...groups.entries()]
    .map(([dir, ss]) => ({ dir, sessions: ss }))
    .sort((a, b) => maxTs(b.sessions) - maxTs(a.sessions));
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl/web && npx vitest run src/lib/group.test.ts`
Expected: PASS (all cases).

- [ ] **Step 5: Commit**

```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
git add web/src/lib/group.ts web/src/lib/group.test.ts
git commit -m "feat(web): groupSessions helper for source-dir grouping

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Render groups in `AgentList.tsx` + styling (React/CSS)

**Files:**
- Modify: `web/src/components/AgentList.tsx`
- Modify: `web/src/styles/app.css`

There is no `@testing-library/react` in the project (and no component test harness), so this task is verified by the grouping unit tests from Task 5 plus a production build. The component change is a thin, mechanical wiring of `groupSessions`.

- [ ] **Step 1: Update `AgentList.tsx` to render per-group `<tbody>`**

Replace the entire contents of `web/src/components/AgentList.tsx` with:

```tsx
import type { Session } from '../lib/types';
import { groupSessions } from '../lib/group';
import BusyIdleBadge from './BusyIdleBadge';

function age(iso: string): string {
  if (!iso) return '—';
  const ms = Date.now() - new Date(iso).getTime();
  const m = Math.floor(ms / 60000);
  if (m < 1) return '<1m';
  if (m < 60) return `${m}m`;
  return `${Math.floor(m / 60)}h${m % 60}m`;
}

export default function AgentList({ sessions, selectedId, onSelect }: {
  sessions: Session[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  if (sessions.length === 0) {
    return <div className="list empty">No agents yet. Click "+ New agent".</div>;
  }
  const groups = groupSessions(sessions);
  return (
    <div className="list">
      <table>
        <thead>
          <tr><th>ID</th><th>Type</th><th>State</th><th>Status</th><th>Age</th><th>Subject</th></tr>
        </thead>
        {groups.map((g) => (
          <tbody key={g.dir}>
            <tr className="group">
              <td colSpan={6}>{g.dir} ({g.sessions.length})</td>
            </tr>
            {g.sessions.map((s) => (
              <tr key={s.id} className={s.id === selectedId ? 'sel' : ''} onClick={() => onSelect(s.id)}>
                <td>{s.id}</td>
                <td>{s.type || <span className="muted">classifying…</span>}</td>
                <td><BusyIdleBadge status={s.status} /></td>
                <td>{s.status}</td>
                <td>{age(s.updated_at)}</td>
                <td className="muted">{s.subject}</td>
              </tr>
            ))}
          </tbody>
        ))}
      </table>
    </div>
  );
}
```

- [ ] **Step 2: Add the group-header style**

In `web/src/styles/app.css`, immediately after the line `.list tbody tr.sel { background: #2f81f733; }`, add:

```css
.list tr.group td { font-weight: 600; color: var(--idle); background: #8881; cursor: default; }
.list tr.group:hover td { background: #8881; }
```

- [ ] **Step 3: Verify the web build and the full web test suite pass**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl/web && npm run build && npm test`
Expected: Astro build succeeds with no type errors; `vitest run` reports all test files passing (including `group.test.ts`, `status.test.ts`, `api.test.ts`).

- [ ] **Step 4: Commit**

```bash
cd /Users/srajan.pathak/workspace/personal/agentctl
git add web/src/components/AgentList.tsx web/src/styles/app.css
git commit -m "feat(web): group the agents table by source directory

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: Full verification

**Files:** none (verification only).

- [ ] **Step 1: Run the full Go test suite**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go test ./...`
Expected: every package reports `ok` (no `FAIL`).

- [ ] **Step 2: Run `go vet`**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl && go vet ./internal/tui/`
Expected: no output (clean).

- [ ] **Step 3: Run the full web test suite + build**

Run: `cd /Users/srajan.pathak/workspace/personal/agentctl/web && npm test && npm run build`
Expected: vitest all-pass; build succeeds.

- [ ] **Step 4 (manual, optional): eyeball the TUI**

If a daemon with several agents across ≥2 directories is available, run `agentctl tui` and confirm: agents appear under per-directory headers, group with the most-recent activity is on top, cursor `j/k` moves over agents only (skips headers), and scrolling a long list keeps a sticky header visible.

---

## Notes for the implementer

- **TDD order matters:** within each task, write the test, watch it fail, then implement. Do not implement ahead of the test.
- **Do not touch** `internal/daemon`, `internal/store`, `internal/config`, or the CLI/MCP list paths — grouping is presentational only; the daemon stays `UpdatedAt`-desc.
- **The windowing in Task 3 is load-bearing.** If an existing `renderList` test breaks, the regression is in the sticky-header capacity math (`bodyCap`/`top` recompute), not in the test — re-read the algorithm rather than editing the assertions.
- All commits should land on the current branch (`tui-master-pane`); do not create a new branch or push unless asked.
