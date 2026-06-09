# TUI Layout — Framed Panes + Scrollable List — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Stop the list's top rows scrolling off-screen (clamp it to a scroll window) and render both panes as equal-height rounded-border boxes with titles.

**Architecture:** Add pure helpers `listWindow` (stateless scroll window) + `titleBox` (bordered box with title spliced into the top border). Rewrite `renderList` to emit a windowed, height-bounded block, fix the detail viewport height, and assemble both panes as `titleBox`es in `View()`/`layout()`. No `Model`/reducer/keybinding changes.

**Tech Stack:** Go 1.26, `github.com/charmbracelet/lipgloss v1.1.0`, testify.

**Design spec:** `docs/superpowers/specs/2026-06-03-agentctl-tui-layout-design.md`

**Worktree:** all work in `/Users/srajan.pathak/workspace/personal/agentctl-tui` (branch `tui-layout`).

**Ordering:** pure helpers + tests first (Task 1; they compile unused), then the rendering rewrite that uses them (Task 2). Each commit builds + tests green.

---

### Task 1: pure helpers — `listWindow` + `titleBox`

**Files:** `internal/tui/list.go`, `internal/tui/boxes.go` (new), `internal/tui/list_test.go`, `internal/tui/boxes_test.go` (new).

- [ ] **Step 1: Write the failing tests.** Append to `internal/tui/list_test.go`:
```go
func TestListWindow(t *testing.T) {
	require.Equal(t, 0, listWindow(3, 0, 10), "n<=visible → 0")
	require.Equal(t, 0, listWindow(10, 2, 5), "cursor within first window → 0")
	require.Equal(t, 1, listWindow(10, 5, 5), "cursor at 5, visible 5 → top 1")
	require.Equal(t, 5, listWindow(10, 9, 5), "cursor at end → n-visible")
	require.Equal(t, 5, listWindow(10, 100, 5), "cursor past end clamps")
	require.Equal(t, 0, listWindow(10, 3, 0), "visible<1 → 0")
}
```
Create `internal/tui/boxes_test.go`:
```go
package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/require"
)

func TestTitleBoxDimsAndTitle(t *testing.T) {
	out := titleBox("Agents (3)", "row1\nrow2", 24, 6)
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 6, "exactly outerH lines")
	require.Contains(t, lines[0], "Agents (3)", "title inset into the top border")
	require.True(t, strings.HasPrefix(lines[0], "╭"), "rounded top-left corner preserved")
	require.Equal(t, 24, lipgloss.Width(out), "box is outerW wide")
}

func TestTitleBoxTruncatesLongTitle(t *testing.T) {
	out := titleBox("a very long title that does not fit", "x", 16, 4)
	require.Equal(t, 16, lipgloss.Width(out), "over-long title still fits the box width")
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/tui/ -run 'TestListWindow|TestTitleBox'` → FAIL (`listWindow`/`titleBox` undefined).

- [ ] **Step 3: Add `listWindow`** to `internal/tui/list.go` (it already has `max`):
```go
// listWindow returns the index of the first row to render so a window of
// `visible` rows always contains the cursor. Stateless — derived from cursor +
// height each render, so no scroll state is kept on the Model.
func listWindow(n, cursor, visible int) int {
	if visible < 1 || n <= visible {
		return 0
	}
	top := 0
	if cursor >= visible {
		top = cursor - visible + 1
	}
	if maxTop := n - visible; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}
```

- [ ] **Step 4: Create `internal/tui/boxes.go`:**
```go
package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// titleBox renders body inside a rounded-border box of exactly outerW x outerH,
// with title inset into the top border line (lipgloss v1.1 has no native border
// title). lipgloss pads/truncates body to the inner height.
func titleBox(title, body string, outerW, outerH int) string {
	if outerW < 4 {
		outerW = 4
	}
	if outerH < 3 {
		outerH = 3
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Width(outerW - 2).
		Height(outerH - 2).
		Render(body)
	return spliceTitle(box, title)
}

// spliceTitle overwrites the top border line of a bordered box with " title ",
// preserving the leading corner and trailing border; truncates a long title.
func spliceTitle(box, title string) string {
	if title == "" {
		return box
	}
	parts := strings.SplitN(box, "\n", 2)
	top := []rune(parts[0])
	if len(top) < 5 {
		return box
	}
	inner := len(top) - 3 // writable columns [2 .. len-2)
	label := []rune(" " + trunc(title, max(0, inner-1)) + " ")
	if len(label) > inner {
		label = label[:inner]
	}
	for i, r := range label {
		top[2+i] = r
	}
	rest := ""
	if len(parts) == 2 {
		rest = "\n" + parts[1]
	}
	return string(top) + rest
}
```

