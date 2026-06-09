# TUI open-dir (`o`) + dir-aware new-agent (`n`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the cockpit and classic TUIs open a directory as a (possibly empty) group with `o`, and default a new agent's launch dir (`n`) to the group the cursor is in, overridable inline.

**Architecture:** A new pure layer in `internal/tui/list.go` turns `(sessions, openedDirs)` into a flat list of navigable `item`s (agents + one placeholder per empty opened dir); the cursor indexes items. Path entry uses a `/fs/dirs`-backed text input with tab-completion. The two existing models (`Model` classic, `listPaneModel` cockpit) get parallel, thin wiring around the shared pure functions (Option 1 — no shared `listCore`).

**Tech Stack:** Go, Bubble Tea (`charmbracelet/bubbletea`, `bubbles/textinput`, `bubbles/textarea`), testify.

**Spec:** `docs/superpowers/specs/2026-06-03-agentctl-tui-open-dir-and-dir-aware-spawn-design.md`

---

## File Structure

- `internal/client/client.go` — add `DirListing`/`DirEntry` + `ListDirs` (mirrors `/fs/dirs`).
- `internal/tui/list.go` — pure core: `item`, `itemKey`, `dirKey`, `buildItems`, `itemAt`, `activeDir`, `expandPath`, `dirCompletionTarget`, `completeDir`, `longestCommonPrefix`; refactor `renderList`/`buildRows` to walk `[]item`.
- `internal/tui/cmds.go` — `dirListMsg`, `openDirMsg`, `listDirsCmd`, `openDirCmd`; change `spawnCmd` to take a `cwd`.
- `internal/tui/model.go` — `api` interface gains `ListDirs`; new mode constants; classic `Model` fields + accessors + `repin`.
- `internal/tui/keys.go` — classic key wiring (`o`/`n`/`x`/`tab`, dir-input modes).
- `internal/tui/view.go` — classic footer/forms/help + `renderList` call site.
- `internal/tui/list_pane.go` — cockpit fields, accessors, `repin`, key wiring, forms/footer, `renderList` call site.
- Tests: `internal/client/client_test.go`, `internal/tui/list_test.go`, `internal/tui/model_test.go`, `internal/tui/list_pane_test.go`.

---

## Task 1: Client `ListDirs`

**Files:**
- Modify: `internal/client/client.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/client/client_test.go`:

```go
func TestListDirs(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/fs/dirs", r.URL.Path)
		require.Equal(t, "/home/me/work", r.URL.Query().Get("path"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"path":"/home/me/work","parent":"/home/me","entries":[{"name":"api","path":"/home/me/work/api"}]}`))
	}))
	defer ts.Close()

	l, err := New(ts.URL).ListDirs(t.Context(), "/home/me/work")
	require.NoError(t, err)
	require.Equal(t, "/home/me/work", l.Path)
	require.Equal(t, "/home/me", l.Parent)
	require.Len(t, l.Entries, 1)
	require.Equal(t, "api", l.Entries[0].Name)
	require.Equal(t, "/home/me/work/api", l.Entries[0].Path)
}
```

If `net/http`/`httptest`/`require` are not already imported in this test file, add them (the existing `Spawn` test already uses `httptest`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestListDirs -v`
Expected: FAIL — `New(...).ListDirs` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/client/client.go`, add `net/url` to the import block, and add:

```go
// DirEntry is one subdirectory in a DirListing.
type DirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// DirListing mirrors the daemon's GET /fs/dirs response: a directory, its
// parent (empty at the filesystem root), and its immediate subdirectories.
type DirListing struct {
	Path    string     `json:"path"`
	Parent  string     `json:"parent"`
	Entries []DirEntry `json:"entries"`
}

