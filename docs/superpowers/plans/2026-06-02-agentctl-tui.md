# agentctl Terminal UI (TUI) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A live, interactive Bubble Tea terminal cockpit (`agentctl tui`, also bare `agentctl`) — two-pane (agent list + selected-agent detail/output/history) that polls the daemon, with keys to create-from-prompt, send a message, attach, and terminate agents.

**Architecture:** New `internal/tui` package: a Bubble Tea program whose `Update` is a pure reducer over typed messages, driven by a small `api` interface that `*client.Client` already satisfies (HTTP to the daemon only — never Mongo). Live data comes from polling `GET /sessions` and `GET /sessions/{id}/output` on a ~1s tick; actions call the client.

**Tech Stack:** Go 1.26, `charmbracelet/bubbletea` + `lipgloss` + `bubbles` (viewport/textarea/textinput). Existing `internal/client`, `internal/store`.

**Reference spec:** `docs/superpowers/specs/2026-06-02-agentctl-tui-design.md`

---

## Conventions
- Module `github.com/srajanpathak/agentctl`. Executor sets up an isolated worktree first.
- TDD for the pure reducer (`Update`), helpers, and cmd→fake-api wiring. The full render is verified by a non-panicking `View` smoke test + manual run (no PTY in CI/sandbox).
- Commit after each task with the given message (no Co-Authored-By footer).
- Tests: `go test ./internal/tui/` (no Docker needed; uses a fake api).

## File map
```
internal/tui/model.go    Model, mode/msg types, api interface, Init, Update, helpers (selectedID/selected)
internal/tui/cmds.go     listCmd, outputCmd, spawnCmd, cleanupCmd, inputCmd, attachCmd, tick
internal/tui/keys.go     key handling (handleKey + per-mode handlers)
internal/tui/list.go     left list render + age()
internal/tui/detail.go   right detail render + overlays (new/send/kill/help)
internal/tui/styles.go   lipgloss styles + badge(status)
internal/tui/view.go     View() — compose header/body/footer
internal/tui/tui.go      Run(a api) error
internal/tui/*_test.go   reducer/helpers/cmds tests + fake api
internal/cli/tui.go      `agentctl tui` cmd; root RunE launches it on bare `agentctl`
internal/client/client.go  + StatusError (expose HTTP status for 409 detection)
README.md                + TUI note (Phase 6)
```

Phase order: **1** skeleton+list+nav → **2** detail+output → **3** create → **4** send+terminate(+StatusError) → **5** attach+help+banner → **6** integration+README.

---

## Phase 1 — Skeleton, live list, navigation

### Task 1.1: Dependencies + package skeleton + CLI command

**Files:** `internal/tui/model.go`, `internal/tui/styles.go`, `internal/tui/tui.go`, `internal/cli/tui.go`, `internal/cli/root.go`

- [ ] **Step 1: Add deps**

Run:
```bash
cd /Users/srajan.pathak/workspace/personal/agentctl-tui   # (the executor's worktree path)
go get github.com/charmbracelet/bubbletea@latest
go get github.com/charmbracelet/lipgloss@latest
go get github.com/charmbracelet/bubbles@latest
```
Expected: modules added to go.mod/go.sum.

- [ ] **Step 2: Model + api interface (skeleton)**

Create `internal/tui/model.go`:
```go
package tui

import (
	"context"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
)

// api is the subset of *client.Client the TUI needs (fakeable in tests).
type api interface {
	List(ctx context.Context) ([]*store.Session, error)
	Output(ctx context.Context, id string, lines int) (string, error)
	Spawn(ctx context.Context, p client.SpawnParams) (*store.Session, error)
	Cleanup(ctx context.Context, id string, force, hard bool) error
	Input(ctx context.Context, id, text string) error
}

type mode int

const (
	modeNormal mode = iota
	modeNewAgent
	modeSendMsg
	modeConfirmKill
	modeHelp
)

// Model is the Bubble Tea model. Update is a pure reducer over messages.
type Model struct {
	api       api
	sessions  []*store.Session
	cursor    int
	output    string
	vp        viewport.Model
	ta        textarea.Model
	ti        textinput.Model
	mode      mode
	killForce bool
	status    string
	connected bool
	w, h      int
	ready     bool
}

// New builds an initial model bound to the given api client.
func New(a api) Model {
	ta := textarea.New()
	ta.Placeholder = "What should this agent do?"
	ti := textinput.New()
	ti.Placeholder = "message…"
	return Model{api: a, ta: ta, ti: ti, connected: true}
}

func (m Model) selected() *store.Session {
	if m.cursor >= 0 && m.cursor < len(m.sessions) {
		return m.sessions[m.cursor]
	}
	return nil
}

func (m Model) selectedID() string {
	if s := m.selected(); s != nil {
		return s.ID
	}
	return ""
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(listCmd(m.api), tick())
}
```

- [ ] **Step 3: Styles + badge**

