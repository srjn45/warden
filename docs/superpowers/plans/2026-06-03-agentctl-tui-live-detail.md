# TUI live detail pane (interactive agent terminal) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the cockpit's read-only right pane into a live, interactive terminal of the selected agent (typed into directly), opened on Enter from the list pane; remove the empty history block; add prefix-less Alt+Arrow pane navigation.

**Architecture:** The detail pane is no longer a bubbletea viewer driven by a `selection.json` file. Instead `buildCockpit` creates the right pane as a placeholder process and hands its tmux pane id to the list pane (`--detail-pane=%N`); pressing Enter in the list pane runs `tmux respawn-pane -k -t <detailPane> "env -u TMUX tmux attach -t <agent>"`, replacing the pane with a live nested attach to that agent's session. The read-only detail viewer, `selection.json`, and the per-pid state-dir subsystem are deleted.

**Tech Stack:** Go, charmbracelet/bubbletea + lipgloss, tmux (compositor), cobra, testify. Builds on the cockpit from `2026-06-03-agentctl-tui-master-pane-design.md`.

**Spec:** `docs/superpowers/specs/2026-06-03-agentctl-tui-live-detail-design.md`

---

## File Structure

- `internal/tui/detail.go` — `renderDetail` loses the history block (still used by classic `--classic`). `renderHistory` deleted.
- `internal/tui/view.go` — `detailChrome` recomputed (13 → 5).
- `internal/tui/list_pane.go` — gains `detailPane` field + Enter handler + `respawnDetailArgs`/`openInDetailCmd`; loses selection writing (`stateDir`, `lastWrote`, `syncSelection`).
- `internal/tui/compositor.go` — `buildCockpit` rebuilt (placeholder detail pane, capture its id, pass to list, `remain-on-exit`, Alt+Arrow binds); `RunCockpit` loses the state-dir subsystem; `detailPaneCmd`, `cockpitStateDir`, `cockpitBaseDir`, `pidAlive`, `cleanStaleStateDirs` removed.
- `internal/cli/tui.go` — `--state-dir` + `--pane=detail` removed; `--detail-pane` added.
- **Deleted:** `internal/tui/detail_pane.go`, `internal/tui/detail_pane_test.go`, `internal/tui/selection.go`, `internal/tui/selection_test.go`.
- `docs/USAGE.md` — document the live detail pane, Enter-to-open, Alt+Arrow.

## Conventions

- Tests use `testify/require`; bubbletea models are pure reducers tested by applying messages. Helpers `fakeAPI`, `key(...)`, `lstep(...)` already exist (`model_test.go`, `list_pane_test.go`).
- tmux driven through `lifecycle.Runner`; `lifecycle.FakeRunner` records `.Calls[i].Argv` and matches `.Responses` by `"name arg1 arg2 …"`.
- Run `go test ./internal/tui/ -run <name> -v`, `go build ./...`, `go test ./...`. Commit after each green task.

---

### Task 1: Remove the history block from the detail view

**Files:**
- Modify: `internal/tui/detail.go`
- Modify: `internal/tui/view.go`

- [ ] **Step 1: Edit `renderDetail` and delete `renderHistory` in `internal/tui/detail.go`**

