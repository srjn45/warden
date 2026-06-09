# TUI master-Claude pane (tmux-composited cockpit) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-architect `agentctl tui` from one full-screen bubbletea program into a tmux-composited cockpit — agents list (top-left), an embedded live `claude` "master" session (bottom-left), and the agent detail viewer (full-height right) — with list→detail selection synced through a local state file.

**Architecture:** `agentctl tui` becomes a *launcher*: it builds a tmux session named `agentctl-tui-<pid>` with three panes, each its own process. Top-left and right run new, focused bubbletea pane-apps (`--pane=list`, `--pane=detail`); bottom-left runs plain `claude` (which inherits the user-scope `agentctl` MCP). The daemon stays the single source of truth — both pane-apps poll it independently. The only cross-pane state is the selected agent id, written by the list pane to `selection.json` in a per-pid state dir and read by the detail pane on its existing tick. The legacy single-pane app is kept as `--classic` and is the automatic fallback when tmux is unavailable or we're already inside tmux. The master is **ephemeral** (dies with the cockpit); persistence is a documented future enhancement.

**Tech Stack:** Go, `charmbracelet/bubbletea` + `lipgloss`, tmux (compositor), cobra (CLI), `stretchr/testify` (tests). Reuses `internal/lifecycle.Runner` (the tmux exec seam) and `internal/client` (daemon HTTP client).

---

## Spec reference

`docs/superpowers/specs/2026-06-03-agentctl-tui-master-pane-design.md`

## File Structure

**New files (all in `internal/tui/`):**
- `selection.go` — read/write the `selection.json` state file; cockpit state-dir paths. One responsibility: cross-pane selection persistence.
- `selection_test.go` — round-trip + missing/corrupt handling.
- `compositor.go` — `chooseClassic` (mode decision), `tmuxAvailable`, cockpit dir/session naming, `buildCockpit` (the tmux pane-construction sequence over a `lifecycle.Runner`), and `RunCockpit` (build + attach + cleanup). One responsibility: turning "launch the cockpit" into tmux commands.
- `compositor_test.go` — mode table + tmux call-sequence assertions via `lifecycle.FakeRunner`.
- `list_pane.go` — `listPaneModel` + `RunListPane`. The top-left list + action modals; writes selection on cursor change.
- `list_pane_test.go`.
- `detail_pane.go` — `detailPaneModel` + `RunDetailPane`. The read-only right viewport; reads selection each tick.
- `detail_pane_test.go`.

**Modified files:**
- `internal/tui/list.go` — convert `renderList` from a `Model` method to a free function (shared by classic + list pane).
- `internal/tui/detail.go` — convert `renderDetail` from a `Model` method to a free function (shared by classic + detail pane).
- `internal/tui/view.go` — update the two call sites to the new free functions (classic behavior unchanged).
- `internal/cli/tui.go` — add `--classic`, hidden `--pane` and `--state-dir` flags; dispatch to pane apps / cockpit / classic.
- `internal/cli/root.go` — bare `agentctl` (no args) launches the cockpit (classic-aware) instead of `tui.Run` directly.
- `docs/USAGE.md` — document the cockpit, panes, master, and `--classic`.

## Conventions to follow (from the existing code)

- Tests use `testify/require` and table style; bubbletea models are pure reducers tested by applying messages (`step`/`key` helpers in `model_test.go`).
- tmux is driven through `lifecycle.Runner`; `lifecycle.FakeRunner` records `Calls` and matches `Responses` by `"name arg1 arg2 …"`.
- Pure helpers + a thin exec wrapper (see `boxes.go`, `lifecycle.go`).
- Commit after every green step.

---

### Task 1: Selection state file

**Files:**
- Create: `internal/tui/selection.go`
- Test: `internal/tui/selection_test.go`

- [ ] **Step 1: Write the failing test**

```go
package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSelectionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeSelection(dir, "agent-4f98", 1700000000))
	require.Equal(t, "agent-4f98", readSelection(dir))
}

func TestSelectionMissingReturnsEmpty(t *testing.T) {
	require.Equal(t, "", readSelection(t.TempDir()))
	require.Equal(t, "", readSelection("")) // empty dir is a no-op
}

func TestSelectionCorruptReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "selection.json"), []byte("{not json"), 0o600))
	require.Equal(t, "", readSelection(dir))
}

func TestWriteSelectionEmptyDirIsNoop(t *testing.T) {
	require.NoError(t, writeSelection("", "x", 0)) // must not error or panic
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestSelection -v`
Expected: FAIL — `undefined: writeSelection` / `readSelection`.

- [ ] **Step 3: Write minimal implementation**

