# TUI Pipeline View (Phase 5) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface pipelines in the TUI cockpit's list pane — a grouped "▸ Pipelines" section with per-job status, an inline DAG view, attach-to-a-running-job, and the two no-input controls (cancel a pipeline, retry a failed/stuck job) — reusing the existing item-union / inline-render / attach machinery.

**Architecture:** The list pane's `items()` is already a tagged union (pinned `approvals` row + dir-group headers + session rows). Pipelines slot in as two new `item` variants (`pipeline` header + `pipelineJob`), prepended like the approvals row; pipeline-owned sessions are filtered out of the flat list (shown under their pipeline instead). Selecting a pipeline renders an inline DAG in the detail region (a new `renderPipeline`, mirroring `renderApprovalsQueue`); selecting a *running* job attaches the right pane via the existing `attachCmd` (jobs are sessions). The model polls `PipelineList` alongside sessions on each tick.

**Tech Stack:** Go, Bubble Tea (`charmbracelet/bubbletea`), the existing `internal/client` pipeline methods. Module: `github.com/srajanpathak/agentctl`.

## Scope (confirmed direction)
**In this plan:** view (pipeline + job rows, inline DAG), attach a running job (`a`), **cancel** a pipeline (`x`), **retry** a failed/needs_attention job (`r`).
**Deferred to a follow-up (Phase 5b):** `edit-job` (needs the textarea input mode) and an in-TUI pipeline *builder* (`add job` + dependency checklist). Pipelines are authored via `agentctl pipeline create -f spec.yaml` (the primary path, also used by the lead Claude), so the in-TUI builder is lower value than the viewer/controller. The CLI `pipeline edit-job`/`retry` already exist from Phase 4b.

---

## File Structure

- **Modify** `internal/tui/model.go` — `api` interface (+3 pipeline methods); `Model.pipelines` field; `items()` composition + session filter; `Init`; `Update` (tick + new `pipelinesMsg`/`pipelineActionMsg` cases).
- **Modify** `internal/tui/cmds.go` — `pipelinesCmd`, `cancelPipelineCmd`, `retryJobCmd` + their msg types.
- **Modify** `internal/tui/list.go` — `item` variants (`pipeline`/`pjPipe`/`pjJob`); `itemKey`; pure `pipelineItems`; `renderItemLine` cases + `jobGlyph`.
- **Create** `internal/tui/pipeline_view.go` — pure `renderPipeline` (inline DAG).
- **Modify** `internal/tui/view.go` — detail dispatch for a selected pipeline row; footer/help text.
- **Modify** `internal/tui/keys.go` — `x` cancel pipeline, `r` retry job, `a` attach running job.
- **Modify** `internal/tui/model_test.go` — extend `fakeAPI` with the 3 pipeline methods + fields.
- **Modify** `docs/USAGE.md` — TUI pipeline keys.

---

## Task 1: Pipeline data plumbing (api, model field, polling, actions)

**Files:**
- Modify: `internal/tui/model.go`
- Modify: `internal/tui/cmds.go`
- Modify: `internal/tui/model_test.go` (extend `fakeAPI`)
- Test: `internal/tui/model_test.go`

- [ ] **Step 1: Extend the test fake + write the failing test**

In `internal/tui/model_test.go`, add fields to the `fakeAPI` struct and the three methods. Add to the struct (near its other fields like `sessions`/`terminated`):

```go
	pipelines []*pipeline.Pipeline
	retried   string // "<pid>/<job>" of the last PipelineRetry
	canceled  string // pid of the last PipelineCancel
```

Add the methods (and ensure the file imports `"github.com/srajanpathak/agentctl/internal/pipeline"`):

```go
func (f *fakeAPI) PipelineList(context.Context) ([]*pipeline.Pipeline, error) { return f.pipelines, nil }
func (f *fakeAPI) PipelineRetry(_ context.Context, pid, job string) error {
	f.retried = pid + "/" + job
	return nil
}
func (f *fakeAPI) PipelineCancel(_ context.Context, pid string) error {
	f.canceled = pid
	return nil
}
```

Append a test:

```go
func TestPipelinesMsgStoresPipelines(t *testing.T) {
	m := New(&fakeAPI{})
	updated, _ := m.Update(pipelinesMsg{pipelines: []*pipeline.Pipeline{
		{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}}},
	}})
	if got := updated.(Model).pipelines; len(got) != 1 || got[0].ID != "demo" {
		t.Fatalf("pipelines not stored: %+v", got)
	}
}

func TestPipelineActionMsgRefetches(t *testing.T) {
	m := New(&fakeAPI{})
	_, cmd := m.Update(pipelineActionMsg{err: nil})
	if cmd == nil {
		t.Fatalf("a pipeline action should trigger a refetch cmd")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestPipelinesMsg|TestPipelineActionMsg'`