Create `internal/tui/styles.go`:
```go
package tui

import (
	"github.com/charmbracelet/lipgloss"
	"github.com/srajanpathak/agentctl/internal/store"
)

var (
	stBusy      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))  // green
	stAttention = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))  // amber
	stIdle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))  // grey
	stError     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))  // red
	stHeader    = lipgloss.NewStyle().Bold(true)
	stCursor    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	stMuted     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	stPaneTitle = lipgloss.NewStyle().Bold(true)
	stStatus    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// badge maps a status to a short label + style (mirrors the web status.ts mapping).
func badge(s store.Status) (string, lipgloss.Style) {
	switch s {
	case store.StatusSpawning:
		return "starting", stBusy
	case store.StatusWorking:
		return "busy", stBusy
	case store.StatusWaitingForInput:
		return "needs-input", stAttention
	case store.StatusIdle:
		return "idle", stIdle
	case store.StatusDone:
		return "done", stIdle
	case store.StatusErrored:
		return "error", stError
	case store.StatusOrphaned:
		return "orphaned", stError
	default:
		if s == "" {
			return "classifying", stMuted
		}
		return string(s), stIdle
	}
}
```

- [ ] **Step 4: Run entrypoint + CLI command**

Create `internal/tui/tui.go`:
```go
package tui

import tea "github.com/charmbracelet/bubbletea"

// Run starts the TUI against the given api client and blocks until the user quits.
func Run(a api) error {
	p := tea.NewProgram(New(a), tea.WithAltScreen())
	_, err := p.Run()
	return err
}
```
Create `internal/cli/tui.go`:
```go
package cli

import (
	"github.com/spf13/cobra"
	"github.com/srajanpathak/agentctl/internal/tui"
)

func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Live terminal cockpit for agents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return tui.Run(clientFor(cmd))
		},
	}
}
```
In `internal/cli/root.go`, register the command and make bare `agentctl` launch it. Add to `newRootCmd()` before `return root`:
```go
	root.AddCommand(newTUICmd())
	root.Args = cobra.NoArgs
	root.RunE = func(cmd *cobra.Command, args []string) error {
		return tui.Run(clientFor(cmd))
	}
```
and add the import `"github.com/srajanpathak/agentctl/internal/tui"` to `root.go`. (`clientFor` already exists in `internal/cli/common.go` and returns a `*client.Client`, which satisfies `tui.api`.)

- [ ] **Step 5: Build**

Run: `go build ./...`
Expected: compiles. (`Update`/`View` are added in the next task; for this step add temporary minimal stubs so it builds — OR implement Task 1.2 before building. Implement 1.2 next; build there.)

> Note: `Model` does not yet satisfy `tea.Model` (no `Update`/`View`). Proceed directly to Task 1.2, which adds them, then build.

- [ ] **Step 6: Commit (after 1.2 builds)** — deferred; commit at end of Task 1.2.

### Task 1.2: cmds, reducer (nav + list), list render, View

**Files:** `internal/tui/cmds.go`, `internal/tui/keys.go`, `internal/tui/list.go`, `internal/tui/view.go`, `internal/tui/model.go` (Update), `internal/tui/model_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/model_test.go`:
```go
package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
	"github.com/stretchr/testify/require"
)

// fakeAPI is a test double for the tui api interface.
type fakeAPI struct {
	sessions  []*store.Session
	listErr   error
	output    string
	spawned   *client.SpawnParams
	cleaned   *struct{ id string; force, hard bool }
	cleanErr  error
	sentTo    string
	sentText  string
}

func (f *fakeAPI) List(context.Context) ([]*store.Session, error) { return f.sessions, f.listErr }
func (f *fakeAPI) Output(_ context.Context, _ string, _ int) (string, error) { return f.output, nil }
func (f *fakeAPI) Spawn(_ context.Context, p client.SpawnParams) (*store.Session, error) {
	f.spawned = &p
	return &store.Session{ID: "agent-new"}, nil
}
func (f *fakeAPI) Cleanup(_ context.Context, id string, force, hard bool) error {
	f.cleaned = &struct{ id string; force, hard bool }{id, force, hard}
	return f.cleanErr
}
func (f *fakeAPI) Input(_ context.Context, id, text string) error { f.sentTo, f.sentText = id, text; return nil }

// step applies a msg and returns the updated concrete Model.
func step(m Model, msg tea.Msg) Model {
	nm, _ := m.Update(msg)
	return nm.(Model)
}

func key(s string) tea.KeyMsg {
	switch s {
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

func threeSessions() []*store.Session {
	return []*store.Session{
		{ID: "a", Status: store.StatusWorking},
		{ID: "b", Status: store.StatusIdle},
		{ID: "c", Status: store.StatusWaitingForInput},
	}
}

func TestCursorMovesAndClamps(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.Equal(t, 0, m.cursor)
	m = step(m, key("down"))
	m = step(m, key("down"))
	require.Equal(t, 2, m.cursor)
	m = step(m, key("down")) // clamp at last
	require.Equal(t, 2, m.cursor)
	m = step(m, key("up"))
	require.Equal(t, 1, m.cursor)
	require.Equal(t, "b", m.selectedID())
}

func TestSessionsMsgRepinsByID(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	m = step(m, key("down")) // cursor=1 → "b"
	// New snapshot reorders: b is now first. Selection should follow id "b".
	m = step(m, sessionsMsg{sessions: []*store.Session{
		{ID: "b", Status: store.StatusIdle},
		{ID: "a", Status: store.StatusWorking},
	}})
	require.Equal(t, "b", m.selectedID())
}

func TestListErrorSetsDisconnected(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{err: client.ErrDaemonDown})
	require.False(t, m.connected)
}

func TestQuitKey(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(key("q"))
	require.NotNil(t, cmd, "q should return a command (tea.Quit)")
}

func TestViewDoesNotPanic(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.NotEmpty(t, m.View())
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tui/`
Expected: FAIL — `sessionsMsg`, `Update`, `View` undefined.