```go
package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// selection is the cross-pane state written by the list pane and read by the
// detail pane. It lives in the cockpit's per-pid state dir, never the daemon.
type selection struct {
	ID string `json:"id"`
	TS int64  `json:"ts"`
}

func selectionPath(stateDir string) string {
	return filepath.Join(stateDir, "selection.json")
}

// writeSelection atomically records the selected agent id. An empty stateDir is
// a no-op (classic mode has no cockpit state dir).
func writeSelection(stateDir, id string, ts int64) error {
	if stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(selection{ID: id, TS: ts})
	if err != nil {
		return err
	}
	tmp := selectionPath(stateDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, selectionPath(stateDir))
}

// readSelection returns the selected id, or "" if unset, missing, or corrupt.
func readSelection(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	b, err := os.ReadFile(selectionPath(stateDir))
	if err != nil {
		return ""
	}
	var s selection
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return s.ID
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestSelection -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/selection.go internal/tui/selection_test.go
git commit -m "feat(tui): selection state file for cross-pane sync"
```

---

### Task 2: Extract renderList / renderDetail into free functions

This makes the two renderers reusable by both the classic `Model` and the new pane models, without behavior change. Pure refactor — existing tests must stay green.

**Files:**
- Modify: `internal/tui/list.go` (the `renderList` method)
- Modify: `internal/tui/detail.go` (the `renderDetail` method)
- Modify: `internal/tui/view.go` (two call sites)

- [ ] **Step 1: Convert `renderList` to a free function**

In `internal/tui/list.go`, change the method signature and its two field references:

Replace:
```go
// renderList renders the agent list windowed to exactly `height` lines and
// `width` columns of inner content, always keeping the selected row visible.
func (m Model) renderList(width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(m.sessions) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	n := len(m.sessions)
```
with:
```go
// renderList renders the agent list windowed to exactly `height` lines and
// `width` columns of inner content, always keeping the selected row visible.
func renderList(sessions []*store.Session, cursor, width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(sessions) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	n := len(sessions)
```

Then, within that function body, replace every remaining `m.sessions` with `sessions` and every `m.cursor` with `cursor`. (There are references in the `for` loop, `badge(s.Status)`, the `i == m.cursor` check, and `listWindow(n, m.cursor, visible)`.)

- [ ] **Step 2: Convert `renderDetail` to a free function**

In `internal/tui/detail.go`, replace:
```go
func (m Model) renderDetail(width int) string {
	s := m.selected()
	if s == nil {
		return stMuted.Render("Select an agent")
	}
```
with:
```go
func renderDetail(s *store.Session, vp viewport.Model, outputFocused bool, width int) string {
	if s == nil {
		return stMuted.Render("Select an agent")
	}
```
Then in the same function replace `m.vp.View()` with `vp.View()` and `focusHint(m.outputFocused)` with `focusHint(outputFocused)`. Add the viewport import:
```go
import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/srajanpathak/agentctl/internal/store"
)
```

- [ ] **Step 3: Update the call sites in `view.go`**

In `internal/tui/view.go`, inside `View()`, replace:
```go
		left := titleBox(listTitle, m.renderList(listOuter-2, bodyH-2), listOuter, bodyH)
		right := titleBox(detailTitle, m.renderDetail(detailOuter-2), detailOuter, bodyH)
```
with:
```go
		left := titleBox(listTitle, renderList(m.sessions, m.cursor, listOuter-2, bodyH-2), listOuter, bodyH)
		right := titleBox(detailTitle, renderDetail(m.selected(), m.vp, m.outputFocused, detailOuter-2), detailOuter, bodyH)
```

- [ ] **Step 4: Run the full tui suite to verify no behavior change**

Run: `go test ./internal/tui/ -v`
Expected: PASS — all existing tests (`model_test.go`, `list_test.go`, `boxes_test.go`) still green.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/detail.go internal/tui/view.go
git commit -m "refactor(tui): renderList/renderDetail as free functions for pane reuse"
```

---

### Task 3: Mode decision + tmux availability helpers

**Files:**
- Create: `internal/tui/compositor.go`
- Test: `internal/tui/compositor_test.go`

- [ ] **Step 1: Write the failing test**

```go
package tui

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChooseClassic(t *testing.T) {
	cases := []struct {
		name                                  string
		classicFlag, tmuxAvailable, insideTmux bool
		want                                  bool
	}{
		{"default composited", false, true, false, false},
		{"explicit --classic", true, true, false, true},
		{"no tmux falls back", false, false, false, true},
		{"inside tmux falls back", false, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, chooseClassic(c.classicFlag, c.tmuxAvailable, c.insideTmux))
		})
	}
}