Expected: FAIL — `undefined: pipelinesMsg` / `pipelineActionMsg`, and `*fakeAPI` won't satisfy `api` until the interface methods are added (build error).

- [ ] **Step 3: Add the api methods, model field, cmds, and Update handling**

In `internal/tui/model.go`, add the three methods to the `api` interface (after `Approve`):

```go
	Approve(ctx context.Context, id string, option int, fingerprint string) error
	PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error)
	PipelineRetry(ctx context.Context, pid, job string) error
	PipelineCancel(ctx context.Context, pid string) error
```

Add the `pipeline` import to `internal/tui/model.go`'s import block:

```go
	"github.com/srajanpathak/agentctl/internal/pipeline"
```

Add a field to the `Model` struct (near `approvals []approval.View`):

```go
	pipelines []*pipeline.Pipeline
```

Update `Init` to also poll pipelines:

```go
func (m Model) Init() tea.Cmd {
	return tea.Batch(listCmd(m.api), pipelinesCmd(m.api), tick())
}
```

Update the `tickMsg` case to refresh pipelines too:

```go
	case tickMsg:
		return m, tea.Batch(listCmd(m.api), outputCmd(m.api, m.selectedID()), approvalsCmd(m.api), pipelinesCmd(m.api), tick())
```

Add two new cases to the `Update` switch (e.g. after the `approveResultMsg` case):

```go
	case pipelinesMsg:
		if msg.err == nil {
			prevKey := m.selectedKey()
			m.pipelines = msg.pipelines
			m.repin(prevKey)
		}
		return m, nil

	case pipelineActionMsg:
		if msg.err != nil {
			m.status = "pipeline action failed: " + msg.err.Error()
		} else {
			m.status = ""
		}
		return m, pipelinesCmd(m.api)
```

In `internal/tui/cmds.go`, add the `pipeline` import, the msg types, and the commands (the client methods own their own HTTP timeouts, so a plain `context.Background()` is correct here):

```go
type pipelinesMsg struct {
	pipelines []*pipeline.Pipeline
	err       error
}
type pipelineActionMsg struct{ err error }

func pipelinesCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ps, err := a.PipelineList(context.Background())
		return pipelinesMsg{pipelines: ps, err: err}
	}
}

func cancelPipelineCmd(a api, pid string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineCancel(context.Background(), pid)}
	}
}

func retryJobCmd(a api, pid, job string) tea.Cmd {
	return func() tea.Msg {
		return pipelineActionMsg{err: a.PipelineRetry(context.Background(), pid, job)}
	}
}
```