- [ ] **Step 3: cmds**

Create `internal/tui/cmds.go`:
```go
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srajanpathak/agentctl/internal/client"
	"github.com/srajanpathak/agentctl/internal/store"
)

type sessionsMsg struct {
	sessions []*store.Session
	err      error
}
type outputMsg struct {
	id   string
	text string
}
type spawnDoneMsg struct {
	id  string
	err error
}
type cleanupDoneMsg struct {
	id       string
	err      error
	conflict bool
}
type inputDoneMsg struct{ err error }
type attachDoneMsg struct{ err error }
type tickMsg time.Time

func bg() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func listCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		ss, err := a.List(ctx)
		return sessionsMsg{sessions: ss, err: err}
	}
}

func outputCmd(a api, id string) tea.Cmd {
	if id == "" {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		out, err := a.Output(ctx, id, 400)
		if err != nil {
			return outputMsg{id: id, text: ""}
		}
		return outputMsg{id: id, text: out}
	}
}

func spawnCmd(a api, prompt string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		s, err := a.Spawn(ctx, client.SpawnParams{Prompt: prompt})
		if err != nil {
			return spawnDoneMsg{err: err}
		}
		return spawnDoneMsg{id: s.ID}
	}
}

func inputCmd(a api, id, text string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		return inputDoneMsg{err: a.Input(ctx, id, text)}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}
```
(`cleanupCmd` and `attachCmd` are added in Phases 4/5.)

- [ ] **Step 4: Update reducer + key handling**

Append to `internal/tui/model.go`:
```go
// Update is the pure reducer.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		m.ready = true
		return m, nil

	case tickMsg:
		return m, tea.Batch(listCmd(m.api), outputCmd(m.api, m.selectedID()), tick())

	case sessionsMsg:
		if msg.err != nil {
			m.connected = false
			return m, nil
		}
		m.connected = true
		prevID := m.selectedID()
		m.sessions = msg.sessions
		m.repin(prevID)
		return m, nil

	case outputMsg:
		if msg.id == m.selectedID() {
			m.output = msg.text
			m.vp.SetContent(msg.text)
			m.vp.GotoBottom()
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// repin keeps the cursor on the session with prevID if it still exists, else clamps.
func (m *Model) repin(prevID string) {
	if prevID != "" {
		for i, s := range m.sessions {
			if s.ID == prevID {
				m.cursor = i
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
```
Create `internal/tui/keys.go`:
```go
package tui

import tea "github.com/charmbracelet/bubbletea"

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global quit (normal mode only; modes handle their own esc).
	if m.mode == modeNormal {
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "down", "j":
			if m.cursor < len(m.sessions)-1 {
				m.cursor++
			}
			return m, nil
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
			return m, nil
		}
	}
	return m, nil
}
```
(Other modes/keys are added in later phases.)

- [ ] **Step 5: list render + view**