func TestCockpitNames(t *testing.T) {
	require.Equal(t, "agentctl-tui-1234", cockpitSession(1234))
	require.Equal(t, filepath.Join("/run/agentctl", "tui-1234"), cockpitStateDir("/run/agentctl", 1234))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestChooseClassic|TestCockpitNames' -v`
Expected: FAIL — `undefined: chooseClassic` / `cockpitSession` / `cockpitStateDir`.

- [ ] **Step 3: Write minimal implementation**

```go
package tui

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

// chooseClassic decides whether to use the legacy single-pane TUI instead of the
// tmux-composited cockpit. We require a real, non-nested tmux: composited mode
// builds a new session and attaches to it, which can't be done cleanly from
// inside an existing tmux client.
func chooseClassic(classicFlag, tmuxAvailable, insideTmux bool) bool {
	return classicFlag || !tmuxAvailable || insideTmux
}

// tmuxAvailable reports whether the tmux binary is on PATH.
func tmuxAvailable() bool {
	_, err := exec.LookPath("tmux")
	return err == nil
}

func cockpitSession(pid int) string {
	return fmt.Sprintf("agentctl-tui-%d", pid)
}

func cockpitStateDir(base string, pid int) string {
	return filepath.Join(base, fmt.Sprintf("tui-%d", pid))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestChooseClassic|TestCockpitNames' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/compositor.go internal/tui/compositor_test.go
git commit -m "feat(tui): cockpit mode-decision + naming helpers"
```

---

### Task 4: tmux pane-construction sequence (`buildCockpit`)

Build the three-pane layout over the `lifecycle.Runner` seam so the exact tmux call sequence is unit-testable, exactly like `internal/lifecycle` does.

> **VERIFY BEFORE IMPLEMENTING:** This uses `split-window -l <N%>`, which requires tmux ≥ 3.1. Run `tmux -V`. If the installed tmux is older, change the two `-l "60%"`/`-l "50%"` pairs to `-p "60"`/`-p "50"` (the deprecated percentage flag) — everything else is identical.

**Files:**
- Modify: `internal/tui/compositor.go`
- Test: `internal/tui/compositor_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/compositor_test.go`:
```go
import (
	"context"
	"strings"
	// keep existing imports (path/filepath, testing, require)
	"github.com/srajanpathak/agentctl/internal/lifecycle"
)

func TestBuildCockpitSequence(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	// Make each pane-creating tmux call return a distinct pane id.
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} /bin/agentctl tui --pane=list --state-dir=/st"] = lifecycle.FakeResp{Out: "%0\n"}

	o := cockpitOpts{session: "S", self: "/bin/agentctl", stateDir: "/st", homeDir: "/home", masterCwd: "/work"}
	err := buildCockpit(context.Background(), fr, o)
	require.NoError(t, err)

	// 1) session created with the list pane command
	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", "S", "-c", "/home", "-P", "-F", "#{pane_id}", "/bin/agentctl tui --pane=list --state-dir=/st"}, fr.Calls[0].Argv)
	// 2) detail pane: split the list pane horizontally, 60% to the new right pane
	require.Equal(t, []string{"tmux", "split-window", "-h", "-l", "60%", "-t", "%0", "-c", "/home", "-P", "-F", "#{pane_id}", "/bin/agentctl tui --pane=detail --state-dir=/st"}, fr.Calls[1].Argv)
	// 3) master pane: split the list pane vertically, 50%, running claude in the work dir
	require.Equal(t, []string{"tmux", "split-window", "-v", "-l", "50%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", "claude"}, fr.Calls[2].Argv)
	// 4) mouse on + focus the list pane
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "mouse", "on"}, fr.Calls[3].Argv)
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%0"}, fr.Calls[4].Argv)
}

func TestPaneCommandStrings(t *testing.T) {
	require.Equal(t, "/bin/agentctl tui --pane=list --state-dir=/st", listPaneCmd("/bin/agentctl", "/st"))
	require.Equal(t, "/bin/agentctl tui --pane=detail --state-dir=/st", detailPaneCmd("/bin/agentctl", "/st"))
	// paths with spaces are single-quoted so tmux's `sh -c` keeps them intact
	require.True(t, strings.Contains(shquote("a b"), "'a b'"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestBuildCockpitSequence|TestPaneCommandStrings' -v`
Expected: FAIL — `undefined: cockpitOpts` / `buildCockpit` / `listPaneCmd` / `detailPaneCmd` / `shquote`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/tui/compositor.go` (extend the import block to include `context`, `strings`, and `github.com/srajanpathak/agentctl/internal/lifecycle`):
```go
type cockpitOpts struct {
	session   string // tmux session name, e.g. "agentctl-tui-1234"
	self      string // absolute path to the agentctl binary
	stateDir  string // per-pid selection state dir
	homeDir   string // cwd for the list/detail pane processes
	masterCwd string // cwd for the master claude pane (the launching shell's dir)
}

// shquote single-quotes s so tmux's `sh -c <command>` preserves spaces.
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func listPaneCmd(self, stateDir string) string {
	return self + " tui --pane=list --state-dir=" + stateDir
}

func detailPaneCmd(self, stateDir string) string {
	return self + " tui --pane=detail --state-dir=" + stateDir
}

// runPaneCreate runs a pane-creating tmux command (-P -F '#{pane_id}') and
// returns the new pane id, so later commands target panes by stable id rather
// than by spatial index (which tmux renumbers on every split).
func runPaneCreate(ctx context.Context, run lifecycle.Runner, args ...string) (string, error) {
	out, err := run.Run(ctx, "", "tmux", args...)
	if err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, out)
	}
	return strings.TrimSpace(out), nil
}