- [ ] **Step 5: Verify** — `go test ./internal/tui/ -run 'TestListWindow|TestTitleBox'` → PASS, then full `go test ./internal/tui/` → PASS (existing tests unaffected — `renderList`/`View` unchanged this task). `go build ./... && go vet ./internal/tui/`; `gofmt -l internal/tui/` → empty.

- [ ] **Step 6: Commit**
```bash
git add internal/tui/list.go internal/tui/boxes.go internal/tui/list_test.go internal/tui/boxes_test.go
git commit -m "feat(tui): pure helpers — listWindow (scroll window) + titleBox (bordered, titled)"
```

---

### Task 2: rewrite the rendering — windowed list + framed panes + correct heights

**Files:** `internal/tui/list.go`, `internal/tui/view.go`, `internal/tui/list_test.go`, `internal/tui/model_test.go`.

- [ ] **Step 1: Update + add tests.**

In `internal/tui/list_test.go`: change the existing call `m.renderList(120)` → `m.renderList(120, 10)`, and add (add `fmt` to that file's imports):
```go
func TestRenderListClampsToHeightAndKeepsCursor(t *testing.T) {
	m := New(&fakeAPI{})
	for i := 0; i < 20; i++ {
		m.sessions = append(m.sessions, &store.Session{ID: fmt.Sprintf("agent-%02d", i), Status: store.StatusWorking})
	}
	m.cursor = 18
	out := m.renderList(80, 8)
	require.Len(t, strings.Split(out, "\n"), 8, "rendered to exactly height lines")
	require.Contains(t, out, "agent-18", "the selected row is within the window")
	require.Contains(t, out, "more", "a ▲/▼ hint appears when rows are hidden")
}

func TestRenderListShortListPadsToHeight(t *testing.T) {
	m := New(&fakeAPI{})
	m.sessions = []*store.Session{{ID: "only", Status: store.StatusWorking}}
	require.Len(t, strings.Split(m.renderList(80, 6), "\n"), 6, "short list padded to height")
}
```

In `internal/tui/model_test.go`, add a no-overflow smoke (add `fmt`/`strings` to imports if missing):
```go
func TestViewDoesNotOverflowHeight(t *testing.T) {
	m := New(&fakeAPI{})
	m = step(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	for i := 0; i < 40; i++ {
		m.sessions = append(m.sessions, &store.Session{ID: fmt.Sprintf("a-%02d", i), Status: store.StatusWorking})
	}
	out := m.View()
	require.LessOrEqual(t, strings.Count(out, "\n")+1, 30, "View must not exceed terminal height")
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/tui/ -run 'TestRenderList|TestViewDoesNotOverflow'` → FAIL (renderList arity changed / overflow assertion fails against the current unclamped render).

- [ ] **Step 3: Rewrite `renderList`** in `internal/tui/list.go` (add `"strings"` to its imports):
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
	visible := height
	hidden := n > height
	if hidden {
		if visible = height - 1; visible < 1 {
			visible = 1
		}
	}
	top := listWindow(n, m.cursor, visible)

	var b strings.Builder
	used := 0
	for i := top; i < top+visible && i < n; i++ {
		s := m.sessions[i]
		label, st := badge(s.Status)
		line := fmt.Sprintf("%-12s %-9s %-11s %-5s %s",
			trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label), age(s.UpdatedAt),
			trunc(s.Subject, max(0, width-44)))
		cur := "  "
		if i == m.cursor {
			cur = stCursor.Render("› ")
			line = stCursor.Render(line)
		}
		b.WriteString(cur + line + "\n")
		used++
	}
	if hidden {
		var parts []string
		if top > 0 {
			parts = append(parts, fmt.Sprintf("▲ %d more", top))
		}
		if below := n - (top + visible); below > 0 {
			parts = append(parts, fmt.Sprintf("▼ %d more", below))
		}
		b.WriteString(stMuted.Render(strings.Join(parts, "   ")) + "\n")
		used++
	}
	for ; used < height; used++ {
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// padTo pads s with blank lines to exactly height lines.
func padTo(s string, height int) string {
	for cur := strings.Count(s, "\n") + 1; cur < height; cur++ {
		s += "\n"
	}
	return s
}
```

- [ ] **Step 4: Rewrite `View()` + `layout()`** in `internal/tui/view.go`.

Replace `layout()` with:
```go
// detailChrome is the number of non-viewport lines renderDetail emits
// (head, dir, subject, blank, output-title, blank, history≈7).
const detailChrome = 13

func (m *Model) layout() {
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	listOuter := m.listOuterW()
	detailInner := (m.w - listOuter) - 2 // detail box inner width
	if detailInner < 1 {
		detailInner = 1
	}
	m.vp.Width = detailInner
	vpH := (bodyH - 2) - detailChrome // box inner height minus detail chrome
	if vpH < 1 {
		vpH = 1
	}
	m.vp.Height = vpH
	m.ta.SetWidth(m.w - 2)
	m.ta.SetHeight(4)
	m.ti.Width = m.w - 20
}

// listOuterW is the list box outer width (~40%, clamped to leave room for detail).
func (m Model) listOuterW() int {
	w := m.w * 4 / 10
	if w < 24 {
		w = 24
	}
	if w > m.w-10 {
		w = m.w - 10
	}
	if w < 4 {
		w = 4
	}
	return w
}
```

Replace `View()`'s body assembly. Keep the header/conn lines and the footer/mode switch exactly as they are; change only the body construction (the `leftW/rightW/left/right/body` block and the help branch):
```go
	bodyH := m.h - 2
	if bodyH < 3 {
		bodyH = 3
	}
	var body string
	if m.mode == modeHelp {
		body = lipgloss.NewStyle().Width(m.w).Height(bodyH).Render(helpText())
	} else {
		listOuter := m.listOuterW()
		detailOuter := m.w - listOuter
		listTitle := fmt.Sprintf("Agents (%d)", len(m.sessions))
		detailTitle := m.selectedID()
		if detailTitle == "" {
			detailTitle = "—"
		}
		left := titleBox(listTitle, m.renderList(listOuter-2, bodyH-2), listOuter, bodyH)
		right := titleBox(detailTitle, m.renderDetail(detailOuter-2), detailOuter, bodyH)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}
```
(The final `return fmt.Sprintf("%s\n%s\n%s", header, body, footer)` and the `footer`/mode switch stay. Remove the now-unused old `leftW`/`rightW` lines.)

- [ ] **Step 5: Verify** — `go test ./internal/tui/` → PASS (new + existing). `go build ./... && go vet ./internal/tui/`; `gofmt -l internal/tui/` → empty. Confirm `grep -n 'renderList(' internal/tui/*.go` shows only the new 2-arg calls.

- [ ] **Step 6: Commit**
```bash
git add internal/tui/list.go internal/tui/view.go internal/tui/list_test.go internal/tui/model_test.go
git commit -m "feat(tui): framed panes + height-clamped scrollable list (no more cut-off top rows)"
```

---

## Final verification (after both tasks)

- [ ] `go build ./... && go vet ./... && go test -race ./...` — all green.
- [ ] Live check (darwin, real terminal): `make build`, run the daemon, `./bin/agentctl` (or `agentctl tui`) with several agents; confirm both panes are framed boxes with titles, the list never pushes the header off-screen, the selected row stays visible while paging with ↑/↓/j/k, and `▲/▼ N more` hints appear when the list is taller than the box. Resize the terminal to confirm reflow + no overflow.

Then proceed to **superpowers:finishing-a-development-branch**.