Replace the body of `renderDetail` so it no longer appends history. Change:
```go
	outTitle := stPaneTitle.Render("─ output ") + stMuted.Render(focusHint(outputFocused))
	out := vp.View()

	hist := stPaneTitle.Render("─ history ─") + "\n" + renderHistory(s, 6)

	return strings.Join([]string{head, meta, subj, "", outTitle, out, "", hist}, "\n")
}
```
to:
```go
	outTitle := stPaneTitle.Render("─ output ") + stMuted.Render(focusHint(outputFocused))
	out := vp.View()

	return strings.Join([]string{head, meta, subj, "", outTitle, out}, "\n")
}
```
Then delete the entire `renderHistory` function:
```go
func renderHistory(s *store.Session, n int) string {
	ev := s.Events
	if len(ev) == 0 {
		return stMuted.Render("no events yet")
	}
	start := 0
	if len(ev) > n {
		start = len(ev) - n
	}
	var b strings.Builder
	for _, e := range ev[start:] {
		fmt.Fprintf(&b, "%s  %-14s %s\n", e.TS.Format("15:04:05"), e.Type, trunc(e.Detail, 40))
	}
	return b.String()
}
```
Now `fmt` is unused in this file — change the import block from:
```go
import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/srajanpathak/agentctl/internal/store"
)
```
to:
```go
import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/srajanpathak/agentctl/internal/store"
)
```

- [ ] **Step 2: Recompute `detailChrome` in `internal/tui/view.go`**

Change:
```go
// detailChrome is the number of non-viewport lines renderDetail emits
// (head, dir, subject, blank, output-title, blank, history≈7).
const detailChrome = 13
```
to:
```go
// detailChrome is the number of non-viewport lines renderDetail emits
// (head, dir, subject, blank, output-title).
const detailChrome = 5
```

- [ ] **Step 3: Build + test**

Run: `go build ./... && go test ./internal/tui/ -v 2>&1 | tail -6`
Expected: clean build; all tests PASS (no test asserts the history text, so nothing breaks). `renderDetail` is still referenced by classic `Model` and by the (not-yet-deleted) detail pane — both compile.

- [ ] **Step 4: Commit**

```bash
git add internal/tui/detail.go internal/tui/view.go
git commit -m "feat(tui): drop empty history block from detail view"
```

---

### Task 2: Add the detail-pane respawn primitive (pure, not yet wired)

Adds the helper the list pane will use to open an agent into the detail pane. It is unused until Task 3 — that's fine (package-level functions may be unused).

**Files:**
- Modify: `internal/tui/list_pane.go`
- Test: `internal/tui/list_pane_test.go`

- [ ] **Step 1: Write the failing test (append to `internal/tui/list_pane_test.go`)**

```go
func TestRespawnDetailArgs(t *testing.T) {
	require.Equal(t,
		[]string{"respawn-pane", "-k", "-t", "%9", "env -u TMUX tmux attach -t agent-4f98"},
		respawnDetailArgs("%9", "agent-4f98"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestRespawnDetailArgs -v`
Expected: FAIL — `undefined: respawnDetailArgs`.

- [ ] **Step 3: Add the helpers near the bottom of `internal/tui/list_pane.go`** (above `RunListPane`)