Create `internal/tui/list.go`:
```go
package tui

import (
	"fmt"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
)

func age(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// renderList returns the left-pane lines for the given width.
func (m Model) renderList(width int) string {
	if len(m.sessions) == 0 {
		return stMuted.Render("No agents — press n to create one")
	}
	out := ""
	for i, s := range m.sessions {
		label, st := badge(s.Status)
		cursor := "  "
		line := fmt.Sprintf("%-12s %-9s %-11s %s", trunc(s.ID, 12), trunc(string(typeOr(s)), 9), st.Render(label), trunc(s.Subject, max(0, width-40)))
		if i == m.cursor {
			cursor = stCursor.Render("› ")
			line = stCursor.Render(line)
		}
		out += cursor + line + "\n"
	}
	return out
}

func typeOr(s *store.Session) string {
	if s.Type == "" {
		return "classifying"
	}
	return string(s.Type)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
```
Create `internal/tui/view.go`:
```go
package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

func (m *Model) layout() {
	// detail viewport: right ~60% width, body height minus header/footer.
	rw := m.w * 6 / 10
	if rw < 20 {
		rw = m.w
	}
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	m.vp.Width = rw - 2
	m.vp.Height = bodyH - 6
	if m.vp.Height < 1 {
		m.vp.Height = 1
	}
}

func (m Model) View() string {
	if !m.ready {
		return "loading…"
	}
	conn := stStatus.Render("live ●")
	if !m.connected {
		conn = stError.Render("reconnecting…")
	}
	header := stHeader.Render("agentctl") + "  " + conn

	leftW := m.w * 4 / 10
	rightW := m.w - leftW - 1
	left := lipgloss.NewStyle().Width(leftW).Render(m.renderList(leftW))
	right := lipgloss.NewStyle().Width(rightW).Render(m.renderDetail(rightW))
	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)

	footer := m.footer()
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
}

func (m Model) footer() string {
	if m.status != "" {
		return stStatus.Render(m.status)
	}
	return stMuted.Render("n new · s send · a attach · x kill · tab focus · ? help · q quit")
}
```
Create `internal/tui/detail.go` (minimal for Phase 1; expanded in Phase 2):
```go
package tui

import "fmt"

func (m Model) renderDetail(width int) string {
	s := m.selected()
	if s == nil {
		return stMuted.Render("Select an agent")
	}
	label, st := badge(s.Status)
	head := stPaneTitle.Render(s.ID) + " " + st.Render(label)
	meta := stMuted.Render(fmt.Sprintf("dir: %s", dashIfEmpty(s.Workdir)))
	subj := stMuted.Render(fmt.Sprintf("subject: %s", dashIfEmpty(s.Subject)))
	return fmt.Sprintf("%s\n%s\n%s", head, meta, subj)
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
```

- [ ] **Step 6: Run to verify pass + build**

Run: `go build ./... && go test ./internal/tui/`
Expected: build clean; all Phase-1 tests pass (`TestCursorMovesAndClamps`, `TestSessionsMsgRepinsByID`, `TestListErrorSetsDisconnected`, `TestQuitKey`, `TestViewDoesNotPanic`).

- [ ] **Step 7: Commit**

```bash
git add internal/tui internal/cli/tui.go internal/cli/root.go go.mod go.sum
git commit -m "feat: tui skeleton — live agent list, navigation, two-pane view"
```

---

## Phase 2 — Detail pane + live output

### Task 2.1: Output polling, viewport scroll, full detail

**Files:** `internal/tui/detail.go`, `internal/tui/keys.go`, `internal/tui/model.go`, `internal/tui/model_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/tui/model_test.go`:
```go
func TestOutputMsgFillsSelected(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // cursor=0 → "a"
	m = step(m, outputMsg{id: "a", text: "hello output"})
	require.Equal(t, "hello output", m.output)
}

func TestOutputMsgIgnoresStaleID(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, outputMsg{id: "a", text: "for a"})
	m = step(m, outputMsg{id: "c", text: "stale"}) // not selected
	require.Equal(t, "for a", m.output)
}

func TestTabFocusesOutput(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	require.False(t, m.outputFocused)
	m = step(m, key("\t")) // tab
	require.True(t, m.outputFocused)
}
```
(The `key("\t")` path: add a `"tab"` case to the `key` helper — append to the helper's switch: `case "\t": return tea.KeyMsg{Type: tea.KeyTab}`.)

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tui/ -run 'TestOutputMsg|TestTabFocuses'`
Expected: FAIL — `m.outputFocused` undefined; tab not handled.

- [ ] **Step 3: Add outputFocused + tab + viewport routing**

In `internal/tui/model.go`, add `outputFocused bool` to the `Model` struct. In `Update`, route scroll keys to the viewport when focused — add to the `tea.KeyMsg` case BEFORE delegating to `handleKey`:
```go
	case tea.KeyMsg:
		if m.mode == modeNormal && m.outputFocused {
			switch msg.String() {
			case "tab", "esc":
				m.outputFocused = false
				return m, nil
			case "q", "ctrl+c":
				return m, tea.Quit
			}
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg) // PgUp/PgDn/up/down scroll the output
			return m, cmd
		}
		return m.handleKey(msg)
```
In `internal/tui/keys.go` `handleKey`, add a `tab` case in the normal-mode switch:
```go
		case "tab":
			if m.selected() != nil {
				m.outputFocused = true
			}
			return m, nil
```

- [ ] **Step 4: Full detail render (output viewport + history)**

Replace `internal/tui/detail.go`:
```go
package tui

import (
	"fmt"
	"strings"
)