// buildCockpit constructs the three-pane layout in a detached tmux session:
//
//	┌─ list (top-left) ─┐┌─ detail ─────┐
//	├─ master (claude) ─┤│ (full height)│
//	└───────────────────┘└──────────────┘
//
// The caller attaches afterwards. tmux is the compositor; each pane is its own
// process. NOTE: if the homeDir/stateDir/masterCwd can contain spaces, the pane
// command strings must be shquote()'d; agentctl paths are space-free in practice,
// and quoting them would change the exact strings asserted in tests.
func buildCockpit(ctx context.Context, run lifecycle.Runner, o cockpitOpts) error {
	listID, err := runPaneCreate(ctx, run,
		"new-session", "-d", "-s", o.session, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", listPaneCmd(o.self, o.stateDir))
	if err != nil {
		return err
	}
	if _, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-l", "60%", "-t", listID, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", detailPaneCmd(o.self, o.stateDir)); err != nil {
		return err
	}
	if _, err := runPaneCreate(ctx, run,
		"split-window", "-v", "-l", "50%", "-t", listID, "-c", o.masterCwd,
		"-P", "-F", "#{pane_id}", "claude"); err != nil {
		return err
	}
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-t", o.session, "mouse", "on"); err != nil {
		return fmt.Errorf("tmux set-option mouse: %w: %s", err, out)
	}
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", listID); err != nil {
		return fmt.Errorf("tmux select-pane: %w: %s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestBuildCockpitSequence|TestPaneCommandStrings' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/compositor.go internal/tui/compositor_test.go
git commit -m "feat(tui): buildCockpit tmux three-pane construction"
```

---

### Task 5: List pane model

The top-left pane: agents list + the `n`/`s`/`x`/`a`/`?` actions, writing `selection.json` whenever the selected agent changes. Reuses the existing `tickMsg`/`sessionsMsg`/command funcs and pure render helpers.

**Files:**
- Create: `internal/tui/list_pane.go`
- Test: `internal/tui/list_pane_test.go`

- [ ] **Step 1: Write the failing test**

```go
package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func lstep(m listPaneModel, msg tea.Msg) listPaneModel {
	nm, _ := m.Update(msg)
	return nm.(listPaneModel)
}

func TestListPaneWritesSelectionOnLoad(t *testing.T) {
	dir := t.TempDir()
	m := newListPane(&fakeAPI{}, dir)
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a"}, {ID: "b"}}})
	require.Equal(t, "a", readSelection(dir), "first session selected and written")
}

func TestListPaneWritesSelectionOnMove(t *testing.T) {
	dir := t.TempDir()
	m := newListPane(&fakeAPI{}, dir)
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a"}, {ID: "b"}}})
	m = lstep(m, key("down")) // move cursor to "b"
	require.Equal(t, "b", readSelection(dir))
}

func TestListPaneSpawnModal(t *testing.T) {
	m := newListPane(&fakeAPI{}, t.TempDir())
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}
```

(`fakeAPI`, `key` come from `model_test.go`, same package.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestListPane -v`
Expected: FAIL — `undefined: listPaneModel` / `newListPane`.

- [ ] **Step 3: Write minimal implementation**

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/srajanpathak/agentctl/internal/store"
)

// listPaneModel is the top-left cockpit pane: the agents list plus the
// new/send/terminate/attach actions. It owns selection: on every change it
// writes the selected id to the shared state dir for the detail pane to read.
type listPaneModel struct {
	api           api
	stateDir      string
	sessions      []*store.Session
	cursor        int
	ta            textarea.Model
	ti            textinput.Model
	mode          mode
	status        string
	connected     bool
	pendingSelect string
	lastWrote     string
	w, h          int
	ready         bool
}

func newListPane(a api, stateDir string) listPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	return listPaneModel{api: a, ta: ta, ti: ti, stateDir: stateDir, connected: true}
}

func (m listPaneModel) selectedID() string {
	if m.cursor >= 0 && m.cursor < len(m.sessions) {
		return m.sessions[m.cursor].ID
	}
	return ""
}

func (m listPaneModel) selected() *store.Session {
	if m.cursor >= 0 && m.cursor < len(m.sessions) {
		return m.sessions[m.cursor]
	}
	return nil
}

func (m listPaneModel) Init() tea.Cmd { return tea.Batch(listCmd(m.api), tick()) }

// syncSelection writes the current selection when it has changed since last write.
func (m *listPaneModel) syncSelection() {
	id := m.selectedID()
	if id != "" && id != m.lastWrote {
		_ = writeSelection(m.stateDir, id, time.Now().Unix())
		m.lastWrote = id
	}
}