(`internal/tui/cmds.go` already imports `context`; add `"github.com/srajanpathak/agentctl/internal/pipeline"`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestPipelinesMsg|TestPipelineActionMsg' && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/model.go internal/tui/cmds.go internal/tui/model_test.go
git commit -m "feat(tui): poll pipelines + cancel/retry commands"
```

---

## Task 2: item variants + items() composition

**Files:**
- Modify: `internal/tui/list.go`
- Modify: `internal/tui/model.go` (`items()`)
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/list_test.go`:

```go
func TestPipelineItems(t *testing.T) {
	ps := []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning}, {ID: "b", Status: pipeline.JobPending}}}}
	items := pipelineItems(ps)
	if len(items) != 3 {
		t.Fatalf("want 3 items (1 pipeline + 2 jobs), got %d", len(items))
	}
	if items[0].pipeline == nil || items[0].pipeline.ID != "demo" {
		t.Fatalf("first item should be the pipeline header: %+v", items[0])
	}
	if items[1].pjJob == nil || items[1].pjJob.ID != "a" || items[1].pjPipe != "demo" {
		t.Fatalf("second item should be job a: %+v", items[1])
	}
	// distinct job pointers (not aliasing the same loop var).
	if items[1].pjJob == items[2].pjJob {
		t.Fatalf("job items must hold distinct pointers")
	}
}

func TestItemsPrependsPipelinesAndFiltersOwnedSessions(t *testing.T) {
	m := New(&fakeAPI{})
	m.sessions = []*store.Session{
		{ID: "free", Status: store.StatusWorking},
		{ID: "demo-a", Status: store.StatusWorking, PipelineID: "demo", JobID: "a"},
	}
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobRunning, SessionID: "demo-a"}}}}
	items := m.items()
	// pipeline header + its 1 job + the free session; the pipeline-owned session is filtered out.
	var sawPipe, sawJob, sawFree, sawOwned bool
	for _, it := range items {
		if it.pipeline != nil {
			sawPipe = true
		}
		if it.pjJob != nil {
			sawJob = true
		}
		if it.session != nil && it.session.ID == "free" {
			sawFree = true
		}
		if it.session != nil && it.session.ID == "demo-a" {
			sawOwned = true
		}
	}
	if !sawPipe || !sawJob || !sawFree {
		t.Fatalf("missing rows: pipe=%v job=%v free=%v", sawPipe, sawJob, sawFree)
	}
	if sawOwned {
		t.Fatalf("pipeline-owned session must not appear as a flat session row")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestPipelineItems|TestItemsPrepends'`
Expected: FAIL — `undefined: pipelineItems`; `item` has no `pipeline`/`pjJob` fields.

- [ ] **Step 3: Add the item variants, itemKey, pipelineItems, and items()**

In `internal/tui/list.go`, extend the `item` struct:

```go
type item struct {
	session   *store.Session
	dir       string
	approvals bool // synthetic top-of-list inbox row
	apprCount int  // number of waiting agents (inbox row only)

	pipeline *pipeline.Pipeline // pipeline header row
	pjPipe   string             // pipelineJob row: owning pipeline id
	pjJob    *pipeline.Job      // pipelineJob row: the job
}
```

Add the `pipeline` import to `internal/tui/list.go`'s import block:

```go
	"github.com/srajanpathak/agentctl/internal/pipeline"
```

Extend `itemKey` (add the two new cases before the final `dirKey` return):

```go
func itemKey(it item) string {
	if it.approvals {
		return "approvals\x00"
	}
	if it.pipeline != nil {
		return "pipe\x00" + it.pipeline.ID
	}
	if it.pjJob != nil {
		return "pjob\x00" + it.pjPipe + "\x00" + it.pjJob.ID
	}
	if it.session != nil {
		return it.session.ID
	}
	return dirKey(it.dir)
}
```

Add the pure builder (anywhere in `list.go`, e.g. after `buildItems`):

```go
// pipelineItems flattens pipelines into a header row per pipeline followed by an
// indented row per job. Each job row holds a distinct *Job pointer.
func pipelineItems(ps []*pipeline.Pipeline) []item {
	var out []item
	for _, p := range ps {
		out = append(out, item{pipeline: p})
		for i := range p.Jobs {
			j := p.Jobs[i] // fresh var each iteration → distinct pointer
			out = append(out, item{pjPipe: p.ID, pjJob: &j})
		}
	}
	return out
}
```

In `internal/tui/model.go`, replace `items()` so it prepends the approvals row (unchanged) + the pipelines section, and filters pipeline-owned sessions out of the flat dir-grouped list:

```go
func (m Model) items() []item {
	// Pipeline-owned sessions are shown under their pipeline, not the flat list.
	flat := make([]*store.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.PipelineID == "" {
			flat = append(flat, s)
		}
	}
	base := buildItems(flat, m.openedDirs)

	var head []item
	if m.approvalsOn {
		head = append(head, item{approvals: true, apprCount: len(m.approvals)})
	}
	head = append(head, pipelineItems(m.pipelines)...)
	return append(head, base...)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/ -run 'TestPipelineItems|TestItemsPrepends' && go test ./internal/tui/...`
Expected: PASS (the new tests and all existing TUI tests — with no pipelines, `items()` is unchanged from before since `flat == m.sessions` and `head` is just the optional approvals row).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/model.go internal/tui/list_test.go
git commit -m "feat(tui): pipeline/pipelineJob item variants nested in the list"
```

---

## Task 3: Render pipeline + job rows in the list

**Files:**
- Modify: `internal/tui/list.go` (`renderItemLine`, `jobGlyph`)
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/list_test.go`:

```go
func TestRenderItemLinePipelineRows(t *testing.T) {
	head := renderItemLine(item{pipeline: &pipeline.Pipeline{ID: "demo", Status: pipeline.StatusRunning}}, false, 60)
	if !strings.Contains(head, "demo") || !strings.Contains(head, "▸") || !strings.Contains(head, "running") {
		t.Fatalf("pipeline header row wrong: %q", head)
	}
	jobRow := renderItemLine(item{pjPipe: "demo", pjJob: &pipeline.Job{ID: "a", Status: pipeline.JobDone, DependsOn: []string{"x"}}}, false, 60)
	if !strings.Contains(jobRow, "a") || !strings.Contains(jobRow, jobGlyph(pipeline.JobDone)) || !strings.Contains(jobRow, "x") {
		t.Fatalf("job row wrong: %q", jobRow)
	}
}

func TestJobGlyphDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []pipeline.JobStatus{pipeline.JobPending, pipeline.JobRunning, pipeline.JobDone, pipeline.JobFailed, pipeline.JobSkipped, pipeline.JobNeedsAttention} {
		g := jobGlyph(s)
		if g == "" || seen[g] {
			t.Fatalf("glyph for %s is empty or duplicate: %q", s, g)
		}
		seen[g] = true
	}
}
```

Ensure `internal/tui/list_test.go` imports `"strings"` (add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestRenderItemLinePipeline|TestJobGlyph'`
Expected: FAIL — `undefined: jobGlyph`; the pipeline/job rows render as blank (the `switch` has no case for them).

- [ ] **Step 3: Add the glyph helper + render cases**

In `internal/tui/list.go`, add the glyph helper:

```go
// jobGlyph maps a pipeline job status to a one-rune status glyph.
func jobGlyph(s pipeline.JobStatus) string {
	switch s {
	case pipeline.JobDone:
		return "●"
	case pipeline.JobRunning:
		return "◐"
	case pipeline.JobFailed:
		return "✗"
	case pipeline.JobNeedsAttention:
		return "⚠"
	case pipeline.JobSkipped:
		return "⊘"
	default: // pending
		return "○"
	}
}
```

Extend `renderItemLine`'s `switch` with two cases (place them before the `case it.session == nil:` case so they take precedence — pipeline/job rows also have a nil session):

```go
	switch {
	case it.approvals:
		txt := "⏳ Approvals (" + strconv.Itoa(it.apprCount) + ")"
		if it.apprCount == 0 {
			line = stMuted.Render(txt)
		} else {
			line = stStatus.Render(txt)
		}
	case it.pipeline != nil:
		line = stPaneTitle.Render("▸ "+it.pipeline.ID) + "  " + stMuted.Render(string(it.pipeline.Status))
	case it.pjJob != nil:
		deps := ""
		if len(it.pjJob.DependsOn) > 0 {
			deps = stMuted.Render("  (deps: " + strings.Join(it.pjJob.DependsOn, ",") + ")")
		}
		line = fmt.Sprintf("    %s %-12s %-13s", jobGlyph(it.pjJob.Status), trunc(it.pjJob.ID, 12), string(it.pjJob.Status)) + deps
	case it.session == nil:
		line = stMuted.Render("(no agents — n to spawn here)")
	default:
		// ... (existing session rendering, unchanged)
```

(Keep the rest of the `default` case exactly as-is.) Then extend the cursor-styling guard at the bottom of `renderItemLine` so pipeline/job rows highlight when selected:

```go
	cur := "  "
	if selected {
		cur = stCursor.Render("› ")
		if it.session != nil || it.approvals || it.pipeline != nil || it.pjJob != nil {
			line = stCursor.Render(line)
		}
	}
```

(`strings` and `fmt` are already imported in `list.go`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/list.go internal/tui/list_test.go
git commit -m "feat(tui): render pipeline header + job rows with status glyphs"
```

---

## Task 4: Inline DAG render + detail dispatch

**Files:**
- Create: `internal/tui/pipeline_view.go`
- Modify: `internal/tui/view.go`
- Test: `internal/tui/pipeline_view_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/tui/pipeline_view_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/pipeline"
)

func TestRenderPipeline(t *testing.T) {
	p := &pipeline.Pipeline{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "analyze", Status: pipeline.JobDone, Output: "found X"},
		{ID: "impl", Status: pipeline.JobRunning, DependsOn: []string{"analyze"}},
	}}
	out := renderPipeline(p, 60, 20)
	for _, want := range []string{"demo", "running", "analyze", "impl", "found X", "deps: analyze"} {
		if !strings.Contains(out, want) {
			t.Fatalf("renderPipeline missing %q in:\n%s", want, out)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run TestRenderPipeline`
Expected: FAIL — `undefined: renderPipeline`.

- [ ] **Step 3: Write `renderPipeline` + wire the detail dispatch**

Create `internal/tui/pipeline_view.go`:

```go
package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/pipeline"
)

// renderPipeline draws a pipeline's DAG in the detail pane when its header row is
// selected (mirrors renderApprovalsQueue). Read-only summary; actions come from
// keys handled by the model.
func renderPipeline(p *pipeline.Pipeline, width, height int) string {
	var b strings.Builder
	b.WriteString(stMuted.Render("pipeline "+p.ID+" — "+string(p.Status)) + "\n\n")
	for i := range p.Jobs {
		j := &p.Jobs[i]
		line := fmt.Sprintf("%s %-12s %-13s", jobGlyph(j.Status), trunc(j.ID, 12), string(j.Status))
		if len(j.DependsOn) > 0 {
			line += stMuted.Render("deps: " + strings.Join(j.DependsOn, ","))
		}
		b.WriteString(line + "\n")
		if j.Output != "" {
			b.WriteString("    " + stMuted.Render(trunc(j.Output, max(0, width-4))) + "\n")
		}
	}
	b.WriteString("\n" + stMuted.Render("x cancel pipeline · on a job: r retry · a attach"))
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}
```

In `internal/tui/view.go`, extend the detail dispatch in `View()`. Replace the `if cur.approvals { ... } else { ... }` block with a three-way branch:

```go
		cur := itemAt(m.items(), m.cursor)
		var detailTitle, detailBody string
		switch {
		case cur.approvals:
			detailTitle = "Approvals"
			detailBody = renderApprovalsQueue(m.approvals, m.apprCursor, m.apprFocused, detailOuter-2, bodyH-2)
		case cur.pipeline != nil:
			detailTitle = cur.pipeline.ID
			detailBody = renderPipeline(cur.pipeline, detailOuter-2, bodyH-2)
		default:
			detailTitle = m.selectedID()
			if detailTitle == "" {
				detailTitle = "—"
			}
			detailBody = renderDetail(m.selected(), m.vp, m.outputFocused, detailOuter-2)
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/... && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/pipeline_view.go internal/tui/view.go internal/tui/pipeline_view_test.go
git commit -m "feat(tui): inline pipeline DAG view in the detail pane"
```

---

## Task 5: Keys — attach running job, cancel pipeline, retry job

**Files:**
- Modify: `internal/tui/keys.go`
- Modify: `internal/tui/view.go` (footer + help text)
- Test: `internal/tui/list_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/list_test.go`:

```go
func pipeModel() Model {
	m := New(&fakeAPI{})
	m.ready = true
	m.pipelines = []*pipeline.Pipeline{{ID: "demo", Name: "demo", Status: pipeline.StatusRunning, Jobs: []pipeline.Job{
		{ID: "a", Status: pipeline.JobFailed, SessionID: "demo-a"},
		{ID: "b", Status: pipeline.JobRunning, SessionID: "demo-b"},
	}}}
	return m
}

func TestKeyCancelPipeline(t *testing.T) {
	m := pipeModel()
	m.cursor = 0 // the pipeline header row
	updated, cmd := m.handleKey(keyPress("x"))
	if cmd == nil {
		t.Fatalf("x on a pipeline row should return a cancel cmd")
	}
	cmd() // runs the command (calls the fake api)
	fa := updated.(Model).api.(*fakeAPI)
	if fa.canceled != "demo" {
		t.Fatalf("want canceled=demo, got %q", fa.canceled)
	}
}

func TestKeyRetryFailedJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 1 // job "a" (failed)
	_, cmd := m.handleKey(keyPress("r"))
	if cmd == nil {
		t.Fatalf("r on a failed job should return a retry cmd")
	}
	cmd()
	fa := m.api.(*fakeAPI)
	if fa.retried != "demo/a" {
		t.Fatalf("want retried=demo/a, got %q", fa.retried)
	}
}

func TestKeyRetryIgnoredOnRunningJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 2 // job "b" (running) — not retryable
	_, cmd := m.handleKey(keyPress("r"))
	if cmd != nil {
		t.Fatalf("r on a running job should be a no-op")
	}
}

func TestKeyAttachRunningJob(t *testing.T) {
	m := pipeModel()
	m.cursor = 2 // job "b" (running, has a session)
	_, cmd := m.handleKey(keyPress("a"))
	if cmd == nil {
		t.Fatalf("a on a running job should return an attach cmd")
	}
}
```

If a `keyPress` helper does not already exist in the TUI tests, add it to `internal/tui/list_test.go`:

```go
func keyPress(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
```

(Add the `tea "github.com/charmbracelet/bubbletea"` import to the test file if missing. If a `keyPress`/equivalent already exists, reuse it and skip this addition.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/tui/ -run 'TestKeyCancelPipeline|TestKeyRetry|TestKeyAttachRunningJob'`
Expected: FAIL — `x` returns no cancel cmd for a pipeline row, `r` is unhandled, `a` doesn't attach a job session.

- [ ] **Step 3: Add the key handlers**

In `internal/tui/keys.go`, add the `pipeline` import, then update the `modeNormal` switch. Replace the existing `case "x":` with a pipeline-aware version:

```go
		case "x":
			it := itemAt(m.items(), m.cursor)
			switch {
			case it.pipeline != nil:
				m.status = "canceling " + it.pipeline.ID
				return m, cancelPipelineCmd(m.api, it.pipeline.ID)
			case it.session != nil:
				m.mode = modeConfirmKill
			case it.dir != "":
				delete(m.openedDirs, it.dir)
				m.status = "closed " + abbrevHome(it.dir)
			}
			return m, nil
```

Replace the existing `case "a":` so it also attaches a running job's session:

```go
		case "a":
			it := itemAt(m.items(), m.cursor)
			if it.session != nil {
				return m, attachCmd(it.session.ID)
			}
			if it.pjJob != nil && it.pjJob.SessionID != "" {
				return m, attachCmd(it.pjJob.SessionID)
			}
			return m, nil
```

Add a new `case "r":` (retry a failed/needs_attention job):

```go
		case "r":
			it := itemAt(m.items(), m.cursor)
			if it.pjJob != nil && (it.pjJob.Status == pipeline.JobFailed || it.pjJob.Status == pipeline.JobNeedsAttention) {
				m.status = "retrying " + it.pjPipe + "/" + it.pjJob.ID
				return m, retryJobCmd(m.api, it.pjPipe, it.pjJob.ID)
			}
			return m, nil
```

In `internal/tui/view.go`, add the new keys to the footer hint and the help text:

- In `footer()`, append `· r retry` to the muted hint string.
- In `helpText()`, add lines:
  ```
  "  r            retry a failed/needs-attention pipeline job\n" +
  "  x            kill agent / cancel pipeline / close dir (context-sensitive)\n" +
  ```
  (and remove the now-superseded plain `x` line if present, or leave both — keep it readable).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/tui/... && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/keys.go internal/tui/view.go internal/tui/list_test.go
git commit -m "feat(tui): cancel pipeline (x), retry job (r), attach running job (a)"
```

---

## Task 6: Docs + full verification

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Document the TUI pipeline view**

In `docs/USAGE.md`, find the TUI/cockpit section (it lists the list-pane keys) and add a short note in the same style:

```markdown
Pipelines appear in the list pane under a **▸ Pipelines** section (one header row
per pipeline, then an indented row per job with a status glyph). Selecting a
pipeline header shows its DAG in the detail pane. On a pipeline row, `x` cancels
it; on a job row, `r` retries a failed/needs-attention job and `a` attaches to a
running job's session. (Authoring pipelines is via `agentctl pipeline create -f`;
editing job prompts and building pipelines in the TUI are not yet available.)
```

- [ ] **Step 2: Run the full suite**

Run: `go build ./... && go test ./... && make lint`
Expected: PASS across all packages; lint (go vet) clean. If anything fails, do NOT commit — report it.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document the TUI pipeline view + keys"
```

---

## Verification checklist (after all tasks)

- [ ] `go build ./...` clean; `go test ./...` green; `make lint` clean.
- [ ] Manual smoke (rebuild + restart daemon: `./scripts/reinstall.sh`), with at least one pipeline created (`agentctl pipeline create -f spec.yaml` + `start`):
  - Open the cockpit (`agentctl`). The list pane shows a **▸ Pipelines** section with the pipeline and its jobs (status glyphs); pipeline-owned agents are NOT duplicated in the flat agent list.
  - Select the pipeline header → the detail pane shows the DAG (jobs, statuses, deps, captured outputs).
  - Select a running job row, press `a` → the right pane attaches to that job's agent.
  - Press `x` on the pipeline header → it cancels (jobs go skipped, status canceled on next refresh).
  - On a failed/needs-attention job row, press `r` → it re-runs (status returns to running on refresh).

## Deferred to a follow-up (not in this plan)
- `edit-job` in the TUI (`e` → textarea pre-filled with the job prompt → `PipelineEditJob`).
- An in-TUI pipeline *builder* (`add job` + dependency checklist → `PipelineCreate`). Authoring stays on `agentctl pipeline create -f spec.yaml` for now.
```