func (m Model) renderDetail(width int) string {
	s := m.selected()
	if s == nil {
		return stMuted.Render("Select an agent")
	}
	label, st := badge(s.Status)
	head := stPaneTitle.Render(s.ID) + " " + st.Render(label) + "  " + stMuted.Render(typeOr(s))
	meta := stMuted.Render("dir: " + dashIfEmpty(s.Workdir))
	subj := stMuted.Render("subject: " + dashIfEmpty(s.Subject))

	outTitle := stPaneTitle.Render("─ output ") + stMuted.Render(focusHint(m.outputFocused))
	out := m.vp.View()

	hist := stPaneTitle.Render("─ history ─") + "\n" + renderHistory(s, 6)

	return strings.Join([]string{head, meta, subj, "", outTitle, out, "", hist}, "\n")
}

func focusHint(focused bool) string {
	if focused {
		return "(scrolling — tab/esc to leave)"
	}
	return "(tab to scroll)"
}

func renderHistory(s sessionLike, n int) string {
	ev := s.events()
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

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
```
Add a tiny adapter so `renderHistory` is testable without importing store cycles — actually use `*store.Session` directly. Replace `sessionLike`/`s.events()` usage with the concrete type:
```go
// (simpler) signature:
func renderHistory(s *store.Session, n int) string {
	ev := s.Events
	...
}
```
and call `renderHistory(s, 6)`. Add `"github.com/srajanpathak/agentctl/internal/store"` import. (Drop the `sessionLike` indirection — use `*store.Session`.)

- [ ] **Step 5: Run to verify pass**

Run: `go build ./... && go test ./internal/tui/`
Expected: PASS (Phase 1 + 2 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/tui
git commit -m "feat: tui detail pane — live output viewport, scroll, history"
```

---

## Phase 3 — Create agent (prompt)

### Task 3.1: newAgent mode (textarea → spawn)

**Files:** `internal/tui/keys.go`, `internal/tui/model.go`, `internal/tui/detail.go`, `internal/tui/model_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/tui/model_test.go`:
```go
func TestNewAgentModeFlow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	require.Equal(t, modeNewAgent, m.mode)
	// type a prompt
	m = step(m, key("research SSE"))
	// submit with ctrl+s
	m, _ = submit(m, tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, f.spawned)
	require.Equal(t, "research SSE", f.spawned.Prompt)
}

func TestNewAgentEscCancels(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("n"))
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}

func TestSpawnDoneSelectsNewAgent(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "agent-new"}, {ID: "x"}}})
	m = step(m, spawnDoneMsg{id: "agent-new"})
	require.Equal(t, "agent-new", m.pendingSelect)
	// next list refresh pins it
	m = step(m, sessionsMsg{sessions: []*store.Session{{ID: "x"}, {ID: "agent-new"}}})
	require.Equal(t, "agent-new", m.selectedID())
}

// submit is a test helper: applies one key and returns model + cmd.
func submit(m Model, msg tea.Msg) (Model, tea.Cmd) {
	nm, cmd := m.Update(msg)
	return nm.(Model), cmd
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tui/ -run 'TestNewAgent|TestSpawnDone'`
Expected: FAIL — `n` not handled, `pendingSelect` undefined.

- [ ] **Step 3: Implement newAgent mode**

In `internal/tui/model.go`, add `pendingSelect string` to `Model`. In `repin`, prefer `pendingSelect` if set and present:
```go
func (m *Model) repin(prevID string) {
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
```
Add `spawnDoneMsg` handling to `Update` (new case):
```go
	case spawnDoneMsg:
		if msg.err != nil {
			m.status = "spawn failed: " + msg.err.Error()
		} else {
			m.status = "spawned " + msg.id
			m.pendingSelect = msg.id
		}
		return m, nil
```
In the `tea.KeyMsg` case of `Update`, BEFORE the normal/focused handling, dispatch by mode for the input modes:
```go
		if m.mode == modeNewAgent {
			return m.updateNewAgent(msg)
		}
```
In `internal/tui/keys.go`, add the `n` trigger to the normal-mode switch and the mode handler:
```go
		case "n":
			m.mode = modeNewAgent
			m.ta.Reset()
			m.ta.Focus()
			return m, nil
```
```go
func (m Model) updateNewAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.ta.Blur()
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
}
```
Add `"strings"` to `keys.go` imports.

- [ ] **Step 4: Render the newAgent overlay**

In `internal/tui/view.go` `View`, when `m.mode == modeNewAgent` replace the footer area with the textarea. Change the final return to:
```go
	footer := m.footer()
	if m.mode == modeNewAgent {
		footer = stPaneTitle.Render("New agent — describe the task (ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
```
And size the textarea in `layout()`:
```go
	m.ta.SetWidth(m.w - 2)
	m.ta.SetHeight(4)
```

- [ ] **Step 5: Run to verify pass**

Run: `go build ./... && go test ./internal/tui/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/tui
git commit -m "feat: tui create agent from a prompt (n)"
```

---

## Phase 4 — Send + terminate

### Task 4.1: client.StatusError (expose HTTP status)

**Files:** `internal/client/client.go`, `internal/client/client_test.go`

- [ ] **Step 1: Failing test**

Append to `internal/client/client_test.go`:
```go
func TestCleanupConflictIsStatusError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"uncommitted changes"}`))
	}))
	defer ts.Close()
	err := New(ts.URL).Cleanup(t.Context(), "A-1", false, false)
	require.Error(t, err)
	var se *StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, 409, se.Code)
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/client/ -run TestCleanupConflict`
Expected: FAIL — `StatusError` undefined.

- [ ] **Step 3: Add StatusError + return it from do**

In `internal/client/client.go`, add:
```go
// StatusError is returned for non-2xx daemon responses, exposing the HTTP code.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("daemon error (%d): %s", e.Code, e.Msg)
}
```
In `do`, replace the `if resp.StatusCode >= 400 { ... }` block's final error with a `*StatusError`:
```go
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		msg := e.Error
		if msg == "" {
			msg = resp.Status
		}
		return &StatusError{Code: resp.StatusCode, Msg: msg}
	}