func (m listPaneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.ta.SetWidth(m.w - 2)
		m.ta.SetHeight(4)
		m.ti.Width = m.w - 20
		m.ready = true
		return m, nil
	case tickMsg:
		return m, tea.Batch(listCmd(m.api), tick())
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prev := m.selectedID()
		m.sessions = msg.sessions
		m.repin(prev)
		m.syncSelection()
		return m, nil
	case spawnDoneMsg:
		if msg.err != nil {
			m.status = "spawn failed: " + msg.err.Error()
		} else {
			m.status, m.pendingSelect = "spawned "+msg.id, msg.id
		}
		return m, nil
	case inputDoneMsg:
		m.status = "sent"
		if msg.err != nil {
			m.status = "send failed: " + msg.err.Error()
		}
		return m, nil
	case cleanupDoneMsg:
		m.mode = modeNormal
		m.status = "terminated " + msg.id
		if msg.err != nil {
			m.status = "terminate failed: " + msg.err.Error()
		}
		return m, nil
	case attachDoneMsg:
		if msg.err != nil {
			m.status = "attach failed: " + msg.err.Error()
		}
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *listPaneModel) repin(prevID string) {
	want := prevID
	if m.pendingSelect != "" {
		want = m.pendingSelect
	}
	if want != "" {
		for i, s := range m.sessions {
			if s.ID == want {
				m.cursor = i
				if want == m.pendingSelect {
					m.pendingSelect = ""
				}
				return
			}
		}
	}
	if m.cursor >= len(m.sessions) {
		m.cursor = len(m.sessions) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func (m listPaneModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeNewAgent:
		switch msg.Type {
		case tea.KeyEsc:
			m.mode, _ = modeNormal, m.ta.Blur()
			return m, nil
		case tea.KeyCtrlS:
			prompt := strings.TrimSpace(m.ta.Value())
			m.mode = modeNormal
			m.ta.Blur()
			if prompt == "" {
				m.status = "prompt was empty"
				return m, nil
			}
			return m, spawnCmd(m.api, prompt)
		}
		var cmd tea.Cmd
		m.ta, cmd = m.ta.Update(msg)
		return m, cmd
	case modeSendMsg:
		switch msg.Type {
		case tea.KeyEsc:
			m.mode, _ = modeNormal, m.ti.Blur()
			return m, nil
		case tea.KeyEnter:
			text := strings.TrimSpace(m.ti.Value())
			id := m.selectedID()
			m.mode = modeNormal
			m.ti.Blur()
			if text == "" || id == "" {
				return m, nil
			}
			return m, inputCmd(m.api, id, text)
		}
		var cmd tea.Cmd
		m.ti, cmd = m.ti.Update(msg)
		return m, cmd
	case modeConfirmKill:
		switch msg.String() {
		case "esc", "n", "N":
			m.mode, m.status = modeNormal, ""
		case "y", "Y":
			if id := m.selectedID(); id != "" {
				return m, terminateCmd(m.api, id)
			}
		}
		return m, nil
	case modeHelp:
		m.mode = modeNormal
		return m, nil
	}
	// normal mode
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
			m.syncSelection()
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			m.syncSelection()
		}
	case "n":
		m.mode = modeNewAgent
		m.ta.Reset()
		m.ta.Focus()
	case "s":
		if m.selected() != nil {
			m.mode = modeSendMsg
			m.ti.Reset()
			m.ti.Focus()
		}
	case "x":
		if m.selected() != nil {
			m.mode = modeConfirmKill
		}
	case "a":
		if id := m.selectedID(); id != "" {
			return m, attachCmd(id)
		}
	case "?":
		m.mode = modeHelp
	}
	return m, nil
}