```go
// respawnDetailArgs builds the tmux args that replace the detail pane's process
// with a live (nested) attach to the given agent's tmux session. `env -u TMUX`
// lets tmux attach from inside tmux; `respawn-pane -k` kills the placeholder
// (or the previously-opened agent) first.
func respawnDetailArgs(detailPane, agentSession string) []string {
	return []string{"respawn-pane", "-k", "-t", detailPane,
		"env -u TMUX tmux attach -t " + agentSession}
}

// openInDetailCmd opens the given agent's live session in the detail pane.
func openInDetailCmd(detailPane, agentSession string) tea.Cmd {
	return func() tea.Msg {
		return attachDoneMsg{err: exec.Command("tmux", respawnDetailArgs(detailPane, agentSession)...).Run()}
	}
}
```
(`exec` and `tea` are already imported in `list_pane.go`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run TestRespawnDetailArgs -v` → PASS. Then `go build ./...` → clean.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list_pane.go internal/tui/list_pane_test.go
git commit -m "feat(tui): respawn-pane helper to open an agent into the detail pane"
```

---

### Task 3: The pivot — list pane drives the detail pane on Enter; cockpit rebuilt; selection writing removed

This is the coordinated change across the list pane, the CLI, and the compositor. It keeps the tree compiling and the cockpit runtime-coherent. The now-orphaned `detail_pane.go` / `selection.go` still compile and are deleted in Task 4.

**Files:**
- Modify: `internal/tui/list_pane.go`
- Modify: `internal/tui/list_pane_test.go`
- Modify: `internal/cli/tui.go`
- Modify: `internal/tui/compositor.go`
- Modify: `internal/tui/compositor_test.go`

- [ ] **Step 1: Rework `listPaneModel` in `internal/tui/list_pane.go`**

(a) Struct: replace `stateDir` and `lastWrote` with `detailPane`:
```go
type listPaneModel struct {
	api           api
	detailPane    string // tmux pane id of the detail pane this list drives
	sessions      []*store.Session
	cursor        int
	ta            textarea.Model
	ti            textinput.Model
	mode          mode
	status        string
	connected     bool
	pendingSelect string
	w, h          int
	ready         bool
}
```

(b) Constructor:
```go
func newListPane(a api, detailPane string) listPaneModel {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	return listPaneModel{api: a, ta: ta, ti: ti, detailPane: detailPane, connected: true}
}
```

(c) Delete the `syncSelection` method entirely:
```go
// syncSelection writes the current selection when it has changed since last write.
func (m *listPaneModel) syncSelection() {
	id := m.selectedID()
	if id != "" && id != m.lastWrote {
		_ = writeSelection(m.stateDir, id, time.Now().Unix())
		m.lastWrote = id
	}
}
```

(d) In `Update`, the `sessionsMsg` case: remove the `m.syncSelection()` call. Change:
```go
		m.connected = true
		prev := m.selectedID()
		m.sessions = msg.sessions
		m.repin(prev)
		m.syncSelection()
		return m, nil
```
to:
```go
		m.connected = true
		prev := m.selectedID()
		m.sessions = msg.sessions
		m.repin(prev)
		return m, nil
```

(e) In `handleKey` normal mode: remove the two `m.syncSelection()` calls in the `down/j` and `up/k` cases, and add an `enter` case. Change:
```go
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
```
to:
```go
	case "enter":
		if s := m.selected(); s != nil && m.detailPane != "" {
			return m, openInDetailCmd(m.detailPane, s.TmuxSession)
		}
	case "down", "j":
		if m.cursor < len(m.sessions)-1 {
			m.cursor++
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
```

(f) Update the footer hint string in `View()`. Change:
```go
	footer := stMuted.Render("n new · s send · a attach · x kill · ? help · q quit")
```
to:
```go
	footer := stMuted.Render("enter open · n new · s send · a attach · x kill · ? help · q quit")
```

(g) `RunListPane` signature:
```go
// RunListPane runs the top-left cockpit pane; detailPane is the tmux id of the
// detail pane it drives (opened on Enter).
func RunListPane(a api, detailPane string) error {
	p := tea.NewProgram(newListPane(a, detailPane), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
```

(h) Remove the now-unused `time` import from `list_pane.go` (it was only used by `syncSelection`). The import block becomes:
```go
import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/srajanpathak/agentctl/internal/store"
)
```

- [ ] **Step 2: Update `internal/tui/list_pane_test.go`**

Delete the two selection tests (selection no longer exists):
```go
func TestListPaneWritesSelectionOnLoad(t *testing.T) { ... }
func TestListPaneWritesSelectionOnMove(t *testing.T) { ... }
```
Update `TestListPaneSpawnModal` to the new constructor signature (replace `t.TempDir()` arg with a pane id string) and add an Enter test. The relevant tests become:
```go
func TestListPaneSpawnModal(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	m = lstep(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestListPaneEnterOpensDetail(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	m = lstep(m, sessionsMsg{sessions: []*store.Session{{ID: "a", TmuxSession: "a"}}})
	_, cmd := m.Update(key("enter"))
	require.NotNil(t, cmd, "Enter on a selected agent opens it in the detail pane")
}

func TestListPaneEnterNoopWithoutSelection(t *testing.T) {
	m := newListPane(&fakeAPI{}, "%9")
	_, cmd := m.Update(key("enter")) // no sessions loaded
	require.Nil(t, cmd, "Enter with no selection does nothing")
}
```
(If `lstep` is defined in this file it stays; `key` and `fakeAPI` come from `model_test.go`.)

- [ ] **Step 3: Update `internal/cli/tui.go`** — replace `--state-dir`/`--pane=detail` with `--detail-pane`

Change the command body and flags:
```go
func newTUICmd() *cobra.Command {
	var classic bool
	var pane, detailPane string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := clientFor(cmd)
			switch pane {
			case "list":
				return tui.RunListPane(a, detailPane)
			case "":
				return runCockpitOrClassic(a, classic)
			default:
				return fmt.Errorf("unknown --pane %q (want list)", pane)
			}
		},
	}
	cmd.Flags().BoolVar(&classic, "classic", false, "use the legacy single-pane TUI (no tmux)")
	cmd.Flags().StringVar(&pane, "pane", "", "internal: render a single cockpit pane (list)")
	cmd.Flags().StringVar(&detailPane, "detail-pane", "", "internal: tmux id of the detail pane the list drives")
	_ = cmd.Flags().MarkHidden("pane")
	_ = cmd.Flags().MarkHidden("detail-pane")
	return cmd
}
```
(`runCockpitOrClassic` is unchanged. The `tui.RunDetailPane` reference is gone, so after Task 4 deletes that function nothing breaks.)

- [ ] **Step 4: Rebuild `buildCockpit` and trim `compositor.go`**

In `internal/tui/compositor.go`:

(a) `cockpitOpts` — drop `stateDir`:
```go
type cockpitOpts struct {
	session   string // tmux session name, e.g. "agentctl-tui-1234"
	self      string // absolute path to the agentctl binary
	homeDir   string // cwd for the list pane process
	masterCwd string // cwd for the master claude pane (the launching shell's dir)
}
```

(b) Replace `listPaneCmd` and delete `detailPaneCmd`. New:
```go
// listPaneCmd is the shell command tmux runs for the top-left list pane. It is
// told the detail pane's id so it can drive (respawn) it when the user opens an
// agent with Enter.
func listPaneCmd(self, detailPane string) string {
	return self + " tui --pane=list --detail-pane=" + detailPane
}

// detailPlaceholderCmd keeps the right pane alive showing a hint until the user
// opens an agent into it. `exec sleep` so the process is cleanly replaceable by
// `respawn-pane`.
func detailPlaceholderCmd() string {
	return `sh -c 'printf "Select an agent and press Enter to open it here.\n"; exec sleep 2147483647'`
}
```
Delete the old `detailPaneCmd`:
```go
func detailPaneCmd(self, stateDir string) string {
	return self + " tui --pane=detail --state-dir=" + stateDir
}
```

(c) Replace the whole `buildCockpit` function:
```go
// buildCockpit constructs the three-pane layout in a detached tmux session:
//
//	┌─ list (top-left) ─┐┌─ detail ─────┐
//	├─ master (claude) ─┤│ (full height)│
//	└───────────────────┘└──────────────┘
//
// Panes are created right-to-left so the list pane (created last) can be handed
// the detail pane's stable id (--detail-pane) and drive it via respawn-pane. The
// detail pane starts as a placeholder; the list pane opens an agent into it on
// Enter. The caller attaches afterwards.
func buildCockpit(ctx context.Context, run lifecycle.Runner, o cockpitOpts) error {
	// 1. Detail pane fills the window initially (placeholder); capture its id.
	detailID, err := runPaneCreate(ctx, run,
		"new-session", "-d", "-s", o.session, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", detailPlaceholderCmd())
	if err != nil {
		return err
	}
	// 2. Master claude to the LEFT of detail (-b), 40% width, in the launch dir.
	masterID, err := runPaneCreate(ctx, run,
		"split-window", "-h", "-b", "-l", "40%", "-t", detailID, "-c", o.masterCwd,
		"-P", "-F", "#{pane_id}", "claude")
	if err != nil {
		return err
	}
	// 3. List pane ABOVE master (-b), 50% of the left column; it gets detailID.
	listID, err := runPaneCreate(ctx, run,
		"split-window", "-v", "-b", "-l", "50%", "-t", masterID, "-c", o.homeDir,
		"-P", "-F", "#{pane_id}", listPaneCmd(o.self, detailID))
	if err != nil {
		return err
	}
	// 4. Keep the detail pane (showing [exited]) instead of collapsing the layout
	//    when an opened agent's attach exits.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-p", "-t", detailID, "remain-on-exit", "on"); err != nil {
		return fmt.Errorf("tmux set-option remain-on-exit: %w: %s", err, out)
	}
	// 5. Mouse + prefix-less Alt+Arrow pane navigation.
	if out, err := run.Run(ctx, "", "tmux", "set-option", "-t", o.session, "mouse", "on"); err != nil {
		return fmt.Errorf("tmux set-option mouse: %w: %s", err, out)
	}
	for _, b := range [][2]string{{"M-Left", "-L"}, {"M-Right", "-R"}, {"M-Up", "-U"}, {"M-Down", "-D"}} {
		if out, err := run.Run(ctx, "", "tmux", "bind-key", "-n", b[0], "select-pane", b[1]); err != nil {
			return fmt.Errorf("tmux bind-key %s: %w: %s", b[0], err, out)
		}
	}
	// 6. Focus the list pane.
	if out, err := run.Run(ctx, "", "tmux", "select-pane", "-t", listID); err != nil {
		return fmt.Errorf("tmux select-pane: %w: %s", err, out)
	}
	return nil
}
```

(d) Delete the state-dir subsystem functions entirely: `cockpitBaseDir`, `cockpitStateDir`, `pidAlive`, `cleanStaleStateDirs`. (Keep `cockpitSession`, `shquote`, `runPaneCreate`, `chooseClassic`, `tmuxAvailable`, `ChooseClassic`, `TmuxAvailable`.)

(e) Simplify `RunCockpit` (no state dir):
```go
// RunCockpit builds the tmux cockpit for this process and attaches to it,
// blocking until the user detaches/quits. masterCwd is the launching shell's
// directory (where the master claude pane runs).
func RunCockpit(a api, self, masterCwd string) error {
	_ = a // the panes hold their own clients; reserved for future inline checks
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}
	o := cockpitOpts{
		session:   cockpitSession(os.Getpid()),
		self:      self,
		homeDir:   home,
		masterCwd: masterCwd,
	}
	if err := buildCockpit(context.Background(), lifecycle.ExecRunner{}, o); err != nil {
		// Tear down a half-built session so we never leave an orphan.
		_, _ = lifecycle.ExecRunner{}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)
		return err
	}
	// Always tear the session down on return (covers detach, where `tmux attach`
	// returns 0 while the session keeps running). kill-session on a gone session
	// is a harmless ignored error.
	defer lifecycle.ExecRunner{}.Run(context.Background(), "", "tmux", "kill-session", "-t", o.session)

	attach := exec.Command("tmux", "attach", "-t", o.session)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	return attach.Run()
}
```

(f) Fix the import block of `compositor.go` — `errors`, `path/filepath`, `strconv`, `syscall` are no longer used. It becomes:
```go
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
)
```

- [ ] **Step 5: Update `internal/tui/compositor_test.go`**

(a) Delete `TestCockpitBaseDirPrefersXDG` and `TestCleanStaleStateDirsRemovesDeadPidDirs` (the functions they test are gone).

(b) `TestCockpitNames` referenced the deleted `cockpitStateDir` — reduce it to just the session-name assertion:
```go
func TestCockpitNames(t *testing.T) {
	require.Equal(t, "agentctl-tui-1234", cockpitSession(1234))
}
```

(c) Replace `TestBuildCockpitSequence` and `TestPaneCommandStrings` with versions for the new right-to-left sequence:
```go
func TestBuildCockpitSequence(t *testing.T) {
	fr := &lifecycle.FakeRunner{Responses: map[string]lifecycle.FakeResp{}}
	fr.Responses["tmux new-session -d -s S -c /home -P -F #{pane_id} "+detailPlaceholderCmd()] = lifecycle.FakeResp{Out: "%0\n"}
	fr.Responses["tmux split-window -h -b -l 40% -t %0 -c /work -P -F #{pane_id} claude"] = lifecycle.FakeResp{Out: "%1\n"}
	fr.Responses["tmux split-window -v -b -l 50% -t %1 -c /home -P -F #{pane_id} "+listPaneCmd("/bin/agentctl", "%0")] = lifecycle.FakeResp{Out: "%2\n"}

	o := cockpitOpts{session: "S", self: "/bin/agentctl", homeDir: "/home", masterCwd: "/work"}
	require.NoError(t, buildCockpit(context.Background(), fr, o))
	require.Len(t, fr.Calls, 10, "unexpected number of tmux calls")

	// 1) detail placeholder (fills window)
	require.Equal(t, []string{"tmux", "new-session", "-d", "-s", "S", "-c", "/home", "-P", "-F", "#{pane_id}", detailPlaceholderCmd()}, fr.Calls[0].Argv)
	// 2) master claude to the left of detail, 40%
	require.Equal(t, []string{"tmux", "split-window", "-h", "-b", "-l", "40%", "-t", "%0", "-c", "/work", "-P", "-F", "#{pane_id}", "claude"}, fr.Calls[1].Argv)
	// 3) list pane above master, 50%, handed the detail pane id
	require.Equal(t, []string{"tmux", "split-window", "-v", "-b", "-l", "50%", "-t", "%1", "-c", "/home", "-P", "-F", "#{pane_id}", "/bin/agentctl tui --pane=list --detail-pane=%0"}, fr.Calls[2].Argv)
	// 4) detail pane keeps its slot when an attach exits
	require.Equal(t, []string{"tmux", "set-option", "-p", "-t", "%0", "remain-on-exit", "on"}, fr.Calls[3].Argv)
	// 5) mouse on
	require.Equal(t, []string{"tmux", "set-option", "-t", "S", "mouse", "on"}, fr.Calls[4].Argv)
	// 6-9) prefix-less Alt+Arrow pane navigation
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Left", "select-pane", "-L"}, fr.Calls[5].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Right", "select-pane", "-R"}, fr.Calls[6].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Up", "select-pane", "-U"}, fr.Calls[7].Argv)
	require.Equal(t, []string{"tmux", "bind-key", "-n", "M-Down", "select-pane", "-D"}, fr.Calls[8].Argv)
	// 10) focus the list pane
	require.Equal(t, []string{"tmux", "select-pane", "-t", "%2"}, fr.Calls[9].Argv)
}

func TestPaneCommandStrings(t *testing.T) {
	require.Equal(t, "/bin/agentctl tui --pane=list --detail-pane=%0", listPaneCmd("/bin/agentctl", "%0"))
	require.Contains(t, detailPlaceholderCmd(), "press Enter to open")
	require.True(t, strings.Contains(shquote("a b"), "'a b'"))
}
```
(If the test file no longer imports `os`/`path/filepath` after deleting the two tests, remove those imports. Keep `context`, `strings`, `testing`, `require`, `lifecycle`.)

- [ ] **Step 6: Build + full test**

Run: `go build ./... && go test ./... 2>&1 | tail -15`
Expected: clean build; all packages PASS. `internal/tui/detail_pane.go` and `internal/tui/selection.go` are now dead (the list pane no longer writes selection, `buildCockpit` no longer launches `--pane=detail`, the CLI no longer dispatches it) but still compile, so the tree is green.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/list_pane.go internal/tui/list_pane_test.go internal/cli/tui.go internal/tui/compositor.go internal/tui/compositor_test.go
git commit -m "feat(tui): live detail pane — list opens agents via respawn-pane; Alt+Arrow nav"
```

---

### Task 4: Delete the dead detail-viewer + selection code

After Task 3 nothing references these. Removing them together compiles.

**Files:**
- Delete: `internal/tui/detail_pane.go`, `internal/tui/detail_pane_test.go`, `internal/tui/selection.go`, `internal/tui/selection_test.go`

- [ ] **Step 1: Confirm nothing references the symbols**

Run: `grep -rn "RunDetailPane\|newDetailPane\|detailPaneModel\|writeSelection\|readSelection\|selectionPath" internal/ | grep -v "_test.go:" || echo "no non-test references"`
Expected: only matches inside the four files being deleted (or "no non-test references"). If anything else references them, STOP — Task 3 was incomplete.

- [ ] **Step 2: Delete the files**

```bash
git rm internal/tui/detail_pane.go internal/tui/detail_pane_test.go internal/tui/selection.go internal/tui/selection_test.go
```

- [ ] **Step 3: Build + full test**

Run: `go build ./... && go test ./... 2>&1 | tail -15`
Expected: clean build; all packages PASS.

- [ ] **Step 4: Commit**

```bash
git commit -m "refactor(tui): remove dead detail viewer + selection-file code"
```

---

### Task 5: Manual integration + docs

No automated test covers interactive tmux; verify by hand, then document.

- [ ] **Step 1: Build the binary**

Run: `go build -o /tmp/agentctl-live ./cmd/agentctl && /tmp/agentctl-live tui --help`
Expected: builds; `--classic` visible; `--pane`/`--detail-pane` hidden.

- [ ] **Step 2: Verify tmux supports the flags used**

Run: `tmux -V`
Expected: ≥ 3.1 (for `split-window -l N%` and pane option `set-option -p remain-on-exit`).

- [ ] **Step 3: Detached geometry + driver smoke test (headless)**

Replicate `buildCockpit`'s sequence in a detached session using placeholders (no real claude), confirm 3 panes + that respawn-pane opens an attach, then tear down. Run:
```bash
S=agentctl-live-smoke
tmux kill-session -t "$S" 2>/dev/null
DET=$(tmux new-session -d -s "$S" -x 200 -y 50 -P -F '#{pane_id}' "sh -c 'echo DETAIL_PLACEHOLDER; exec sleep 60'")
MAS=$(tmux split-window -h -b -l 40% -t "$DET" -P -F '#{pane_id}' 'sleep 60')
LST=$(tmux split-window -v -b -l 50% -t "$MAS" -P -F '#{pane_id}' 'sleep 60')
tmux set-option -p -t "$DET" remain-on-exit on
tmux list-panes -t "$S" -F '#{pane_id} left=#{pane_left} top=#{pane_top} w=#{pane_width} h=#{pane_height}'
# simulate Enter → open a (placeholder) agent into the detail pane:
tmux respawn-pane -k -t "$DET" "sh -c 'echo OPENED_AGENT; exec sleep 60'"
sleep 1; tmux capture-pane -p -t "$DET" | grep -m1 OPENED_AGENT && echo "respawn OK"
tmux kill-session -t "$S"
```
Expected: list-panes shows detail right/full-height (left=80 top=0), master bottom-left, list top-left; `respawn OK` prints (the detail pane's content was replaced).

- [ ] **Step 4: Full interactive verification (daemon + a real agent + attached terminal)**

`agentctl daemon &`, spawn an agent, then `agentctl tui` from a normal shell and confirm:
- Press **Enter** on an agent → its live terminal appears in the right pane; type a prompt → the agent (its claude) receives it and responds in-pane.
- **Alt+←/→/↑/↓** move focus between list, master, and detail with no prefix.
- **`a`** still does a full-screen `switch-client` to the agent.
- Browsing the list with **j/k** does NOT change the detail pane (only Enter does).
- Terminating an opened agent leaves the detail pane showing `[exited]`, layout intact.
- **`q`** tears down the whole cockpit.

- [ ] **Step 5: Update `docs/USAGE.md`**

In the cockpit section: the right pane is now a **live interactive terminal** of the agent you open with **Enter** (type straight into it, like the master pane); browsing the list with j/k no longer changes it; **Alt+Arrow** switches panes (no prefix); `a` still gives full-screen attach. Remove any mention of the history block and of selection syncing / read-only output. Note the nested-tmux caveats (Ctrl-b is ambiguous in the detail pane — use Alt+Arrow; Alt+Arrow binds are tmux server-global). Classic (`--classic`) is unchanged except its detail view no longer shows history.

- [ ] **Step 6: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document the live interactive detail pane + Alt+Arrow nav"
```

---

## Self-Review

**Spec coverage:**
- Detail pane → live nested attach (`env -u TMUX tmux attach`) → Tasks 2 (`respawnDetailArgs`), 3 (Enter wiring, placeholder pane). ✅
- Driven by list pane via `respawn-pane` on Enter (not auto-follow) → Task 3 step 1(e). ✅
- `buildCockpit` passes detail pane id to list (`--detail-pane`), right-to-left create order → Task 3 step 4(c). ✅
- `remain-on-exit` on the detail pane → Task 3 step 4(c) call 4 + test. ✅
- Prefix-less Alt+Arrow nav → Task 3 step 4(c) + test. ✅
- Keep `a` (switch-client) → unchanged in list_pane.go (not modified). ✅
- Remove history block → Task 1. ✅
- Remove `selection.json` + detail viewer + state-dir subsystem (`cockpitBaseDir`/`cockpitStateDir`/`pidAlive`/`cleanStaleStateDirs`, `--state-dir`) → Task 3 (compositor/CLI trim) + Task 4 (file deletes). ✅
- Classic unaffected except no history → Task 1 (shared `renderDetail`). ✅
- Caveats documented → Task 5 step 5. ✅
- Testing: pure `respawnDetailArgs`, Enter reducer test, rebuilt `buildCockpit` FakeRunner sequence, manual interactive checklist → Tasks 2, 3, 5. ✅

**Placeholder scan:** No TBD/TODO; every code step has full code; the tmux-version dependency is an explicit Task 5 check with the concrete fix path noted (≥3.1).

**Type consistency:** `newListPane(a, detailPane string)` / `RunListPane(a, detailPane string)` consistent across list_pane.go, its tests, and `cli/tui.go` (Task 3). `listPaneCmd(self, detailPane string)` matches its caller in `buildCockpit` and the `compositor_test` assertions (Task 3). `cockpitOpts` (session/self/homeDir/masterCwd) matches `buildCockpit` and `RunCockpit`. `respawnDetailArgs(detailPane, agentSession)` / `openInDetailCmd(detailPane, agentSession)` defined in Task 2, used in Task 3. `detailPlaceholderCmd()` defined and asserted consistently (Task 3 steps 4 & 5). Deleted symbols (`detailPaneCmd`, `cockpitStateDir`, `cockpitBaseDir`, `pidAlive`, `cleanStaleStateDirs`, `RunDetailPane`, `writeSelection`, `readSelection`) have no remaining references after Task 3 (verified in Task 4 step 1).

## Out of scope (future)

Cockpit on its own tmux socket (scope bindings, dodge nested prefix), inline-composer alternative, auto-follow mode, a populated history view.