```
(The `Error()` string matches the previous `fmt.Errorf("daemon error (%d): %s", ...)` format, so nothing else changes.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/client/`
Expected: PASS (new test + existing).

- [ ] **Step 5: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "feat: client StatusError exposes HTTP status (for 409 handling)"
```

### Task 4.2: send (s) + terminate (x) with force on 409

**Files:** `internal/tui/cmds.go`, `internal/tui/keys.go`, `internal/tui/model.go`, `internal/tui/view.go`, `internal/tui/model_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/tui/model_test.go`:
```go
func TestSendMessageFlow(t *testing.T) {
	f := &fakeAPI{}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("s"))
	require.Equal(t, modeSendMsg, m.mode)
	m = step(m, key("hello"))
	m, _ = submit(m, key("enter"))
	require.Equal(t, "a", f.sentTo)
	require.Equal(t, "hello", f.sentText)
	require.Equal(t, modeNormal, m.mode)
}

func TestKillConfirmThenForceOn409(t *testing.T) {
	f := &fakeAPI{cleanErr: &client.StatusError{Code: 409, Msg: "unpushed"}}
	m := New(f)
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, sessionsMsg{sessions: threeSessions()}) // selected "a"
	m = step(m, key("x"))
	require.Equal(t, modeConfirmKill, m.mode)
	m, _ = submit(m, key("y")) // confirm → cleanup (non-force)
	// simulate the cleanup result coming back as a conflict
	m = step(m, cleanupDoneMsg{id: "a", conflict: true})
	require.True(t, m.killForce, "409 → force prompt")
	m, _ = submit(m, key("X")) // force
	require.Equal(t, "a", f.cleaned.id)
	require.True(t, f.cleaned.force)
}

func TestKillEscCancels(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, sessionsMsg{sessions: threeSessions()})
	m = step(m, key("x"))
	m = step(m, key("esc"))
	require.Equal(t, modeNormal, m.mode)
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tui/ -run 'TestSendMessage|TestKill'`
Expected: FAIL — `s`/`x` not handled, `cleanupCmd`/`cleanupDoneMsg.conflict` flow missing.

- [ ] **Step 3: cleanupCmd**

Append to `internal/tui/cmds.go`:
```go
func cleanupCmd(a api, id string, force bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		err := a.Cleanup(ctx, id, force, false)
		if err != nil {
			var se *client.StatusError
			if errors.As(err, &se) && se.Code == 409 {
				return cleanupDoneMsg{id: id, conflict: true, err: err}
			}
			return cleanupDoneMsg{id: id, err: err}
		}
		return cleanupDoneMsg{id: id}
	}
}
```
Add `"errors"` to `cmds.go` imports.

- [ ] **Step 4: send/kill key handling + msgs**

In `internal/tui/model.go` `Update`, dispatch the new modes in the `tea.KeyMsg` case (alongside `modeNewAgent`):
```go
		if m.mode == modeSendMsg {
			return m.updateSendMsg(msg)
		}
		if m.mode == modeConfirmKill {
			return m.updateConfirmKill(msg)
		}
```
Add result-msg cases to `Update`:
```go
	case inputDoneMsg:
		if msg.err != nil {
			m.status = "send failed: " + msg.err.Error()
		} else {
			m.status = "sent"
		}
		return m, nil

	case cleanupDoneMsg:
		switch {
		case msg.conflict:
			m.mode = modeConfirmKill
			m.killForce = true
			m.status = "uncommitted/unpushed — press X to force, esc to cancel"
		case msg.err != nil:
			m.mode = modeNormal
			m.killForce = false
			m.status = "terminate failed: " + msg.err.Error()
		default:
			m.mode = modeNormal
			m.killForce = false
			m.status = "terminated " + msg.id
		}
		return m, nil
```
In `internal/tui/keys.go`, add `s` and `x` to the normal-mode switch and the two mode handlers:
```go
		case "s":
			if m.selected() != nil {
				m.mode = modeSendMsg
				m.ti.Reset()
				m.ti.Focus()
			}
			return m, nil
		case "x":
			if m.selected() != nil {
				m.mode = modeConfirmKill
				m.killForce = false
			}
			return m, nil