func (m listPaneModel) View() string {
	if !m.ready {
		return "loading…"
	}
	conn := stStatus.Render("live ●")
	if !m.connected {
		conn = stError.Render("reconnecting…")
	}
	header := stHeader.Render("agentctl") + "  " + conn
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	if m.mode == modeHelp {
		return header + "\n" + lipgloss.NewStyle().Width(m.w).Height(bodyH).Render(helpText())
	}
	title := fmt.Sprintf("Agents (%d)", len(m.sessions))
	body := titleBox(title, renderList(m.sessions, m.cursor, m.w-2, bodyH-2), m.w, bodyH)

	footer := stMuted.Render("n new · s send · a attach · x kill · ? help · q quit")
	if m.status != "" {
		footer = stStatus.Render(m.status)
	}
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent (ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		footer = stError.Render("Terminate " + m.selectedID() + "? y / N")
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

// RunListPane runs the top-left cockpit pane against the daemon client.
func RunListPane(a api, stateDir string) error {
	p := tea.NewProgram(newListPane(a, stateDir), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestListPane -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list_pane.go internal/tui/list_pane_test.go
git commit -m "feat(tui): list pane model (writes selection, owns actions)"
```

---

### Task 6: Detail pane model

The full-height right pane: read-only viewport of the selected agent's output, picking up the selection from `selection.json` on each tick.

**Files:**
- Create: `internal/tui/detail_pane.go`
- Test: `internal/tui/detail_pane_test.go`

- [ ] **Step 1: Write the failing test**

```go
package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

func dstep(m detailPaneModel, msg tea.Msg) detailPaneModel {
	nm, _ := m.Update(msg)
	return nm.(detailPaneModel)
}

func TestDetailPaneReadsSelectionOnTick(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeSelection(dir, "agent-x", 1))
	m := newDetailPane(&fakeAPI{}, dir)
	m = dstep(m, tickMsg{}) // tick re-reads the selection file
	require.Equal(t, "agent-x", m.selID)
}

func TestDetailPaneShowsOutputForSelection(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeSelection(dir, "agent-x", 1))
	m := newDetailPane(&fakeAPI{}, dir)
	m = dstep(m, tickMsg{})
	m = dstep(m, sessionsMsg{sessions: []*store.Session{{ID: "agent-x", Subject: "doing things"}}})
	m = dstep(m, outputMsg{id: "agent-x", text: "hello from agent"})
	require.Contains(t, m.output, "hello from agent")
	require.NotNil(t, m.sess)
	require.Equal(t, "agent-x", m.sess.ID)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestDetailPane -v`
Expected: FAIL — `undefined: detailPaneModel` / `newDetailPane`.

- [ ] **Step 3: Write minimal implementation**

```go
package tui

import (
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/srajanpathak/agentctl/internal/store"
)

// detailPaneModel is the full-height right cockpit pane: a read-only viewport of
// the selected agent's output. Selection comes from the shared state file
// (written by the list pane), re-read on every tick.
type detailPaneModel struct {
	api       api
	stateDir  string
	selID     string
	sess      *store.Session
	sessions  []*store.Session
	output    string
	vp        viewport.Model
	connected bool
	w, h      int
	ready     bool
}

func newDetailPane(a api, stateDir string) detailPaneModel {
	return detailPaneModel{api: a, stateDir: stateDir, connected: true}
}

func (m detailPaneModel) Init() tea.Cmd { return tick() }

func (m *detailPaneModel) findSelected() {
	m.sess = nil
	for _, s := range m.sessions {
		if s.ID == m.selID {
			m.sess = s
			return
		}
	}
}

func (m detailPaneModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.vp.Width = m.w - 2
		vpH := (m.h - 2) - 2 - detailChrome
		if vpH < 1 {
			vpH = 1
		}
		m.vp.Height = vpH
		m.ready = true
		return m, nil
	case tickMsg:
		m.selID = readSelection(m.stateDir)
		m.findSelected()
		return m, tea.Batch(listCmd(m.api), outputCmd(m.api, m.selID), tick())
	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		m.sessions = msg.sessions
		m.findSelected()
		return m, nil
	case outputMsg:
		if msg.id == m.selID {
			m.output = msg.text
			m.vp.SetContent(msg.text)
			m.vp.GotoBottom()
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg) // PgUp/PgDn/arrows scroll the output
		return m, cmd
	}
	return m, nil
}

func (m detailPaneModel) View() string {
	if !m.ready {
		return "loading…"
	}
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	title := m.selID
	if title == "" {
		title = "—"
	}
	return titleBox(title, renderDetail(m.sess, m.vp, false, m.w-2), m.w, bodyH)
}

// RunDetailPane runs the full-height right cockpit pane against the daemon client.
func RunDetailPane(a api, stateDir string) error {
	p := tea.NewProgram(newDetailPane(a, stateDir), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestDetailPane -v`
Expected: PASS (2 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/detail_pane.go internal/tui/detail_pane_test.go
git commit -m "feat(tui): detail pane model (reads selection, read-only output)"
```

---

### Task 7: RunCockpit (build + attach + cleanup) and state-dir base

Wire the pieces into a launcher entrypoint, including best-effort cleanup of this run's state dir and stale dirs from dead prior runs.

**Files:**
- Modify: `internal/tui/compositor.go`
- Test: `internal/tui/compositor_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/tui/compositor_test.go`:
```go
import "os" // add to the import block

func TestCockpitBaseDirPrefersXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	require.Equal(t, filepath.Join("/run/user/1000", "agentctl"), cockpitBaseDir())
	t.Setenv("XDG_RUNTIME_DIR", "")
	require.Equal(t, filepath.Join(os.TempDir(), "agentctl"), cockpitBaseDir())
}

func TestCleanStaleStateDirsRemovesDeadPidDirs(t *testing.T) {
	base := t.TempDir()
	dead := cockpitStateDir(base, 999999) // a pid extremely unlikely to be alive
	require.NoError(t, os.MkdirAll(dead, 0o700))
	keep := cockpitStateDir(base, os.Getpid()) // our own pid: alive, must survive
	require.NoError(t, os.MkdirAll(keep, 0o700))

	cleanStaleStateDirs(base)

	_, err := os.Stat(dead)
	require.True(t, os.IsNotExist(err), "dead pid dir should be removed")
	_, err = os.Stat(keep)
	require.NoError(t, err, "live pid dir should survive")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestCockpitBaseDir|TestCleanStale' -v`
Expected: FAIL — `undefined: cockpitBaseDir` / `cleanStaleStateDirs`.

- [ ] **Step 3: Write minimal implementation**

Add to `internal/tui/compositor.go` (extend imports with `os`, `os/exec` is already present, `strconv`, `syscall`):
```go
// cockpitBaseDir is the parent of all per-pid cockpit state dirs.
func cockpitBaseDir() string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "agentctl")
	}
	return filepath.Join(os.TempDir(), "agentctl")
}

// pidAlive reports whether a process with pid exists (signal 0 probe).
func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// cleanStaleStateDirs removes tui-<pid> dirs under base whose pid is no longer
// alive. Best-effort: errors are ignored (a leftover dir is harmless).
func cleanStaleStateDirs(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "tui-") {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "tui-"))
		if err != nil {
			continue
		}
		if !pidAlive(pid) {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}

// RunCockpit builds the tmux cockpit for this process and attaches to it,
// blocking until the user detaches/quits. masterCwd is the launching shell's
// directory (where the master claude pane runs). It cleans up this run's state
// dir on exit and sweeps stale dirs from dead prior runs on entry.
func RunCockpit(a api, self, masterCwd string) error {
	_ = a // the panes hold their own clients; reserved for future inline checks
	pid := os.Getpid()
	base := cockpitBaseDir()
	cleanStaleStateDirs(base)

	stateDir := cockpitStateDir(base, pid)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	defer os.RemoveAll(stateDir)

	home, _ := os.UserHomeDir()
	o := cockpitOpts{
		session:   cockpitSession(pid),
		self:      self,
		stateDir:  stateDir,
		homeDir:   home,
		masterCwd: masterCwd,
	}
	if err := buildCockpit(context.Background(), lifecycle.ExecRunner{}, o); err != nil {
		// Tear down a half-built session so we never leave an orphan.
		_, _ = lifecycle.ExecRunner{}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)
		return err
	}

	attach := exec.Command("tmux", "attach", "-t", o.session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	return attach.Run()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestCockpitBaseDir|TestCleanStale' -v`
Expected: PASS.

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/tui/ -v`
Expected: PASS (all tasks' tests green together).

- [ ] **Step 6: Commit**

```bash
git add internal/tui/compositor.go internal/tui/compositor_test.go
git commit -m "feat(tui): RunCockpit build+attach with state-dir cleanup"
```

---

### Task 8: CLI wiring

Dispatch `agentctl tui` to the right path: internal pane apps, classic, or the cockpit; and make bare `agentctl` launch the cockpit too.

**Files:**
- Modify: `internal/cli/tui.go`
- Modify: `internal/cli/root.go`

- [ ] **Step 1: Rewrite `internal/cli/tui.go`**

Replace the whole file with:
```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/tui"
)

func newTUICmd() *cobra.Command {
	var classic bool
	var pane, stateDir string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := clientFor(cmd)
			switch pane {
			case "list":
				return tui.RunListPane(a, stateDir)
			case "detail":
				return tui.RunDetailPane(a, stateDir)
			case "":
				return runCockpitOrClassic(cmd, a, classic)
			default:
				return fmt.Errorf("unknown --pane %q (want list|detail)", pane)
			}
		},
	}
	cmd.Flags().BoolVar(&classic, "classic", false, "use the legacy single-pane TUI (no tmux)")
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (list|detail)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "", "internal: cockpit shared state dir")
	_ = cmd.Flags().MarkHidden("pane")
	_ = cmd.Flags().MarkHidden("state-dir")
	return cmd
}