// ListDirs lists the immediate subdirectories of path (empty path = the
// daemon's default, the user's home directory).
func (c *Client) ListDirs(ctx context.Context, path string) (DirListing, error) {
	p := "/fs/dirs"
	if path != "" {
		p += "?path=" + url.QueryEscape(path)
	}
	var l DirListing
	if err := c.do(ctx, http.MethodGet, p, nil, &l); err != nil {
		return DirListing{}, err
	}
	return l, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/client/ -run TestListDirs -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat(client): add ListDirs for GET /fs/dirs"
```

---

## Task 2: `api` interface + test fake gain `ListDirs`

This compiles the new dependency into the TUI before any wiring uses it.

**Files:**
- Modify: `internal/tui/model.go:16-23`
- Modify: `internal/tui/model_test.go:17-49`

- [ ] **Step 1: Extend the `api` interface**

In `internal/tui/model.go`, add to the `api` interface:

```go
	ListDirs(ctx context.Context, path string) (client.DirListing, error)
```

(The `client` package is already imported in `model.go`.)

- [ ] **Step 2: Extend the test fake**

In `internal/tui/model_test.go`, add fields to `fakeAPI`:

```go
	listedDir  string
	dirListing client.DirListing
	dirListErr error
```

and the method:

```go
func (f *fakeAPI) ListDirs(_ context.Context, path string) (client.DirListing, error) {
	f.listedDir = path
	return f.dirListing, f.dirListErr
}
```

- [ ] **Step 3: Run to verify it compiles**

Run: `go test ./internal/tui/ -run TestSourceDir -v`
Expected: PASS (package compiles with the new interface method satisfied by the fake).

- [ ] **Step 4: Commit**

```bash
git add internal/tui/model.go internal/tui/model_test.go
git commit -m "feat(tui): add ListDirs to api interface + fake"
```

---

## Task 3: `item` model + `buildItems`

**Files:**
- Modify: `internal/tui/list.go`
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/list_test.go`:

```go
func TestBuildItemsGroupsAgentsNoOpenedDirs(t *testing.T) {
	now := time.Now()
	ss := groupSort([]*store.Session{
		{ID: "a1", Workdir: "/a", UpdatedAt: now},
		{ID: "a2", Workdir: "/a", UpdatedAt: now.Add(-time.Minute)},
		{ID: "b1", Workdir: "/b", UpdatedAt: now.Add(-2 * time.Minute)},
	})
	items := buildItems(ss, nil)
	require.Len(t, items, 3)
	require.Equal(t, "a1", items[0].session.ID)
	require.Equal(t, "a2", items[1].session.ID)
	require.Equal(t, "b1", items[2].session.ID)
	require.Equal(t, "/a", items[0].dir)
}

func TestBuildItemsEmptyOpenedDirGetsPlaceholderOnTop(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{{ID: "a1", Workdir: "/a", UpdatedAt: now.Add(-time.Hour)}}
	opened := map[string]time.Time{"/freshly/opened": now} // newer than the agent
	items := buildItems(ss, opened)
	require.Len(t, items, 2, "one placeholder + one agent")
	require.Nil(t, items[0].session, "placeholder is first (most recent)")
	require.Equal(t, "/freshly/opened", items[0].dir)
	require.Equal(t, "a1", items[1].session.ID)
}

func TestBuildItemsOpenedDirWithAgentsHasNoPlaceholder(t *testing.T) {
	now := time.Now()
	ss := []*store.Session{{ID: "a1", Workdir: "/a", UpdatedAt: now}}
	opened := map[string]time.Time{"/a": now.Add(-time.Hour)} // /a already has an agent
	items := buildItems(ss, opened)
	require.Len(t, items, 1, "no placeholder for an opened dir that has agents")
	require.Equal(t, "a1", items[0].session.ID)
}

func TestItemKeyDistinguishesAgentsFromPlaceholders(t *testing.T) {
	require.Equal(t, "agent-x", itemKey(item{session: &store.Session{ID: "agent-x"}, dir: "/a"}))
	require.Equal(t, dirKey("/a"), itemKey(item{dir: "/a"}))
	require.NotEqual(t, itemKey(item{dir: "/a"}), itemKey(item{session: &store.Session{ID: "/a"}, dir: "/a"}))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run 'TestBuildItems|TestItemKey' -v`
Expected: FAIL — `buildItems`, `item`, `itemKey`, `dirKey` undefined.

- [ ] **Step 3: Implement**

Add to `internal/tui/list.go`:

```go
// item is one navigable row: a real agent (session != nil) or a placeholder for
// an opened directory that currently has no agents (session == nil). dir is the
// group directory and is always set.
type item struct {
	session *store.Session
	dir     string
}

// dirKey is the placeholder identity for an opened dir. The NUL separator can't
// occur in a session ID, so a placeholder key never collides with an agent's.
func dirKey(dir string) string { return "dir\x00" + dir }

// itemKey is the stable identity used to re-pin the cursor across refreshes.
func itemKey(it item) string {
	if it.session != nil {
		return it.session.ID
	}
	return dirKey(it.dir)
}

// buildItems flattens grouped sessions plus opened directories into the list the
// cursor walks. Groups are ordered by most-recent activity (an agent group's key
// is its newest UpdatedAt; an empty opened dir's key is when it was opened — so a
// freshly-opened dir floats to the top). An opened dir that has agents emits its
// agents and no placeholder; an opened dir with none emits a single placeholder.
// Pure: returns a new slice, leaves inputs untouched.
func buildItems(sessions []*store.Session, opened map[string]time.Time) []item {
	type grp struct {
		max  time.Time
		seen int
	}
	groups := map[string]*grp{}
	var order []string
	note := func(dir string, t time.Time) {
		g := groups[dir]
		if g == nil {
			groups[dir] = &grp{max: t, seen: len(order)}
			order = append(order, dir)
			return
		}
		if t.After(g.max) {
			g.max = t
		}
	}
	for _, s := range sessions {
		note(sourceDir(s), s.UpdatedAt)
	}
	for dir, at := range opened {
		note(dir, at)
	}
	sort.SliceStable(order, func(a, b int) bool {
		ga, gb := groups[order[a]], groups[order[b]]
		if ga.max.Equal(gb.max) {
			return ga.seen < gb.seen
		}
		return ga.max.After(gb.max)
	})
	hasAgents := map[string]bool{}
	byDir := map[string][]*store.Session{}
	for _, s := range sessions {
		d := sourceDir(s)
		hasAgents[d] = true
		byDir[d] = append(byDir[d], s)
	}
	var items []item
	for _, dir := range order {
		if hasAgents[dir] {
			for _, s := range byDir[dir] {
				items = append(items, item{session: s, dir: dir})
			}
			continue
		}
		items = append(items, item{dir: dir}) // empty opened dir → placeholder
	}
	return items
}
```

Note: `sort` and `time` are already imported in `list.go`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tui/ -run 'TestBuildItems|TestItemKey' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): item model + buildItems (agents + opened-dir placeholders)"
```

---

## Task 4: `itemAt` + `activeDir`

**Files:**
- Modify: `internal/tui/list.go`
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/list_test.go`:

```go
func TestItemAtClampsOutOfRange(t *testing.T) {
	items := []item{{dir: "/a"}, {session: &store.Session{ID: "x"}, dir: "/a"}}
	require.Equal(t, "/a", itemAt(items, -1).dir, "negative clamps to first")
	require.Equal(t, "x", itemAt(items, 99).session.ID, "past end clamps to last")
	require.Equal(t, item{}, itemAt(nil, 0), "empty list → zero item")
}

func TestActiveDirUsesCursorItemElseFallback(t *testing.T) {
	items := []item{{session: &store.Session{ID: "x"}, dir: "/work/api"}, {dir: "—"}}
	require.Equal(t, "/work/api", activeDir(items, 0, "/fallback"))
	require.Equal(t, "/fallback", activeDir(items, 1, "/fallback"), "unknown dir (—) → fallback")
	require.Equal(t, "/fallback", activeDir(nil, 0, "/fallback"), "no items → fallback")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run 'TestItemAt|TestActiveDir' -v`
Expected: FAIL — `itemAt`, `activeDir` undefined.

- [ ] **Step 3: Implement**

Add to `internal/tui/list.go`:

```go
// itemAt returns the item at cursor, clamped to the slice bounds. Returns a zero
// item (nil session, "" dir) when items is empty.
func itemAt(items []item, cursor int) item {
	if len(items) == 0 {
		return item{}
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(items) {
		cursor = len(items) - 1
	}
	return items[cursor]
}

// activeDir is the directory a new agent should launch in: the cursor item's
// group dir, or fallback when there is no item or the group dir is unknown ("—").
func activeDir(items []item, cursor int, fallback string) string {
	d := itemAt(items, cursor).dir
	if d == "" || d == "—" {
		return fallback
	}
	return d
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tui/ -run 'TestItemAt|TestActiveDir' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): itemAt + activeDir (cursor dir with fallback)"
```

---

## Task 5: Path completion helpers

**Files:**
- Modify: `internal/tui/list.go`
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/list_test.go`:

```go
func TestExpandPath(t *testing.T) {
	require.Equal(t, "/home/me", expandPath("~", "/home/me"))
	require.Equal(t, "/home/me/work", expandPath("~/work", "/home/me"))
	require.Equal(t, "/home/me/work", expandPath("~/work/", "/home/me"), "clean strips trailing slash")
	require.Equal(t, "/abs/path", expandPath("/abs/path", "/home/me"), "absolute unchanged")
}

func TestDirCompletionTarget(t *testing.T) {
	d, leaf := dirCompletionTarget("/home/me/wo")
	require.Equal(t, "/home/me", d)
	require.Equal(t, "wo", leaf)

	d, leaf = dirCompletionTarget("/home/me/work/")
	require.Equal(t, "/home/me/work", d)
	require.Equal(t, "", leaf, "trailing slash → list children")

	d, leaf = dirCompletionTarget("/")
	require.Equal(t, "/", d)
	require.Equal(t, "", leaf)
}

func TestLongestCommonPrefix(t *testing.T) {
	require.Equal(t, "ap", longestCommonPrefix([]string{"api", "apex"}))
	require.Equal(t, "only", longestCommonPrefix([]string{"only"}))
	require.Equal(t, "", longestCommonPrefix([]string{"api", "web"}))
	require.Equal(t, "", longestCommonPrefix(nil))
}

func TestCompleteDir(t *testing.T) {
	listing := client.DirListing{
		Path: "/home/me/work",
		Entries: []client.DirEntry{
			{Name: "api", Path: "/home/me/work/api"},
			{Name: "apex", Path: "/home/me/work/apex"},
			{Name: "web", Path: "/home/me/work/web"},
		},
	}
	completed, cands := completeDir(listing, "/home/me/work/ap")
	require.Equal(t, "/home/me/work/ap", completed, "completes to longest common prefix of api+apex")
	require.Equal(t, []string{"api", "apex"}, cands)

	completed, cands = completeDir(listing, "/home/me/work/web")
	require.Equal(t, "/home/me/work/web", completed, "single match completes fully")
	require.Equal(t, []string{"web"}, cands)

	completed, cands = completeDir(listing, "/home/me/work/zzz")
	require.Equal(t, "/home/me/work/zzz", completed, "no match → unchanged")
	require.Nil(t, cands)
}
```

Add `"github.com/srajanpathak/agentctl/internal/client"` to the imports of `list_test.go` if not present.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run 'TestExpandPath|TestDirCompletionTarget|TestLongestCommonPrefix|TestCompleteDir' -v`
Expected: FAIL — helpers undefined.

- [ ] **Step 3: Implement**

Add `"github.com/srajanpathak/agentctl/internal/client"` to the imports of `list.go`, then add:

```go
// expandPath expands a leading ~, resolves relative paths against the cwd, and
// cleans the result to an absolute path. home is injected (empty = no expansion).
func expandPath(p, home string) string {
	if home != "" {
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return filepath.Clean(p)
}

// dirCompletionTarget splits a typed path into the directory to list and the
// leaf prefix to match. A trailing slash means "list this dir's children" (empty
// leaf); otherwise the last segment is the prefix to complete within its parent.
func dirCompletionTarget(typed string) (listDir, leaf string) {
	if typed == "" {
		return ".", ""
	}
	if strings.HasSuffix(typed, "/") {
		d := strings.TrimRight(typed, "/")
		if d == "" {
			d = "/"
		}
		return d, ""
	}
	return filepath.Dir(typed), filepath.Base(typed)
}

// longestCommonPrefix returns the longest string that prefixes every input.
func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	pre := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, pre) {
			pre = pre[:len(pre)-1]
			if pre == "" {
				return ""
			}
		}
	}
	return pre
}

// completeDir, given a listing of the directory that contains `typed`'s leaf,
// returns `typed` completed to the longest common prefix of the entries that
// match the leaf, plus the matching entry names (for display). When nothing
// matches, returns typed unchanged and nil candidates.
func completeDir(listing client.DirListing, typed string) (completed string, candidates []string) {
	listDir, leaf := dirCompletionTarget(typed)
	for _, e := range listing.Entries {
		if strings.HasPrefix(e.Name, leaf) {
			candidates = append(candidates, e.Name)
		}
	}
	if len(candidates) == 0 {
		return typed, nil
	}
	return filepath.Join(listDir, longestCommonPrefix(candidates)), candidates
}
```