```
```go
func (m Model) updateSendMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeNormal
		m.ti.Blur()
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
}

func (m Model) updateConfirmKill(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	id := m.selectedID()
	switch msg.String() {
	case "esc", "n", "N":
		m.mode = modeNormal
		m.killForce = false
		m.status = ""
		return m, nil
	case "y", "Y":
		if !m.killForce && id != "" {
			return m, cleanupCmd(m.api, id, false)
		}
		return m, nil
	case "X":
		if m.killForce && id != "" {
			return m, cleanupCmd(m.api, id, true)
		}
		return m, nil
	}
	return m, nil
}
```

- [ ] **Step 5: Overlays for send/kill**

In `internal/tui/view.go` `View`, extend the footer-overlay logic:
```go
	footer := m.footer()
	switch m.mode {
	case modeNewAgent:
		footer = stPaneTitle.Render("New agent — describe the task (ctrl+s submit · esc cancel)") + "\n" + m.ta.View()
	case modeSendMsg:
		footer = stPaneTitle.Render("Send to "+m.selectedID()+" (enter · esc):") + " " + m.ti.View()
	case modeConfirmKill:
		if m.killForce {
			footer = stError.Render("uncommitted/unpushed — press X to FORCE terminate, esc to cancel")
		} else {
			footer = stError.Render("Terminate " + m.selectedID() + "? y / N")
		}
	}
	return fmt.Sprintf("%s\n%s\n%s", header, body, footer)
```
Size the textinput in `layout()`: `m.ti.Width = m.w - 20`.

- [ ] **Step 6: Run to verify pass**

Run: `go build ./... && go test ./internal/tui/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/tui
git commit -m "feat: tui send message (s) + terminate with 409 force (x)"
```

---

## Phase 5 — Attach + help + daemon-down banner

### Task 5.1: attach (a) + help (?) + status polish

**Files:** `internal/tui/cmds.go`, `internal/tui/keys.go`, `internal/tui/model.go`, `internal/tui/view.go`, `internal/tui/model_test.go`

- [ ] **Step 1: Failing tests**

Append to `internal/tui/model_test.go`:
```go
func TestHelpToggle(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	m = step(m, key("?"))
	require.Equal(t, modeHelp, m.mode)
	m = step(m, key("?")) // any key closes
	require.Equal(t, modeNormal, m.mode)
}

func TestAttachDoneShowsError(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, attachDoneMsg{err: context.DeadlineExceeded})
	require.Contains(t, m.status, "attach")
}