// runCockpitOrClassic launches the tmux cockpit, or the legacy single-pane TUI
// when --classic is set, tmux is unavailable, or we are already inside tmux.
func runCockpitOrClassic(cmd *cobra.Command, a *clientAlias, classic bool) error {
	if tui.ChooseClassic(classic, tui.TmuxAvailable(), os.Getenv("TMUX") != "") {
		return tui.Run(a)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate agentctl binary: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}
	return tui.RunCockpit(a, self, cwd)
}
```

> **NOTE on `*clientAlias`:** `clientFor` returns `*client.Client`. Replace the `*clientAlias` parameter type with `*client.Client` and add the import `"github.com/srajanpathak/agentctl/internal/client"`. (`clientAlias` is named here only to avoid implying a new type — use the real `*client.Client`.)

- [ ] **Step 2: Export the mode helpers from the tui package**

`chooseClassic` and `tmuxAvailable` are unexported. Add thin exported wrappers in `internal/tui/compositor.go` so the CLI can call them:
```go
// ChooseClassic is the exported wrapper for chooseClassic (used by the CLI).
func ChooseClassic(classicFlag, tmuxAvailable, insideTmux bool) bool {
	return chooseClassic(classicFlag, tmuxAvailable, insideTmux)
}

// TmuxAvailable reports whether the tmux binary is on PATH (used by the CLI).
func TmuxAvailable() bool { return tmuxAvailable() }
```

- [ ] **Step 3: Point bare `agentctl` at the cockpit**

In `internal/cli/root.go`, replace:
```go
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return tui.Run(clientFor(cmd))
	}
```
with:
```go
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return runCockpitOrClassic(cmd, clientFor(cmd), false)
	}