Add `"path/filepath"` to `list.go` imports if not already present (it uses `os` and `strings`; add `path/filepath`).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tui/ -run 'TestExpandPath|TestDirCompletionTarget|TestLongestCommonPrefix|TestCompleteDir' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): path expand + dir tab-completion helpers"
```

---

## Task 6: Refactor `renderList`/`buildRows` to walk `[]item`

**Files:**
- Modify: `internal/tui/list.go` (`buildRows`, `renderList`, and the row→cursor helpers)
- Modify: `internal/tui/list_test.go` (existing render tests pass items)

- [ ] **Step 1: Update existing tests to the new signature**

In `internal/tui/list_test.go`, change every `renderList(m.sessions, m.cursor, ...)` call to `renderList(buildItems(m.sessions, nil), m.cursor, ...)`. There are calls in: `TestRenderListContainsAgeColumn`, `TestRenderListClampsToHeightAndKeepsCursor`, `TestRenderListShortListPadsToHeight`, `TestRenderListHeightOneRendersSingleLine`, `TestRenderListGroupsBySourceDir`, `TestRenderListGroupedSmallHeightKeepsCursor`.

Then add a placeholder-rendering test:

```go
func TestRenderListShowsPlaceholderForEmptyOpenedDir(t *testing.T) {
	now := time.Now()
	items := buildItems(
		[]*store.Session{{ID: "a1", Workdir: "/work/alpha", Status: store.StatusWorking, UpdatedAt: now.Add(-time.Hour)}},
		map[string]time.Time{"/work/empty": now},
	)
	out := renderList(items, 0, 120, 12)
	require.Contains(t, out, "/work/empty (0)", "empty opened dir header with zero count")
	require.Contains(t, out, "no agents", "placeholder hint line")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run TestRenderList -v`
Expected: FAIL — `renderList` still takes `[]*store.Session`; compile error / new test fails.

- [ ] **Step 3: Implement the refactor**

In `internal/tui/list.go`, replace `listRow`, `buildRows`, the cursor/header helpers, and `renderList` with item-based versions:

```go
// listRow is one rendered line: a group header (header != "") or a body row that
// points at items[idx]. buildRows assumes items are in grouped order (buildItems),
// so a group's rows are contiguous.
type listRow struct {
	header string
	idx    int
}

func buildRows(items []item) []listRow {
	var rows []listRow
	prev := ""
	for i := range items {
		dir := items[i].dir
		if i == 0 || dir != prev {
			count := 0
			for j := i; j < len(items) && items[j].dir == dir; j++ {
				if items[j].session != nil {
					count++
				}
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

func countRows(rows []listRow) int {
	n := 0
	for _, r := range rows {
		if r.header == "" {
			n++
		}
	}
	return n
}

// renderList renders the item list windowed to exactly height lines and width
// columns. Items are grouped by dir with dimmed header rows; an empty opened dir
// shows a selectable "(no agents …)" placeholder. The cursor item is always kept
// visible; a sticky header shows when the window starts mid-group.
func renderList(items []item, cursor, width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(items) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	rows := buildRows(items)
	n := len(rows)
	cursorRow := cursorRowIndex(rows, cursor)

	visible := height
	hidden := n > height
	if hidden {
		if visible = height - 1; visible < 1 {
			visible = 1
		}
	}

	top := listWindow(n, cursorRow, visible)
	sticky := rows[top].header == "" && visible > 1 && headerAbove(rows, top) != ""
	bodyCap := visible
	if sticky {
		top = listWindow(n, cursorRow, visible-1)
		sticky = rows[top].header == ""
		if sticky {
			bodyCap = visible - 1
		}
	}

	var b strings.Builder
	used := 0
	if sticky {
		b.WriteString(stMuted.Render(headerAbove(rows, top)) + "\n")
		used++
	}
	end := top
	for i := top; i < top+bodyCap && i < n; i++ {
		r := rows[i]
		if r.header != "" {
			b.WriteString(stMuted.Render(r.header) + "\n")
		} else {
			b.WriteString(renderItemLine(items[r.idx], r.idx == cursor, width) + "\n")
		}
		used++
		end = i + 1
	}

	if hidden && height > 1 {
		var parts []string
		if a := countRows(rows[:top]); a > 0 {
			parts = append(parts, fmt.Sprintf("▲ %d more", a))
		}
		if a := countRows(rows[end:]); a > 0 {
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

// renderItemLine renders one body row: an agent's columns, or the placeholder
// line for an empty opened dir. The cursor row gets the "› " caret + cursor style.
func renderItemLine(it item, selected bool, width int) string {
	var line string
	if it.session == nil {
		line = stMuted.Render("(no agents — n to spawn here)")
	} else {
		s := it.session
		label, st := badge(s.Status)
		line = fmt.Sprintf("%-12s %-9s %-11s %-5s %s",
			trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label), age(s.UpdatedAt),
			trunc(s.Subject, max(0, width-44)))
	}
	cur := "  "
	if selected {
		cur = stCursor.Render("› ")
		if it.session != nil {
			line = stCursor.Render(line)
		}
	}
	return cur + line
}
```

Notes: `countAgents` is renamed to `countRows` (it counts non-header rows, which now includes placeholders — both should count toward the ▲/▼ hint). Remove the old `countAgents`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/tui/ -run TestRenderList -v`
Expected: PASS (all render tests, including the new placeholder test).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "refactor(tui): renderList walks items + renders opened-dir placeholders"
```

---

## Task 7: New messages + commands (`spawnCmd` cwd, dir cmds)

**Files:**
- Modify: `internal/tui/cmds.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/cmds_test.go` (it is in package `tui`):

```go
func TestSpawnCmdUsesGivenCwd(t *testing.T) {
	f := &fakeAPI{}
	msg := spawnCmd(f, "do the thing", "/work/api")()
	done, ok := msg.(spawnDoneMsg)
	require.True(t, ok)
	require.NoError(t, done.err)
	require.NotNil(t, f.spawned)
	require.Equal(t, "/work/api", f.spawned.Cwd)
	require.Equal(t, "do the thing", f.spawned.Prompt)
}
```

If `require` is not already imported in `cmds_test.go`, add `"github.com/stretchr/testify/require"` and `"testing"`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run TestSpawnCmdUsesGivenCwd -v`
Expected: FAIL — `spawnCmd` takes 2 args, not 3.

- [ ] **Step 3: Implement**

In `internal/tui/cmds.go`, change `spawnCmd` to take an explicit cwd (drop the internal `os.Getwd()`):

```go
func spawnCmd(a api, prompt, cwd string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Prompt: prompt, Cwd: cwd})
		if err != nil {
			return spawnDoneMsg{err: err}
		}
		return spawnDoneMsg{id: s.ID}
	}
}
```

Add the dir messages + commands at the end of `cmds.go`:

```go
// dirListMsg carries a /fs/dirs listing back for tab-completion. typed is the
// expanded path the user had typed when completion was requested.
type dirListMsg struct {
	typed   string
	listing client.DirListing
	err     error
}

// openDirMsg is the result of validating a dir the user asked to open.
type openDirMsg struct {
	dir string
	err error
}

// listDirsCmd fetches listDir's subdirectories for completing `typed`.
func listDirsCmd(a api, typed, listDir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		l, err := a.ListDirs(ctx, listDir)
		return dirListMsg{typed: typed, listing: l, err: err}
	}
}

// openDirCmd validates that dir is a readable directory (via /fs/dirs).
func openDirCmd(a api, dir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		_, err := a.ListDirs(ctx, dir)
		return openDirMsg{dir: dir, err: err}
	}
}
```

If `os` is now unused in `cmds.go` after removing `os.Getwd()`, drop it from the import block.

- [ ] **Step 4: Verify (build + the new test) — call sites updated next task**

The existing `spawnCmd(m.api, prompt)` call sites in `keys.go` and `list_pane.go` now fail to compile; they are fixed in Tasks 8 and 9. Verify just this command in isolation by temporarily building the test for the cmds file is not possible without the package compiling, so defer the run to Step 4 of Task 8. For now:

Run: `go vet ./internal/tui/ 2>&1 | head` 
Expected: errors only of the form "not enough arguments in call to spawnCmd" at `keys.go` and `list_pane.go` — confirming the signature changed and pinpointing the two call sites to fix.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/cmds.go internal/tui/cmds_test.go
git commit -m "feat(tui): spawnCmd takes cwd; add dir list/open commands"
```

---

## Task 8: Cockpit wiring (`listPaneModel`)

**Files:**
- Modify: `internal/tui/model.go` (mode constants — shared)
- Modify: `internal/tui/list_pane.go`
- Test: `internal/tui/list_pane_test.go`

- [ ] **Step 1: Add the shared mode constants**

In `internal/tui/model.go`, extend the `mode` const block:

```go
const (
	modeNormal mode = iota
	modeNewAgent
	modeSendMsg
	modeConfirmKill
	modeHelp
	modeOpenDir     // path input for `o`
	modeNewAgentDir // dir-override sub-state of modeNewAgent
)
```

- [ ] **Step 2: Write the failing tests**

Add to `internal/tui/list_pane_test.go`:

```go
func TestListPaneOpenDirAddsPlaceholder(t *testing.T) {
	f := &fakeAPI{dirListing: client.DirListing{Path: "/work/api"}}
	m := newListPane(f, "%9")
	m = lstep(m, key("o"))
	require.Equal(t, modeOpenDir, m.mode)
	m.tp.SetValue("/work/api")
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd)
	m = lstep(m, cmd().(openDirMsg)) // not how Update returns; apply the msg directly below
	// apply the openDirMsg the command produced:
	m = lstep(m, openDirMsg{dir: "/work/api"})
	require.Equal(t, modeNormal, m.mode)
	items := m.items()
	require.Len(t, items, 1)
	require.Nil(t, items[0].session, "opened dir shows as a placeholder")
	require.Equal(t, "/work/api", items[0].dir)
}

func TestListPaneNewAgentResolvesTargetDir(t *testing.T) {
	now := time.Now()
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/work/api", UpdatedAt: now}}})
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	require.Equal(t, "/work/api", m.targetDir, "new agent defaults to the cursor group's dir")
}

func TestListPaneCloseOpenedDirWithX(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m.openedDirs["/work/empty"] = time.Now()
	require.Len(t, m.items(), 1)
	m = lstep(m, key("x")) // cursor is on the placeholder
	require.Empty(t, m.openedDirs, "x on a placeholder closes the opened dir")
	require.NotEqual(t, modeConfirmKill, m.mode, "no kill-confirm for a placeholder")
}

func TestListPaneXOnAgentStillConfirms(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/work/api"}}})
	m = lstep(m, key("x"))
	require.Equal(t, modeConfirmKill, m.mode)
}
```

The first test has an awkward double-apply; simplify it to apply the message the command would emit:

```go
func TestListPaneOpenDirAddsPlaceholder(t *testing.T) {
	f := &fakeAPI{dirListing: client.DirListing{Path: "/work/api"}}
	m := newListPane(f, "%9")
	m = lstep(m, key("o"))
	require.Equal(t, modeOpenDir, m.mode)
	m.tp.SetValue("/work/api")
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd, "enter dispatches openDirCmd")
	m = lstep(m, openDirMsg{dir: "/work/api"}) // the validated result
	require.Equal(t, modeNormal, m.mode)
	items := m.items()
	require.Len(t, items, 1)
	require.Nil(t, items[0].session)
	require.Equal(t, "/work/api", items[0].dir)
}
```

Use this version; delete the earlier draft. Add `"github.com/srajanpathak/agentctl/internal/client"` to `list_pane_test.go` imports.

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/tui/ -run 'TestListPaneOpenDir|TestListPaneNewAgentResolves|TestListPaneCloseOpenedDir|TestListPaneXOnAgent' -v`
Expected: FAIL — fields/handlers undefined (and package may not compile until Step 4).

- [ ] **Step 4: Implement the cockpit wiring**

In `internal/tui/list_pane.go`:

(a) Add fields to `listPaneModel`:

```go
	tp            textinput.Model
	openedDirs    map[string]time.Time
	dirCandidates []string
	targetDir     string
```

Add the `time` import.

(b) Initialise in `newListPane`:

```go
func newListPane(a api, detailPane string) listPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	tp := textinput.New()
	tp.Placeholder = "~/path/to/dir"
	return listPaneModel{
		api: a, ta: ta, ti: ti, tp: tp, detailPane: detailPane,
		openedDirs: map[string]time.Time{}, connected: true,
	}
}
```

(c) Replace the accessors `selectedID`/`selected` and add `items`/`selectedKey`/`activeDir`/`fallbackDir`:

```go
func (m listPaneModel) items() []item { return buildItems(m.sessions, m.openedDirs) }

func (m listPaneModel) selected() *store.Session { return itemAt(m.items(), m.cursor).session }

func (m listPaneModel) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m listPaneModel) selectedKey() string { return itemKey(itemAt(m.items(), m.cursor)) }

func (m listPaneModel) fallbackDir() string {
	d, _ := os.Getwd()
	return d
}

func (m listPaneModel) activeDir() string {
	return activeDir(m.items(), m.cursor, m.fallbackDir())
}
```

(d) Change `repin` to key-based:

```go
func (m *listPaneModel) repin(prevKey string) {
	items := m.items()
	want := prevKey
	if m.pendingSelect != "" {
		want = m.pendingSelect
	}
	if want != "" {
		for i, it := range items {
			if itemKey(it) == want {
				m.cursor = i
				if want == m.pendingSelect {
					m.pendingSelect = ""
				}
				return
			}
		}
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}
```

(e) In `Update`, change the `sessionsMsg` arm to use the key:

```go
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prev := m.selectedKey()
		m.sessions = groupSort(msg.sessions)
		m.repin(prev)
		return m, nil
```

and add two new arms (anywhere among the message cases):

```go
	case dirListMsg:
		if msg.err == nil && (m.mode == modeOpenDir || m.mode == modeNewAgentDir) {
			completed, cands := completeDir(msg.listing, msg.typed)
			m.tp.SetValue(completed)
			m.tp.CursorEnd()
			m.dirCandidates = cands
		}
		return m, nil
	case openDirMsg:
		if msg.err != nil {
			m.status = "cannot open " + msg.dir + ": " + msg.err.Error()
			return m, nil
		}
		m.openedDirs[msg.dir] = time.Now()
		m.pendingSelect = dirKey(msg.dir)
		m.mode = modeNormal
		m.tp.Blur()
		m.dirCandidates = nil
		m.repin("")
		return m, nil
```

(f) In `handleKey`, add the two dir-input mode handlers (alongside the existing `modeNewAgent`/`modeSendMsg`/etc. switch on `m.mode`):

```go
	case modeOpenDir:
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeNormal
			m.tp.Blur()
			m.dirCandidates = nil
			return m, nil
		case tea.KeyTab:
			typed := expandPath(m.tp.Value(), homeDir())
			listDir, _ := dirCompletionTarget(typed)
			return m, listDirsCmd(m.api, typed, listDir)
		case tea.KeyEnter:
			return m, openDirCmd(m.api, expandPath(m.tp.Value(), homeDir()))
		}
		var cmd tea.Cmd
		m.tp, cmd = m.tp.Update(msg)
		return m, cmd
	case modeNewAgentDir:
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeNewAgent
			m.tp.Blur()
			m.dirCandidates = nil
			m.ta.Focus()
			return m, nil
		case tea.KeyTab:
			typed := expandPath(m.tp.Value(), homeDir())
			listDir, _ := dirCompletionTarget(typed)
			return m, listDirsCmd(m.api, typed, listDir)
		case tea.KeyEnter:
			m.targetDir = expandPath(m.tp.Value(), homeDir())
			m.mode = modeNewAgent
			m.tp.Blur()
			m.dirCandidates = nil
			m.ta.Focus()
			return m, nil
		}
		var cmd tea.Cmd
		m.tp, cmd = m.tp.Update(msg)
		return m, cmd
```

(g) In `modeNewAgent` (existing case), add a `tab` branch to open the dir override:

```go
	case modeNewAgent:
		switch msg.Type {
		case tea.KeyEsc:
			m.mode = modeNormal
			m.ta.Blur()
			return m, nil
		case tea.KeyTab:
			m.mode = modeNewAgentDir
			m.ta.Blur()
			m.tp.SetValue(m.targetDir)
			m.tp.CursorEnd()
			m.tp.Focus()
			m.dirCandidates = nil
			return m, nil
		case tea.KeyCtrlS:
			prompt := strings.TrimSpace(m.ta.Value())
			m.mode = modeNormal
			m.ta.Blur()
			if prompt == "" {
				m.status = "prompt was empty"
				return m, nil
			}
			return m, spawnCmd(m.api, prompt, m.targetDir)
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
```

(h) In the normal-mode key switch, update `n`, add `o`, and branch `x`:

```go
		case "n":
			m.targetDir = m.activeDir()
			m.mode = modeNewAgent
			m.ta.Reset()
			m.ta.Focus()
		case "o":
			m.mode = modeOpenDir
			m.tp.Reset()
			m.tp.Focus()
			m.dirCandidates = nil
		case "x":
			it := itemAt(m.items(), m.cursor)
			if it.session == nil {
				if it.dir != "" {
					delete(m.openedDirs, it.dir)
					m.status = "closed " + abbrevHome(it.dir)
				}
			} else {
				m.mode = modeConfirmKill
			}
```

(Remove the old `case "n"` and `case "x"` bodies they replace. The down/up nav already clamps via `len(m.sessions)-1` — change those two to `len(m.items())-1`.)

Update the nav cases:

```go
		case "down", "j":
			if m.cursor < len(m.items())-1 {
				m.cursor++
			}
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
```

(i) Update `View`: change `renderList(m.sessions, …)` to `renderList(m.items(), …)`, keep the title count as agent count (`len(m.sessions)`), add the footer hint and the three new-mode footers:

```go
	footer := stMuted.Render("enter open · n new · o open dir · s send · a attach · x kill · ? help · q quit")
	if m.status != "" {
		footer = stStatus.Render(m.status)
	}
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — "+abbrevHome(m.targetDir)+"  (tab: change dir · ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeNewAgentDir:
		footer = stPaneTitle.Render("Launch dir (tab complete · enter · esc)") + "\n" + m.tp.View() + "\n" + stMuted.Render(strings.Join(m.dirCandidates, "  "))
	case modeOpenDir:
		footer = stPaneTitle.Render("Open directory (tab complete · enter · esc)") + "\n" + m.tp.View() + "\n" + stMuted.Render(strings.Join(m.dirCandidates, "  "))
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		footer = stError.Render("Kill & remove " + m.selectedID() + "? y / N")
	}
```

Also change the body line `body := titleBox(title, renderList(m.sessions, m.cursor, m.w-2, bodyH-2), m.w, bodyH)` to `renderList(m.items(), m.cursor, m.w-2, bodyH-2)`.

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/tui/ -run 'TestListPane' -v`
Expected: PASS (existing cockpit tests + the four new ones).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/model.go internal/tui/list_pane.go internal/tui/list_pane_test.go
git commit -m "feat(tui): cockpit o (open dir) + dir-aware n + x-closes-placeholder"
```

---

## Task 9: Classic wiring (`Model`)

**Files:**
- Modify: `internal/tui/model.go` (`Model` fields, accessors, `repin`, `Update` arms)
- Modify: `internal/tui/keys.go` (key handlers)
- Modify: `internal/tui/view.go` (forms, footer, help, `renderList` call)
- Test: `internal/tui/model_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/tui/model_test.go`:

```go
func TestModelNewAgentResolvesTargetDir(t *testing.T) {
	now := time.Now()
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "a1", Workdir: "/work/api", UpdatedAt: now}}})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	require.Equal(t, "/work/api", m.targetDir)
}

func TestModelOpenDirAddsPlaceholder(t *testing.T) {
	m := New(&fakeAPI{dirListing: client.DirListing{Path: "/work/api"}})
	m = step(m, key("o"))
	require.Equal(t, modeOpenDir, m.mode)
	m = step(m, openDirMsg{dir: "/work/api"})
	require.Equal(t, modeNormal, m.mode)
	items := m.items()
	require.Len(t, items, 1)
	require.Nil(t, items[0].session)
	require.Equal(t, "/work/api", items[0].dir)
}

func TestModelCloseOpenedDirWithX(t *testing.T) {
	m := New(&fakeAPI{})
	m.openedDirs["/work/empty"] = time.Now()
	m = step(m, key("x"))
	require.Empty(t, m.openedDirs)
	require.NotEqual(t, modeConfirmKill, m.mode)
}
```

`client` and `time` are already imported in `model_test.go`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run 'TestModelNewAgentResolves|TestModelOpenDir|TestModelCloseOpenedDir' -v`
Expected: FAIL — fields/handlers undefined (package may not compile yet).

- [ ] **Step 3: Implement — `model.go`**

(a) Add fields to `Model`:

```go
	tp            textinput.Model
	openedDirs    map[string]time.Time
	dirCandidates []string
	targetDir     string
```

Add the `time` import to `model.go`.

(b) Initialise in `New`:

```go
func New(a api) Model {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	tp := textinput.New()
	tp.Placeholder = "~/path/to/dir"
	return Model{a: a, ta: ta, ti: ti, tp: tp, openedDirs: map[string]time.Time{}, connected: true}
}
```

(Match the existing field name for the api — the current `New` sets `api: a`; keep that: use `api: a` not `a: a`.) Concretely:

```go
	return Model{api: a, ta: ta, ti: ti, tp: tp, openedDirs: map[string]time.Time{}, connected: true}
```

(c) Replace `selected`/`selectedID` and add `items`/`selectedKey`/`activeDir`/`fallbackDir`:

```go
func (m Model) items() []item { return buildItems(m.sessions, m.openedDirs) }

func (m Model) selected() *store.Session { return itemAt(m.items(), m.cursor).session }

func (m Model) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m Model) selectedKey() string { return itemKey(itemAt(m.items(), m.cursor)) }

func (m Model) fallbackDir() string {
	d, _ := os.Getwd()
	return d
}

func (m Model) activeDir() string {
	return activeDir(m.items(), m.cursor, m.fallbackDir())
}
```

Add `"os"` to `model.go` imports.

(d) Change `repin` to key-based (same body as the cockpit version):

```go
func (m *Model) repin(prevKey string) {
	items := m.items()
	want := prevKey
	if m.pendingSelect != "" {
		want = m.pendingSelect
	}
	if want != "" {
		for i, it := range items {
			if itemKey(it) == want {
				m.cursor = i
				if want == m.pendingSelect {
					m.pendingSelect = ""
				}
				return
			}
		}
	}
	if m.cursor >= len(items) {
		m.cursor = len(items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}
```

(e) In `Update`, change the `sessionsMsg` arm:

```go
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prevKey := m.selectedKey()
		m.sessions = groupSort(msg.sessions)
		m.repin(prevKey)
		return m, nil
```

and add the `dirListMsg`/`openDirMsg` arms (identical bodies to the cockpit Task 8(e)):

```go
	case dirListMsg:
		if msg.err == nil && (m.mode == modeOpenDir || m.mode == modeNewAgentDir) {
			completed, cands := completeDir(msg.listing, msg.typed)
			m.tp.SetValue(completed)
			m.tp.CursorEnd()
			m.dirCandidates = cands
		}
		return m, nil
	case openDirMsg:
		if msg.err != nil {
			m.status = "cannot open " + msg.dir + ": " + msg.err.Error()
			return m, nil
		}
		m.openedDirs[msg.dir] = time.Now()
		m.pendingSelect = dirKey(msg.dir)
		m.mode = modeNormal
		m.tp.Blur()
		m.dirCandidates = nil
		m.repin("")
		return m, nil
```

(f) In the `tea.KeyMsg` dispatch of `Update`, route the two new modes (add alongside the existing `if m.mode == modeNewAgent {…}` checks):

```go
		if m.mode == modeOpenDir {
			return m.updateOpenDir(msg)
		}
		if m.mode == modeNewAgentDir {
			return m.updateNewAgentDir(msg)
		}
```

- [ ] **Step 4: Implement — `keys.go`**

(a) Update `handleKey` normal-mode cases: change nav clamps to `len(m.items())-1`, replace `n` and `x`, add `o`:

```go
		case "down", "j":
			if m.cursor < len(m.items())-1 {
				m.cursor++
			}
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		case "n":
			m.targetDir = m.activeDir()
			m.mode = modeNewAgent
			m.ta.Reset()
			m.ta.Focus()
			return m, nil
		case "o":
			m.mode = modeOpenDir
			m.tp.Reset()
			m.tp.Focus()
			m.dirCandidates = nil
			return m, nil
		case "x":
			it := itemAt(m.items(), m.cursor)
			if it.session == nil {
				if it.dir != "" {
					delete(m.openedDirs, it.dir)
					m.status = "closed " + abbrevHome(it.dir)
				}
			} else {
				m.mode = modeConfirmKill
			}
			return m, nil
```

(b) Add a `tab` branch in `updateNewAgent` (before the `KeyCtrlS` case) and pass `targetDir` to `spawnCmd`:

```go
func (m Model) updateNewAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.ta.Blur()
		return m, nil
	case tea.KeyTab:
		m.mode = modeNewAgentDir
		m.ta.Blur()
		m.tp.SetValue(m.targetDir)
		m.tp.CursorEnd()
		m.tp.Focus()
		m.dirCandidates = nil
		return m, nil
	case tea.KeyCtrlS:
		prompt := strings.TrimSpace(m.ta.Value())
		m.mode = modeNormal
		m.ta.Blur()
		if prompt == "" {
			m.status = "prompt was empty"
			return m, nil
		}
		return m, spawnCmd(m.api, prompt, m.targetDir)
	}
	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}
```

(c) Add the two new mode handlers to `keys.go`:

```go
func (m Model) updateOpenDir(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.tp.Blur()
		m.dirCandidates = nil
		return m, nil
	case tea.KeyTab:
		typed := expandPath(m.tp.Value(), homeDir())
		listDir, _ := dirCompletionTarget(typed)
		return m, listDirsCmd(m.api, typed, listDir)
	case tea.KeyEnter:
		return m, openDirCmd(m.api, expandPath(m.tp.Value(), homeDir()))
	}
	var cmd tea.Cmd
	m.tp, cmd = m.tp.Update(msg)
	return m, cmd
}

func (m Model) updateNewAgentDir(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNewAgent
		m.tp.Blur()
		m.dirCandidates = nil
		m.ta.Focus()
		return m, nil
	case tea.KeyTab:
		typed := expandPath(m.tp.Value(), homeDir())
		listDir, _ := dirCompletionTarget(typed)
		return m, listDirsCmd(m.api, typed, listDir)
	case tea.KeyEnter:
		m.targetDir = expandPath(m.tp.Value(), homeDir())
		m.mode = modeNewAgent
		m.tp.Blur()
		m.dirCandidates = nil
		m.ta.Focus()
		return m, nil
	}
	var cmd tea.Cmd
	m.tp, cmd = m.tp.Update(msg)
	return m, cmd
}
```

Note: the classic `Update` swallows `tab` in normal mode for output focus *before* `handleKey`; that path is unchanged. The `tab` handling above is only reached inside the new-agent modes.

- [ ] **Step 5: Implement — `view.go`**

(a) Change the `renderList` call in `View`:

```go
			left := titleBox(listTitle, renderList(m.items(), m.cursor, listOuter-2, bodyH-2), listOuter, bodyH)
```

(b) Extend the mode `switch` in `View` with the new forms:

```go
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — "+abbrevHome(m.targetDir)+"  (tab: change dir · ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeNewAgentDir:
		footer = stPaneTitle.Render("Launch dir (tab complete · enter · esc)") + "\n" + m.tp.View() + "\n" + stMuted.Render(strings.Join(m.dirCandidates, "  "))
	case modeOpenDir:
		footer = stPaneTitle.Render("Open directory (tab complete · enter · esc)") + "\n" + m.tp.View() + "\n" + stMuted.Render(strings.Join(m.dirCandidates, "  "))
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		footer = stError.Render("Kill & remove " + m.selectedID() + "? y / N")
	}
```

Add `"strings"` to `view.go` imports.

(c) Add `o` to the footer string and help:

```go
func (m Model) footer() string {
	if m.status != "" {
		return stStatus.Render(m.status)
	}
	return stMuted.Render("n new · o open dir · s send · a attach · x kill · tab focus · ? help · q quit")
}
```

In `helpText()` add a line after the `n` line:

```go
		"  o            open a directory as a group (spawn target for n)\n" +
```

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/tui/ -v`
Expected: PASS (all TUI tests).

- [ ] **Step 7: Commit**

```bash
git add internal/tui/model.go internal/tui/keys.go internal/tui/view.go internal/tui/model_test.go
git commit -m "feat(tui): classic o (open dir) + dir-aware n + x-closes-placeholder"
```

---

## Task 10: Full build, vet, and manual smoke

**Files:** none (verification only)

- [ ] **Step 1: Build and vet the whole module**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: all PASS, no vet warnings.

- [ ] **Step 2: Manual smoke (cockpit)**

Run: `go run ./cmd/agentctl tui` (requires the daemon running: `go run ./cmd/agentctl daemon` in another shell).
Verify:
- `o` opens the path input; typing a partial path + `tab` completes/lists candidates; `enter` adds an empty group with a `(no agents — n to spawn here)` row at the top.
- Parking the cursor on that placeholder and pressing `n` shows `New agent — <dir>`; `ctrl+s` spawns an agent that appears under that same dir group (placeholder gone).
- `tab` inside the new-agent form lets you change the dir, then returns to the prompt.
- `x` on a placeholder closes the opened dir; `x` on an agent still prompts kill & remove.

- [ ] **Step 3: Commit (only if smoke surfaced doc-worthy notes)**

No code commit expected here. If README key tables need the `o` key, update `README.md` and commit:

```bash
git add README.md
git commit -m "docs: document the TUI o (open dir) key"
```

---

## Self-Review Notes (author check — completed)

- **Spec coverage:** opened-dir state (Task 8/9 fields) ✓; selectable placeholder rows (Tasks 3, 6) ✓; `o` + completion (Tasks 5, 7, 8, 9) ✓; dir-aware `n` + override (Tasks 8g/9b) ✓; `x` overload + sticky lifecycle (Tasks 8h/9 keys; stickiness is implicit — `openedDirs` only removed by `x` or quit) ✓; `—` fallback (Task 4) ✓; client `ListDirs` (Task 1) ✓; both models (Tasks 8, 9) ✓; footer/help (Tasks 8i, 9) ✓; path normalization for merge (`expandPath` Clean+abs, Task 5; merge relies on `sourceDir==Workdir==opened dir`) ✓.
- **Type consistency:** `buildItems`, `itemAt`, `itemKey`, `dirKey`, `activeDir`, `expandPath`, `dirCompletionTarget`, `completeDir`, `longestCommonPrefix`, `renderList([]item,…)`, `spawnCmd(api,prompt,cwd)`, `listDirsCmd(api,typed,listDir)`, `openDirCmd(api,dir)`, `dirListMsg{typed,listing,err}`, `openDirMsg{dir,err}` — names used identically across tasks. `countAgents`→`countRows` rename applied in Task 6.
- **Placeholder scan:** no TBD/"handle edge cases"/"similar to Task N"; the one awkward first-draft test in Task 8 Step 2 is explicitly replaced with the final version in the same step.