func TestAttachNoOpWhenNoSelection(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(key("a"))
	require.Nil(t, cmd, "attach with no selection does nothing")
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/tui/ -run 'TestHelp|TestAttach'`
Expected: FAIL — `?`/`a` not handled, `modeHelp` close, `attachDoneMsg` handling.

- [ ] **Step 3: attachCmd**

Append to `internal/tui/cmds.go`:
```go
func attachCmd(id string) tea.Cmd {
	c := exec.Command("tmux", "attach", "-t", id)
	return tea.ExecProcess(c, func(err error) tea.Msg {
		return attachDoneMsg{err: err}
	})
}
```
Add `"os/exec"` to `cmds.go` imports.

- [ ] **Step 4: keys + handling**

In `internal/tui/keys.go` normal-mode switch, add:
```go
		case "a":
			if m.selected() != nil {
				return m, attachCmd(m.selectedID())
			}
			return m, nil
		case "?":
			m.mode = modeHelp
			return m, nil
```
In `internal/tui/model.go` `Update` `tea.KeyMsg` case, handle help mode (any key closes) — add before the normal handling:
```go
		if m.mode == modeHelp {
			m.mode = modeNormal
			return m, nil
		}
```
Add the `attachDoneMsg` case to `Update`:
```go
	case attachDoneMsg:
		if msg.err != nil {
			m.status = "attach failed: " + msg.err.Error()
		} else {
			m.status = ""
		}
		return m, nil
```

- [ ] **Step 5: help overlay + daemon-down banner**

In `internal/tui/view.go`:
- When `!m.connected`, prefix the header banner:
```go
	header := stHeader.Render("agentctl") + "  " + conn
	if !m.connected {
		header += "  " + stError.Render("daemon not running — start it with `agentctl daemon`")
	}
```
- Add a `modeHelp` body override (replace `body` when in help):
```go
	if m.mode == modeHelp {
		body = lipgloss.NewStyle().Width(m.w).Render(helpText())
	}
```
and add:
```go
func helpText() string {
	return stPaneTitle.Render("Keys") + "\n" +
		"  ↑/↓ or j/k   move selection\n" +
		"  tab          focus output (PgUp/PgDn scroll), tab/esc to leave\n" +
		"  n            new agent (prompt)\n" +
		"  s            send a message to the selected agent\n" +
		"  a            attach to its tmux session\n" +
		"  x            terminate (X to force on uncommitted/unpushed)\n" +
		"  ?            toggle this help\n" +
		"  q            quit\n"
}
```

- [ ] **Step 6: Run to verify pass + full suite**

Run: `go build ./... && go test ./internal/tui/ ./internal/client/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/tui
git commit -m "feat: tui attach (a), help (?), daemon-down banner"
```

---

## Phase 6 — Integration + README

### Task 6.1: Full verification + README

**Files:** `README.md`

- [ ] **Step 1: Full build + suites**

Run:
```bash
make release && make mongo-up
go build ./... && go vet ./... && go test ./...
cd web && npm test && cd ..
```
Expected: all green (the TUI package tests pass with the fake api; nothing else regressed).

- [ ] **Step 2: Manual smoke (PTY required — sandbox can't, run locally)**

Run (with the daemon up and a session or two):
```bash
./bin/agentctl daemon & sleep 1
./bin/agentctl start "research SSE reconnection" >/dev/null   # create one
./bin/agentctl            # bare → opens the TUI
# verify: live list, ↓ selects, right pane shows dir/subject/output, tab scrolls output,
#   n creates (ctrl+s), s sends (enter), x terminates (y, then X if guarded), a attaches
#   (drops to tmux, resume on detach), ? help, q quit.
kill %1; tmux kill-server 2>/dev/null
```
Expected: all interactions behave; bare `agentctl` and `agentctl tui` both open it.

- [ ] **Step 3: README — TUI section**

In `README.md`, add a "Terminal UI" section: `agentctl tui` (or bare `agentctl`) opens a live two-pane cockpit; list of agents with busy/idle badges + dir + subject on the left, selected agent's live output + history on the right; keys `n` new (prompt), `s` send, `a` attach, `x` terminate (force on guard), `tab` scroll output, `?` help, `q` quit. Note it polls the daemon (must be running) and that `a` attach hands off to `tmux`.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: terminal UI (agentctl tui)"
```

---

## Self-review against the spec

**Spec coverage** (`2026-06-02-agentctl-tui-design.md`):
- §2 Bubble Tea + Lipgloss + bubbles — Task 1.1 deps; viewport/textarea/textinput used in 2/3/4. ✅
- §2 two-pane layout (header/list/detail/footer) — `view.go` (1.2), detail (2.1). ✅
- §2 full management: navigate (1.2), create n (3.1), send s + terminate x (4.2), attach a (5.1), live-tail (2.1). ✅
- §2 launch `agentctl tui` + bare `agentctl` — Task 1.1 (`newTUICmd` + root `RunE`/`NoArgs`). ✅
- §2 client-only (`api` interface, `*client.Client` satisfies) — Task 1.1. ✅
- §2/§3 poll list + output (~1s tick) — `tick`/`listCmd`/`outputCmd` (1.2), output poll wired via tickMsg batch (1.2), filled in 2.1. ✅
- §4 model/files map — matches the created files. ✅
- §5 modes & keys (normal/newAgent/sendMsg/confirmKill/help; n/s/a/x/tab/?/q; ctrl+s submit; 409→force) — Tasks 1.2/2.1/3.1/4.2/5.1. ✅
- §6 error handling: status line (3.1/4.2/5.1), ErrDaemonDown banner (1.2 sets connected=false; 5.1 banner), 409 force (4.x), attach error (5.1), stale output drop (2.1), empty-list hint (1.2). ✅
- §7 testing: pure Update tests, badge/age helpers, cmds via fake api, View smoke — Tasks 1.2/2.1/3.1/4.2/5.1. ✅
- StatusError needed for 409 detection — Task 4.1 (client). ✅

**Placeholder scan:** No TBD/TODO. Every step has complete code + expected output. Task 1.1 Step 5 explicitly notes the build happens in 1.2 (since `Update`/`View` arrive there) rather than leaving a non-compiling commit — the first commit is at the end of 1.2 when it builds + tests green.

**Type consistency:** `api` interface methods match `*client.Client` (`List/Output/Spawn/Cleanup/Input`, verified against client.go). Msg types (`sessionsMsg`/`outputMsg`/`spawnDoneMsg`/`cleanupDoneMsg{conflict}`/`inputDoneMsg`/`attachDoneMsg`/`tickMsg`) defined in `cmds.go` (1.2) and handled in `Update` across phases. `Model` fields (`cursor`/`outputFocused`/`mode`/`killForce`/`pendingSelect`/`vp`/`ta`/`ti`/`status`/`connected`) are introduced where first used and referenced consistently. `client.StatusError{Code,Msg}` (4.1) is what `cleanupCmd` (4.2) inspects via `errors.As`. `badge`/`age`/`trunc`/`typeOr`/`dashIfEmpty`/`renderHistory(*store.Session,n)` signatures are consistent across `styles.go`/`list.go`/`detail.go`. Root `RunE` + `newTUICmd` both call `tui.Run(clientFor(cmd))`.