```

- [ ] **Step 4: Build and run all tests**

Run: `go build ./... && go test ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/tui.go internal/cli/root.go internal/tui/compositor.go
git commit -m "feat(tui): CLI dispatch for cockpit/panes/classic"
```

---

### Task 9: Manual integration verification + docs

No automated test can prove the tmux composition end-to-end; verify it by hand, then document.

- [ ] **Step 1: Build and install the binary locally**

Run: `go build -o "$(go env GOPATH)/bin/agentctl" ./cmd/agentctl`
Expected: builds; `agentctl` on PATH resolves to the new build (`which agentctl`).

- [ ] **Step 2: Verify tmux version supports the split flags**

Run: `tmux -V`
Expected: `tmux 3.1` or newer. If older, apply the `-l N%` → `-p N` change noted in Task 4 and rebuild.

- [ ] **Step 3: Launch the cockpit (daemon must be running)**

Run (from a normal shell, NOT inside tmux): `agentctl daemon &` then `agentctl tui`
Expected: a tmux session with three panes — list (top-left), `claude` prompt (bottom-left), detail (full-height right). Header reads `agentctl  live ●`.

- [ ] **Step 4: Verify selection sync**

In the list pane, move the cursor with `j`/`k` across agents.
Expected: the right detail pane updates within ~1s to show the highlighted agent's output/meta. Confirm the state file exists: `cat "${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/agentctl/tui-"*/selection.json`.

- [ ] **Step 5: Verify the master pane can drive the fleet**

Click/select the bottom-left `claude` pane (tmux: `Ctrl-b ↓`). Type: `list my agents and tell me which one is idle the longest`.
Expected: the master Claude responds using the agentctl MCP tools (or, if MCP isn't registered, the `agentctl` CLI). Spawning/terminating from the master is reflected in the list pane within ~1s.

- [ ] **Step 6: Verify quit + classic fallback**

In the list pane press `q`.
Expected: the whole tmux session is torn down (master dies with it), shell returns, and the state dir is gone. Then run `agentctl tui --classic` and confirm the legacy single-pane TUI still works. Run `agentctl tui` from *inside* a tmux session and confirm it falls back to classic.

- [ ] **Step 7: Update `docs/USAGE.md`**

In `internal/.../docs/USAGE.md`, under the TUI section, document:
- `agentctl tui` now opens a tmux-composited cockpit: agents list (top-left), an embedded master `claude` (bottom-left) wired to the agentctl MCP, and the agent detail viewer (full-height right).
- Selection in the list drives the detail pane; the master can manage the whole fleet.
- The master is ephemeral (dies on quit); see the design doc's future-enhancement section for persistence.
- `agentctl tui --classic` runs the legacy single-pane TUI; the cockpit auto-falls-back to classic when tmux is unavailable or when launched inside an existing tmux session.

- [ ] **Step 8: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document the tmux-composited TUI cockpit + master pane"
```

---

## Self-Review

**Spec coverage:**
- Tmux compositor + launcher → Tasks 3, 4, 7, 8. ✅
- `--pane=list|detail` internal subcommands → Tasks 5, 6, 8. ✅
- Embedded master claude (PTY) wired to MCP → Task 4 (`claude` pane) + Task 9 step 5; MCP via user-scope registration (no per-launch wiring needed). ✅
- `selection.json` local sync, polled on the existing tick → Tasks 1, 5 (write), 6 (read on tick). ✅
- Daemon stays source of truth; panes poll independently → Tasks 5, 6 reuse `listCmd`/`outputCmd`. ✅
- Per-pid cockpit (two shells = two cockpits) → Task 3 (`cockpitSession`/`cockpitStateDir` by pid), Task 7. ✅
- `--classic` fallback (tmux missing / inside tmux) → Tasks 3, 8. ✅
- Ephemeral master dies on quit; state-dir cleanup; stale-dir sweep → Task 7. ✅
- Error handling: tmux missing → classic; half-built session torn down; corrupt/missing selection → empty → detail "—"; claude missing → only master pane affected → Tasks 1, 3, 6, 7. ✅
- Testing: pure reducers + selection round-trip + tmux sequence via FakeRunner + manual integration → Tasks 1, 4, 5, 6, 7, 9. ✅
- Decompose the monolith into ListModel/DetailModel + shared render funcs → Tasks 2, 5, 6. ✅

**Placeholder scan:** No TBD/TODO; every code step shows full code; the one ambiguity (tmux split flag version, `*client.Client` type) is called out with an explicit instruction, not left vague.

**Type consistency:** `writeSelection(stateDir, id string, ts int64)` / `readSelection(stateDir) string` used consistently (Tasks 1, 5, 6). `cockpitOpts` fields (`session/self/stateDir/homeDir/masterCwd`) match between Tasks 4 and 7. `renderList(sessions, cursor, width, height)` and `renderDetail(sess, vp, outputFocused, width)` signatures match between Task 2 (definition) and Tasks 5/6 (callers) and `view.go`. `RunListPane`/`RunDetailPane`/`RunCockpit`/`ChooseClassic`/`TmuxAvailable` exported names match between the tui package and the CLI (Task 8). `lifecycle.Runner`/`lifecycle.ExecRunner`/`lifecycle.FakeRunner` used as in `internal/lifecycle`.

## Out of scope (north-star roadmap)

Persistent master, live worktree diff view, multi-select bulk actions, logs/output follow mode, file tree, command palette, configurable layouts. See the design doc.
