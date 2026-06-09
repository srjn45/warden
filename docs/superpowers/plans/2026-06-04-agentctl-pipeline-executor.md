# Pipeline DAG Executor (Phase 4) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A daemon-driven DAG executor: declare a pipeline of agent jobs in YAML, and the daemon lazily spawns each job when its dependencies complete, passing upstream outputs (and branch names) downstream, until the whole graph finishes.

**Architecture:** Pure logic lives in `internal/pipeline` (types, DAG validation, the `Plan` reconcile function, prompt composition, YAML parsing, and a per-file store — mirrors `internal/ctxstore`/`internal/mailbox`). The effectful `daemon.Executor` calls `pipeline.Plan` and performs spawns/skips via a new focused `lifecycle.SpawnJob`. The executor is driven by two triggers: each `pipeline emit` (a job declares itself done) and the existing `poller.OnTransition` hook (a job's session errored/orphaned → mark failed). No new polling loop.

**Tech Stack:** Go, chi, cobra, `gopkg.in/yaml.v3` (already in go.sum, indirect — becomes direct). Module: `github.com/srajanpathak/agentctl`.

## Key design decisions (confirmed with the user)

1. **Job spawn = a new `lifecycle.SpawnJob`** method, not an extension of `Spawn`. It reuses primitives (`worktreeRel`, `newAgentSession`, `claudeLaunch`, `killSession`, `rollbackWorktree`, a new `writePromptFile`) but with explicit worktree control and pipeline env — leaving the battle-tested `Spawn` untouched.
2. **Scope = working core.** `pipeline create/start/list/show/cancel` + `emit` + executor + worktree chaining. **Deferred to a Phase 4b:** `pipeline edit-job`, `pipeline retry`, and the "done-but-no-emit grace-window → needs-attention" fallback. In 4a, a job completes only via `emit`; a job's session going `errored`/`orphaned` marks it failed.
3. **Pipeline ID = its `name`** (validated path/tmux-safe + unique). No random ids — job session ids are the readable `"<pipeline>-<job>"`, and shared-context keys are `pipeline.<name>.<job>.output`.
4. **Executor location:** pure decisions in `internal/pipeline.Plan`; side effects in `internal/daemon.Executor`. The `Server` holds `*Executor`; daemon routes reach the pipeline store and `Reconcile` through it.

**Pipeline status values:** `pending` (created, not started) · `running` · `done` (all jobs done) · `stalled` (a job failed; its descendants skipped) · `canceled` (user canceled).
**Job status values:** `pending` · `running` · `done` · `failed` · `skipped`.

---

## File Structure

- **Create** `internal/pipeline/pipeline.go` — types (`Pipeline`, `Job`, `Status`, `JobStatus`), `Job()` lookup, `ParseWorktree`, `Validate`.
- **Create** `internal/pipeline/compose.go` — `ComposePrompt`.
- **Create** `internal/pipeline/plan.go` — `Plan` (pure reconcile decisions) + `Decision`.
- **Create** `internal/pipeline/spec.go` — `ParseSpec` (YAML → validated Pipeline).
- **Create** `internal/pipeline/store.go` — file-backed `Store` (Create/Get/List/Update).
- **Create** `internal/pipeline/*_test.go` — table tests for each of the above.
- **Modify** `internal/store/types.go` — add `PipelineID`, `JobID` to `Session`.
- **Modify** `internal/lifecycle/lifecycle.go` — `JobSpawnRequest` + `SpawnJob`; `newAgentSession` gains variadic extra env; add `writePromptFile`.
- **Modify** `internal/lifecycle/lifecycle_test.go` — `SpawnJob` tests.
- **Create** `internal/daemon/executor.go` — `Executor` (Reconcile + OnTransition + spawn effects).
- **Create** `internal/daemon/executor_test.go` — executor tests with fakes.
- **Create** `internal/daemon/pipeline_routes.go` — create/list/show/start/cancel/emit handlers.
- **Create** `internal/daemon/pipeline_routes_test.go` — route tests.
- **Modify** `internal/daemon/api.go` — `exec *Executor` field on `Server`; register pipeline routes.
- **Modify** `internal/daemon/lifecycle_adapter.go` + the `Lifecycle` interface (in `api.go`) + `internal/daemon/lifecycle_routes_test.go` fakeLife — add `SpawnJob`.
- **Modify** `internal/daemon/server.go` — `NewServer` gains an `*Executor` param.
- **Modify** `internal/cli/daemon.go` — build the pipeline store + `Executor`; compose `OnTransition`; pass to `NewServer`.
- **Modify** `internal/daemon/server_test.go` — update `NewServer(...)` call.
- **Modify** `internal/client/client.go` — pipeline client methods.
- **Create** `internal/cli/pipeline.go` — the `pipeline` command group.
- **Modify** `internal/cli/root.go` — register `newPipelineCmd()`.
- **Modify** `docs/USAGE.md` — pipelines section.

---

## Task 1: Session back-reference fields

**Files:**
- Modify: `internal/store/types.go`
- Test: `internal/store/` (existing tests still pass)

- [ ] **Step 1: Add the fields**

In `internal/store/types.go`, add two fields to the `Session` struct (after `Supervised`):

```go
	Supervised      bool   `json:"supervised"` // launched with --permission-mode acceptEdits (prompts) instead of bypass
	PipelineID      string `json:"pipeline_id,omitempty"` // set for pipeline jobs (back-ref)
	JobID           string `json:"job_id,omitempty"`      // set for pipeline jobs (back-ref)
```

- [ ] **Step 2: Verify build + existing tests**

Run: `go build ./... && go test ./internal/store/...`
Expected: PASS (additive fields; nothing else changes).

- [ ] **Step 3: Commit**

```bash
git add internal/store/types.go
git commit -m "feat(store): add PipelineID/JobID back-ref fields to Session"
```

---

## Task 2: pipeline types + Validate

**Files:**
- Create: `internal/pipeline/pipeline.go`
- Test: `internal/pipeline/pipeline_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/pipeline_test.go`:

```go
package pipeline

import (
	"strings"
	"testing"
)

func valid() *Pipeline {
	return &Pipeline{
		Name: "refactor-auth", Repo: "/repo",
		Jobs: []Job{
			{ID: "analyze", Prompt: "look", Worktree: "none"},
			{ID: "impl", Prompt: "do", DependsOn: []string{"analyze"}, Worktree: "fresh"},
			{ID: "review", Prompt: "merge", DependsOn: []string{"impl"}, Worktree: "from:impl"},
		},
	}
}

func TestValidateOK(t *testing.T) {
	if err := Validate(valid()); err != nil {
		t.Fatalf("valid pipeline rejected: %v", err)
	}
}

func TestValidateRejectsUnknownDep(t *testing.T) {
	p := valid()
	p.Jobs[1].DependsOn = []string{"ghost"}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown-dep error, got %v", err)
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	p := &Pipeline{Name: "p", Repo: "/r", Jobs: []Job{
		{ID: "a", Prompt: "x", DependsOn: []string{"b"}, Worktree: "none"},
		{ID: "b", Prompt: "x", DependsOn: []string{"a"}, Worktree: "none"},
	}}
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestValidateRejectsUnknownFromRef(t *testing.T) {
	p := valid()
	p.Jobs[2].Worktree = "from:ghost"
	if err := Validate(p); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want unknown from-ref error, got %v", err)
	}
}

func TestValidateRejectsBadIDs(t *testing.T) {
	if err := Validate(&Pipeline{Name: "bad/name", Repo: "/r", Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none"}}}); err == nil {
		t.Fatalf("want bad pipeline name error")
	}
	if err := Validate(&Pipeline{Name: "ok", Repo: "/r", Jobs: []Job{{ID: "a/b", Prompt: "x", Worktree: "none"}}}); err == nil {
		t.Fatalf("want bad job id error")
	}
	if err := Validate(&Pipeline{Name: "ok", Repo: "/r", Jobs: []Job{{ID: "a", Prompt: "", Worktree: "none"}}}); err == nil {
		t.Fatalf("want empty-prompt error")
	}
}

func TestParseWorktree(t *testing.T) {
	mode, from := ParseWorktree("from:impl")
	if mode != "from" || from != "impl" {
		t.Fatalf("got %q %q", mode, from)
	}
	if m, _ := ParseWorktree("fresh"); m != "fresh" {
		t.Fatalf("fresh")
	}
}

func TestJobLookup(t *testing.T) {
	p := valid()
	if p.Job("impl") == nil || p.Job("nope") != nil {
		t.Fatalf("Job() lookup wrong")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/...`
Expected: FAIL — package has no buildable files / undefined symbols.

- [ ] **Step 3: Write minimal implementation**

Create `internal/pipeline/pipeline.go`:

```go
// Package pipeline models a DAG of agent jobs and the pure logic that drives it:
// validation, reconcile planning, prompt composition, YAML parsing, and a
// file-backed store. All decision logic is side-effect-free; the daemon's
// Executor performs the actual spawns.
package pipeline

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/store"
)

type Status string

const (
	StatusPending  Status = "pending"
	StatusRunning  Status = "running"
	StatusDone     Status = "done"
	StatusStalled  Status = "stalled"
	StatusCanceled Status = "canceled"
)

type JobStatus string

const (
	JobPending JobStatus = "pending"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobFailed  JobStatus = "failed"
	JobSkipped JobStatus = "skipped"
)

// Job is one node in the DAG. The first block is author-supplied; the second is
// filled at runtime by the executor and emit.
type Job struct {
	ID         string    `json:"id" yaml:"id"`
	Prompt     string    `json:"prompt" yaml:"prompt"`
	DependsOn  []string  `json:"depends_on,omitempty" yaml:"depends_on,omitempty"`
	Handoff    string    `json:"handoff,omitempty" yaml:"handoff,omitempty"`
	Worktree   string    `json:"worktree" yaml:"worktree"` // none | fresh | from:<jobid>
	Supervised bool      `json:"supervised,omitempty" yaml:"supervised,omitempty"`
	Type       string    `json:"type,omitempty" yaml:"type,omitempty"`

	SessionID string    `json:"session_id,omitempty" yaml:"-"`
	Status    JobStatus `json:"status,omitempty" yaml:"-"`
	Output    string    `json:"output,omitempty" yaml:"-"`
	Branch    string    `json:"branch,omitempty" yaml:"-"`
}

type Pipeline struct {
	ID     string `json:"id" yaml:"-"` // == Name; stable key
	Name   string `json:"name" yaml:"name"`
	Repo   string `json:"repo" yaml:"repo"`
	Status Status `json:"status" yaml:"-"`
	Jobs   []Job  `json:"jobs" yaml:"jobs"`
}

// Job returns a pointer to the job with id, or nil.
func (p *Pipeline) Job(id string) *Job {
	for i := range p.Jobs {
		if p.Jobs[i].ID == id {
			return &p.Jobs[i]
		}
	}
	return nil
}

// ParseWorktree splits a worktree spec into (mode, fromJob). "from:impl" ->
// ("from","impl"); "fresh"/"none" -> (mode, "").
func ParseWorktree(s string) (mode, fromJob string) {
	if strings.HasPrefix(s, "from:") {
		return "from", strings.TrimPrefix(s, "from:")
	}
	return s, ""
}

// Validate checks the DAG is well-formed: safe unique ids, non-empty prompts,
// known dependency + from-ref targets, valid worktree modes, and no cycles.
func Validate(p *Pipeline) error {
	if err := store.SafeID(p.Name); err != nil {
		return fmt.Errorf("invalid pipeline name %q: must have no '/', '\\', ':', or '..'", p.Name)
	}
	if p.Repo == "" {
		return fmt.Errorf("pipeline repo is required")
	}
	if len(p.Jobs) == 0 {
		return fmt.Errorf("pipeline has no jobs")
	}
	ids := map[string]bool{}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if err := store.SafeID(j.ID); err != nil {
			return fmt.Errorf("invalid job id %q: must have no '/', '\\', ':', or '..'", j.ID)
		}
		if ids[j.ID] {
			return fmt.Errorf("duplicate job id %q", j.ID)
		}
		ids[j.ID] = true
		if strings.TrimSpace(j.Prompt) == "" {
			return fmt.Errorf("job %q: prompt is required", j.ID)
		}
		switch mode, _ := ParseWorktree(j.Worktree); mode {
		case "none", "fresh", "from":
		default:
			return fmt.Errorf("job %q: invalid worktree %q (want none|fresh|from:<job>)", j.ID, j.Worktree)
		}
	}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		for _, dep := range j.DependsOn {
			if dep == j.ID {
				return fmt.Errorf("job %q depends on itself", j.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("job %q depends on unknown job %q", j.ID, dep)
			}
		}
		if mode, from := ParseWorktree(j.Worktree); mode == "from" {
			if !ids[from] {
				return fmt.Errorf("job %q worktree references unknown job %q", j.ID, from)
			}
		}
	}
	return detectCycle(p)
}

// detectCycle reports a dependency cycle via DFS over the depends_on edges.
func detectCycle(p *Pipeline) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		color[id] = gray
		j := p.Job(id)
		for _, dep := range j.DependsOn {
			switch color[dep] {
			case gray:
				return fmt.Errorf("dependency cycle through job %q", dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[id] = black
		return nil
	}
	for i := range p.Jobs {
		if color[p.Jobs[i].ID] == white {
			if err := visit(p.Jobs[i].ID); err != nil {
				return err
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pipeline/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/pipeline.go internal/pipeline/pipeline_test.go
git commit -m "feat(pipeline): DAG types + Validate (cycles, refs, safe ids)"
```

---

## Task 3: ComposePrompt

**Files:**
- Create: `internal/pipeline/compose.go`
- Test: `internal/pipeline/compose_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/compose_test.go`:

```go
package pipeline

import (
	"strings"
	"testing"
)

func TestComposePromptWithUpstreamAndFooter(t *testing.T) {
	p := &Pipeline{ID: "refactor-auth", Name: "refactor-auth", Repo: "/r", Jobs: []Job{
		{ID: "analyze", Prompt: "look", Worktree: "none", Status: JobDone, Output: "found X", Branch: ""},
		{ID: "impl", Prompt: "do the work", DependsOn: []string{"analyze"}, Worktree: "fresh", Handoff: "the branch name"},
	}}
	out := ComposePrompt(p, p.Job("impl"))

	if !strings.Contains(out, "Upstream output — job `analyze`") || !strings.Contains(out, "found X") {
		t.Fatalf("missing upstream block:\n%s", out)
	}
	if !strings.Contains(out, "do the work") {
		t.Fatalf("missing job prompt")
	}
	if !strings.Contains(out, "agentctl pipeline emit") || !strings.Contains(out, "job `impl`") || !strings.Contains(out, "pipeline `refactor-auth`") {
		t.Fatalf("missing/incorrect footer:\n%s", out)
	}
	if !strings.Contains(out, "the branch name") {
		t.Fatalf("handoff hint not included")
	}
}

func TestComposePromptNoDepsNoUpstreamBlock(t *testing.T) {
	p := &Pipeline{ID: "p", Name: "p", Jobs: []Job{{ID: "a", Prompt: "start", Worktree: "none"}}}
	out := ComposePrompt(p, p.Job("a"))
	if strings.Contains(out, "Upstream output") {
		t.Fatalf("should have no upstream block:\n%s", out)
	}
	if !strings.Contains(out, "agentctl pipeline emit") {
		t.Fatalf("footer always present")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestComposePrompt`
Expected: FAIL — `undefined: ComposePrompt`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/pipeline/compose.go`:

```go
package pipeline

import (
	"fmt"
	"strings"
)

// ComposePrompt builds the full prompt the daemon types into a job's agent:
// upstream outputs (for dependencies that produced one) + the job's own prompt +
// a footer telling it how to emit its handoff.
func ComposePrompt(p *Pipeline, job *Job) string {
	var b strings.Builder
	for _, dep := range job.DependsOn {
		up := p.Job(dep)
		if up == nil || up.Output == "" {
			continue
		}
		fmt.Fprintf(&b, "### Upstream output — job `%s`:\n%s\n", dep, up.Output)
		if up.Branch != "" {
			fmt.Fprintf(&b, "(branch: `%s`)\n", up.Branch)
		}
		b.WriteString("\n")
	}
	b.WriteString(job.Prompt)
	fmt.Fprintf(&b, "\n\n---\nYou are job `%s` in pipeline `%s`. When your task is complete, "+
		"publish your handoff for downstream jobs by running:\n"+
		"  agentctl pipeline emit \"<your handoff text>\"\n", job.ID, p.ID)
	if strings.TrimSpace(job.Handoff) != "" {
		fmt.Fprintf(&b, "Include specifically: %s\n", job.Handoff)
	}
	return b.String()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pipeline/ -run TestComposePrompt`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/compose.go internal/pipeline/compose_test.go
git commit -m "feat(pipeline): ComposePrompt (upstream block + emit footer)"
```

---

## Task 4: Plan (pure reconcile decisions)

**Files:**
- Create: `internal/pipeline/plan.go`
- Test: `internal/pipeline/plan_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/plan_test.go`:

```go
package pipeline

import (
	"reflect"
	"sort"
	"testing"
)

func diamond(statuses map[string]JobStatus) *Pipeline {
	mk := func(id string, deps ...string) Job {
		return Job{ID: id, Prompt: "x", Worktree: "none", DependsOn: deps, Status: statuses[id]}
	}
	return &Pipeline{ID: "p", Name: "p", Repo: "/r", Jobs: []Job{
		mk("a"), mk("b", "a"), mk("c", "a"), mk("d", "b", "c"),
	}}
}

func sortedEqual(t *testing.T, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPlanSpawnsRootsThenUnblocks(t *testing.T) {
	// nothing done yet → only the root (a) is spawnable.
	d := Plan(diamond(map[string]JobStatus{"a": JobPending, "b": JobPending, "c": JobPending, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"a"})
	if d.Status != StatusRunning {
		t.Fatalf("status %s", d.Status)
	}

	// a done → b and c spawnable.
	d = Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobPending, "c": JobPending, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"b", "c"})

	// b and c done → d spawnable.
	d = Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobDone, "c": JobDone, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"d"})
}

func TestPlanRunningJobIsNotRespawned(t *testing.T) {
	d := Plan(diamond(map[string]JobStatus{"a": JobRunning, "b": JobPending, "c": JobPending, "d": JobPending}))
	if len(d.Spawn) != 0 {
		t.Fatalf("running root must not respawn or unblock: %v", d.Spawn)
	}
}

func TestPlanAllDone(t *testing.T) {
	d := Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobDone, "c": JobDone, "d": JobDone}))
	if d.Status != StatusDone || len(d.Spawn) != 0 {
		t.Fatalf("got %+v", d)
	}
}

func TestPlanFailureSkipsDescendantsAndStalls(t *testing.T) {
	// b failed → d (its descendant) is skipped; c may still run.
	d := Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobFailed, "c": JobPending, "d": JobPending}))
	sortedEqual(t, d.Spawn, []string{"c"})
	sortedEqual(t, d.Skip, []string{"d"})
	if d.Status != StatusStalled {
		t.Fatalf("status %s", d.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestPlan`
Expected: FAIL — `undefined: Plan`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/pipeline/plan.go`:

```go
package pipeline

// Decision is the pure output of Plan: which pending jobs are ready to spawn,
// which pending jobs must be skipped (a failed ancestor), and the resulting
// pipeline status. It performs no side effects.
type Decision struct {
	Spawn  []string // job ids ready to spawn (deps all done)
	Skip   []string // job ids to mark skipped (descendant of a failed job)
	Status Status
}

// Plan computes the next reconcile decision from current job statuses.
func Plan(p *Pipeline) Decision {
	status := map[string]JobStatus{}
	for i := range p.Jobs {
		status[p.Jobs[i].ID] = p.Jobs[i].Status
	}

	// Transitively mark descendants of any failed job for skipping (only those
	// still pending — a job already running/done is left as-is).
	skip := map[string]bool{}
	changed := true
	for changed {
		changed = false
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if status[j.ID] != JobPending || skip[j.ID] {
				continue
			}
			for _, dep := range j.DependsOn {
				if status[dep] == JobFailed || skip[dep] {
					skip[j.ID] = true
					changed = true
					break
				}
			}
		}
	}

	var d Decision
	for id := range skip {
		d.Skip = append(d.Skip, id)
	}
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if status[j.ID] != JobPending || skip[j.ID] {
			continue
		}
		ready := true
		for _, dep := range j.DependsOn {
			if status[dep] != JobDone {
				ready = false
				break
			}
		}
		if ready {
			d.Spawn = append(d.Spawn, j.ID)
		}
	}

	d.Status = pipelineStatus(p, status, skip, len(d.Spawn))
	return d
}

func pipelineStatus(p *Pipeline, status map[string]JobStatus, skip map[string]bool, spawnable int) Status {
	anyFailed, anyRunning, allTerminal := false, false, true
	for i := range p.Jobs {
		s := status[p.Jobs[i].ID]
		if s == JobFailed {
			anyFailed = true
		}
		if s == JobRunning {
			anyRunning = true
		}
		// "terminal" for completion purposes = done/failed/skipped (incl. about-to-skip).
		if !(s == JobDone || s == JobFailed || s == JobSkipped || skip[p.Jobs[i].ID]) {
			allTerminal = false
		}
	}
	switch {
	case anyFailed:
		return StatusStalled
	case allTerminal:
		return StatusDone
	case anyRunning || spawnable > 0:
		return StatusRunning
	default:
		return StatusPending
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pipeline/ -run TestPlan`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/plan.go internal/pipeline/plan_test.go
git commit -m "feat(pipeline): pure Plan (ready-set, failure-skip, status)"
```

---

## Task 5: YAML spec parsing

**Files:**
- Create: `internal/pipeline/spec.go`
- Test: `internal/pipeline/spec_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/spec_test.go`:

```go
package pipeline

import "testing"

const sampleYAML = `
name: refactor-auth
repo: /repo
jobs:
  - id: analyze
    prompt: "look at auth"
    worktree: none
  - id: impl
    prompt: "do it"
    depends_on: [analyze]
    worktree: fresh
    handoff: "the branch name"
`

func TestParseSpec(t *testing.T) {
	p, err := ParseSpec([]byte(sampleYAML))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if p.ID != "refactor-auth" || p.Name != "refactor-auth" || p.Repo != "/repo" {
		t.Fatalf("header wrong: %+v", p)
	}
	if p.Status != StatusPending {
		t.Fatalf("new pipeline should be pending, got %s", p.Status)
	}
	if len(p.Jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(p.Jobs))
	}
	a := p.Job("analyze")
	if a.Status != JobPending || a.Type != "development" {
		t.Fatalf("defaults wrong: %+v", a)
	}
	if p.Job("impl").Handoff != "the branch name" {
		t.Fatalf("handoff not parsed")
	}
}

func TestParseSpecDefaultsWorktreeNone(t *testing.T) {
	p, err := ParseSpec([]byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n"))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if p.Job("a").Worktree != "none" {
		t.Fatalf("blank worktree should default to none, got %q", p.Job("a").Worktree)
	}
}

func TestParseSpecInvalidRejected(t *testing.T) {
	if _, err := ParseSpec([]byte("name: p\nrepo: /r\njobs:\n  - id: a\n    prompt: x\n    depends_on: [ghost]\n")); err == nil {
		t.Fatalf("expected validation error for unknown dep")
	}
	if _, err := ParseSpec([]byte("not: valid: yaml: [")); err == nil {
		t.Fatalf("expected yaml parse error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestParseSpec`
Expected: FAIL — `undefined: ParseSpec`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/pipeline/spec.go`:

```go
package pipeline

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseSpec decodes a pipeline YAML spec, applies defaults (worktree=none,
// type=development, all statuses pending), sets ID=Name, and validates the DAG.
func ParseSpec(data []byte) (*Pipeline, error) {
	var p Pipeline
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pipeline yaml: %w", err)
	}
	p.ID = p.Name
	p.Status = StatusPending
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if j.Worktree == "" {
			j.Worktree = "none"
		}
		if j.Type == "" {
			j.Type = "development"
		}
		j.Status = JobPending
	}
	if err := Validate(&p); err != nil {
		return nil, err
	}
	return &p, nil
}
```

- [ ] **Step 4: Run test to verify it passes, and tidy modules**

Run: `go test ./internal/pipeline/ -run TestParseSpec`
Expected: PASS.

Run: `go mod tidy && go build ./...`
Expected: clean (this promotes `gopkg.in/yaml.v3` from indirect to a direct dependency).

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/spec.go internal/pipeline/spec_test.go go.mod go.sum
git commit -m "feat(pipeline): ParseSpec (YAML -> validated Pipeline)"
```

---

## Task 6: pipeline Store (file-backed)

**Files:**
- Create: `internal/pipeline/store.go`
- Test: `internal/pipeline/store_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/store_test.go`:

```go
package pipeline

import (
	"errors"
	"testing"
)

func TestStoreCreateGetList(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	p := &Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}}
	if err := s.Create(p); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create(p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate Create want ErrExists, got %v", err)
	}
	got, err := s.Get("p1")
	if err != nil || got.Name != "p1" || len(got.Jobs) != 1 {
		t.Fatalf("Get: %+v err=%v", got, err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get want ErrNotFound, got %v", err)
	}
	all, _ := s.List()
	if len(all) != 1 {
		t.Fatalf("List want 1, got %d", len(all))
	}
}

func TestStoreUpdate(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Create(&Pipeline{ID: "p1", Name: "p1", Repo: "/r", Status: StatusPending,
		Jobs: []Job{{ID: "a", Prompt: "x", Worktree: "none", Status: JobPending}}})
	if err := s.Update("p1", func(p *Pipeline) {
		p.Status = StatusRunning
		p.Job("a").Status = JobDone
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := s.Get("p1")
	if got.Status != StatusRunning || got.Job("a").Status != JobDone {
		t.Fatalf("update not persisted: %+v", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestStore`
Expected: FAIL — `undefined: NewStore`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/pipeline/store.go`:

```go
package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/srajanpathak/agentctl/internal/store"
)

var (
	ErrNotFound = errors.New("pipeline not found")
	ErrExists   = errors.New("pipeline already exists")
)

// Store persists each pipeline as one JSON file (<dir>/<id>.json), mutated
// atomically under a mutex — mirrors internal/ctxstore.
type Store struct {
	mu  sync.Mutex
	dir string
}

func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) path(id string) (string, error) {
	if err := store.SafeID(id); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, id+".json"), nil
}

func (s *Store) read(path string) (*Pipeline, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var p Pipeline
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) write(path string, p *Pipeline) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func (s *Store) Create(p *Pipeline) error {
	path, err := s.path(p.ID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return ErrExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.write(path, p)
}

func (s *Store) Get(id string) (*Pipeline, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(path)
}

func (s *Store) List() ([]*Pipeline, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := []*Pipeline{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p, err := s.read(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update applies fn to the stored pipeline under the lock and writes it back.
func (s *Store) Update(id string, fn func(*Pipeline)) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.read(path)
	if err != nil {
		return err
	}
	fn(p)
	return s.write(path, p)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pipeline/...`
Expected: PASS (all pipeline tests).

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/store.go internal/pipeline/store_test.go
git commit -m "feat(pipeline): file-backed Store (Create/Get/List/Update)"
```

---

## Task 7: lifecycle.SpawnJob

**Files:**
- Modify: `internal/lifecycle/lifecycle.go`
- Test: `internal/lifecycle/lifecycle_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/lifecycle/lifecycle_test.go`:

```go
func TestSpawnJobFreshWorktreeAndEnv(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	lc.PromptsDir = "/tmp/prompts"
	s, err := lc.SpawnJob(context.Background(), JobSpawnRequest{
		PipelineID: "refactor", JobID: "impl", Repo: "/repo",
		Prompt: "do the work", Worktree: true, Type: store.TypeDevelopment,
	})
	if err != nil {
		t.Fatalf("SpawnJob: %v", err)
	}
	if s.ID != "refactor-impl" || s.PipelineID != "refactor" || s.JobID != "impl" {
		t.Fatalf("session ids wrong: %+v", s)
	}
	if s.Worktree != ".worktrees/refactor-impl" || s.Branch != "refactor-impl" {
		t.Fatalf("worktree/branch wrong: %+v", s)
	}
	// worktree created off HEAD (no base ref).
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", ".worktrees/refactor-impl", "-b", "refactor-impl"})
	// tmux session carries all three identity env vars.
	require.Contains(t, fr.calledArgs(), []string{
		"tmux", "new-session", "-d", "-s", "refactor-impl",
		"-e", "AGENTCTL_SESSION_ID=refactor-impl",
		"-e", "AGENTCTL_PIPELINE_ID=refactor",
		"-e", "AGENTCTL_JOB_ID=impl",
		"-c", "/repo/.worktrees/refactor-impl",
	})
}

func TestSpawnJobFromBaseBranch(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	lc.PromptsDir = "/tmp/prompts"
	_, err := lc.SpawnJob(context.Background(), JobSpawnRequest{
		PipelineID: "p", JobID: "review", Repo: "/repo",
		Prompt: "merge", Worktree: true, BaseBranch: "p-impl", Type: store.TypeDevelopment,
	})
	if err != nil {
		t.Fatalf("SpawnJob: %v", err)
	}
	// worktree branched off the upstream branch.
	require.Contains(t, fr.calledArgs(), []string{"git", "worktree", "add", ".worktrees/p-review", "-b", "p-review", "p-impl"})
}

func TestSpawnJobNoneRunsInRepoRoot(t *testing.T) {
	fr := &FakeRunner{}
	lc := New(fr)
	lc.PromptsDir = "/tmp/prompts"
	s, err := lc.SpawnJob(context.Background(), JobSpawnRequest{
		PipelineID: "p", JobID: "analyze", Repo: "/repo",
		Prompt: "look", Worktree: false, Type: store.TypeAnalysis,
	})
	if err != nil {
		t.Fatalf("SpawnJob: %v", err)
	}
	if s.Worktree != "" || s.Workdir != "/repo" {
		t.Fatalf("none-mode should run in repo root with no worktree: %+v", s)
	}
	for _, a := range fr.calledArgs() {
		require.NotEqual(t, "worktree", argAt(a, 1), "none-mode must not create a worktree")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/lifecycle/ -run TestSpawnJob`
Expected: FAIL — `undefined: JobSpawnRequest` / `lc.SpawnJob undefined`.

- [ ] **Step 3a: Make `newAgentSession` accept extra env**

In `internal/lifecycle/lifecycle.go`, change `newAgentSession` to take variadic extra env and build the args (existing callers pass no extra env, so their call sites and the Phase-3 argv are unchanged):

```go
func (l *Lifecycle) newAgentSession(ctx context.Context, runDir, id, cwd string, env ...string) error {
	l.ensureScrollback(ctx) // before new-session: the new pane inherits the limit
	// -e sets AGENTCTL_SESSION_ID (+ any extra pipeline env) in the session
	// environment so the agent's shell tools know which agent they are.
	args := []string{"new-session", "-d", "-s", id, "-e", "AGENTCTL_SESSION_ID=" + id}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, "-c", cwd)
	if out, err := l.run.Run(ctx, runDir, "tmux", args...); err != nil {
		return fmt.Errorf("tmux new-session: %w: %s", err, out)
	}
	// (existing mouse/option-setting code below stays unchanged)
```

Leave the rest of `newAgentSession` (the mouse/history option calls) exactly as-is.

- [ ] **Step 3b: Add `writePromptFile` helper + `JobSpawnRequest` + `SpawnJob`**

Add to `internal/lifecycle/lifecycle.go`:

```go
// JobSpawnRequest spawns one pipeline job. The executor composes Prompt and
// resolves Worktree/BaseBranch before calling.
type JobSpawnRequest struct {
	PipelineID string
	JobID      string
	Repo       string
	Prompt     string     // already composed (upstream context + footer)
	Worktree   bool       // create a git worktree? false = run in repo root
	BaseBranch string     // worktree base ref ("" = off HEAD); ignored when Worktree is false
	Type       store.Type
	Supervised bool
}

// writePromptFile persists prompt to <PromptsDir>/<id> and returns the path, so
// a multi-line prompt is launched via "$(cat file)" as a single typed line.
func (l *Lifecycle) writePromptFile(ctx context.Context, id, prompt string) (string, error) {
	if l.PromptsDir == "" {
		return "", fmt.Errorf("prompts dir not configured")
	}
	if out, err := l.run.Run(ctx, "", "mkdir", "-p", l.PromptsDir); err != nil {
		return "", fmt.Errorf("mkdir prompts dir: %w: %s", err, out)
	}
	path := filepath.Join(l.PromptsDir, id)
	if out, err := l.run.Run(ctx, "", "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", prompt, path); err != nil {
		return "", fmt.Errorf("write prompt file: %w: %s", err, out)
	}
	return path, nil
}

// SpawnJob launches one pipeline-job agent: optionally creating a git worktree
// (off HEAD or off BaseBranch), starting a tmux session with the pipeline
// identity env, and auto-typing the composed prompt into claude.
func (l *Lifecycle) SpawnJob(ctx context.Context, req JobSpawnRequest) (*store.Session, error) {
	id := req.PipelineID + "-" + req.JobID
	if err := store.SafeID(id); err != nil {
		return nil, fmt.Errorf("invalid job session id %q: %w", id, err)
	}
	sess := &store.Session{
		ID: id, TmuxSession: id, Type: req.Type, Repo: req.Repo,
		Prompt: req.Prompt, Subject: firstWords(req.Prompt, 10),
		Status: store.StatusSpawning, Supervised: req.Supervised,
		PipelineID: req.PipelineID, JobID: req.JobID,
	}
	sess.ClaudeSessionID = store.NewSessionID()

	workdir := req.Repo
	worktreeCreated := false
	if req.Worktree {
		rel := worktreeRel(id)
		add := []string{"worktree", "add", rel, "-b", id}
		if req.BaseBranch != "" {
			add = append(add, req.BaseBranch)
		}
		if out, err := l.run.Run(ctx, req.Repo, "git", add...); err != nil {
			return nil, fmt.Errorf("git worktree add: %w: %s", err, out)
		}
		sess.Worktree = rel
		sess.Branch = id
		worktreeCreated = true
		workdir = filepath.Join(req.Repo, rel)
	}
	sess.Workdir = workdir

	if err := l.newAgentSession(ctx, req.Repo, id, workdir,
		"AGENTCTL_PIPELINE_ID="+req.PipelineID, "AGENTCTL_JOB_ID="+req.JobID); err != nil {
		if worktreeCreated {
			l.rollbackWorktree(sess)
		}
		return nil, err
	}

	promptFile, err := l.writePromptFile(ctx, id, req.Prompt)
	if err != nil {
		l.killSession(id)
		if worktreeCreated {
			l.rollbackWorktree(sess)
		}
		return nil, err
	}
	launch := claudeLaunch(sess.ClaudeSessionID, id, req.Supervised) + ` "$(cat ` + shellQuoteArg(promptFile) + `)"`
	if out, err := l.run.Run(ctx, req.Repo, "tmux", "send-keys", "-t", id, launch, "Enter"); err != nil {
		l.killSession(id)
		if worktreeCreated {
			l.rollbackWorktree(sess)
		}
		return nil, fmt.Errorf("tmux send-keys claude: %w: %s", err, out)
	}
	return sess, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lifecycle/...`
Expected: PASS (the 3 new `SpawnJob` tests plus all existing tests — the Phase-3 `new-session` argv is unchanged because no extra env is passed by existing callers).

- [ ] **Step 5: Commit**

```bash
git add internal/lifecycle/lifecycle.go internal/lifecycle/lifecycle_test.go
git commit -m "feat(lifecycle): SpawnJob (worktree strategy + pipeline env + composed prompt)"
```

---

## Task 8: Add SpawnJob to the daemon Lifecycle interface

**Files:**
- Modify: `internal/daemon/api.go` (the `Lifecycle` interface)
- Modify: `internal/daemon/lifecycle_adapter.go`
- Modify: `internal/daemon/lifecycle_routes_test.go` (the `fakeLife` test double)

- [ ] **Step 1: Add the method to the interface**

In `internal/daemon/api.go`, add to the `Lifecycle` interface (after `SendKeys`):

```go
	SendKeys(ctx context.Context, tmuxSession, key string) error
	// SpawnJob launches one pipeline-job agent (worktree strategy + pipeline env).
	SpawnJob(ctx context.Context, req lifecycle.JobSpawnRequest) (*store.Session, error)
```

Ensure `internal/daemon/api.go` imports `"github.com/srajanpathak/agentctl/internal/lifecycle"` (it likely already does for `SpawnRequest`; if not, add it).

- [ ] **Step 2: Run build to verify it fails**

Run: `go build ./... 2>&1 | head` and `go vet ./internal/daemon/ 2>&1 | head`
Expected: FAIL — `*lifecycleAdapter` and `*fakeLife` no longer satisfy `Lifecycle` (missing `SpawnJob`).

- [ ] **Step 3: Implement the adapter + fake**

In `internal/daemon/lifecycle_adapter.go`, add:

```go
func (a *lifecycleAdapter) SpawnJob(ctx context.Context, req lifecycle.JobSpawnRequest) (*store.Session, error) {
	return a.lc.SpawnJob(ctx, req)
}
```

In `internal/daemon/lifecycle_routes_test.go`, add to the `fakeLife` (near its other methods like `SendKeys`). Add a `spawnedJob *store.Session` field if useful, but minimally:

```go
func (f *fakeLife) SpawnJob(_ context.Context, req lifecycle.JobSpawnRequest) (*store.Session, error) {
	id := req.PipelineID + "-" + req.JobID
	branch := ""
	wt := ""
	if req.Worktree {
		branch = id
		wt = ".worktrees/" + id
	}
	return &store.Session{
		ID: id, TmuxSession: id, Type: req.Type, Repo: req.Repo,
		Status: store.StatusSpawning, PipelineID: req.PipelineID, JobID: req.JobID,
		Branch: branch, Worktree: wt,
	}, nil
}
```

Ensure `internal/daemon/lifecycle_routes_test.go` imports `"github.com/srajanpathak/agentctl/internal/lifecycle"` (it already references `lifecycle.AdoptRequest` etc., so it does).

- [ ] **Step 4: Run build + tests to verify they pass**

Run: `go build ./... && go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/api.go internal/daemon/lifecycle_adapter.go internal/daemon/lifecycle_routes_test.go
git commit -m "feat(daemon): add SpawnJob to the Lifecycle interface + adapter + fake"
```

---

## Task 9: daemon Executor (reconcile + spawn effects + failure)

**Files:**
- Create: `internal/daemon/executor.go`
- Test: `internal/daemon/executor_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/executor_test.go`:

```go
package daemon

import (
	"context"
	"testing"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
)

func newTestExecutor(t *testing.T) (*Executor, *pipeline.Store, *fakeStore) {
	t.Helper()
	ps, err := pipeline.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("pipeline.NewStore: %v", err)
	}
	cs, err := ctxstore.New(t.TempDir())
	if err != nil {
		t.Fatalf("ctxstore.New: %v", err)
	}
	ss := newFakeStore()
	e := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	return e, ps, ss
}

func chain() *pipeline.Pipeline {
	return &pipeline.Pipeline{ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusPending,
		Jobs: []pipeline.Job{
			{ID: "a", Prompt: "first", Worktree: "none", Status: pipeline.JobPending},
			{ID: "b", Prompt: "second", DependsOn: []string{"a"}, Worktree: "from:a", Status: pipeline.JobPending},
		}}
}

func TestReconcileSpawnsRootOnly(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	if err := e.Reconcile(context.Background(), "p"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobRunning || got.Job("a").SessionID != "p-a" {
		t.Fatalf("root a should be running: %+v", got.Job("a"))
	}
	if got.Job("b").Status != pipeline.JobPending {
		t.Fatalf("b should still be pending")
	}
	if got.Status != pipeline.StatusRunning {
		t.Fatalf("pipeline status %s", got.Status)
	}
	// the spawned job's session was inserted, with the back-ref.
	sess, err := ss.Get(context.Background(), "p-a")
	if err != nil || sess.PipelineID != "p" || sess.JobID != "a" {
		t.Fatalf("session not inserted with back-ref: %+v err=%v", sess, err)
	}
}

func TestReconcileUnblocksOnEmittedOutput(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // spawns a
	// simulate a's emit: mark done with output + branch.
	ps.Update("p", func(p *pipeline.Pipeline) {
		j := p.Job("a")
		j.Status = pipeline.JobDone
		j.Output = "done with a"
		j.Branch = "" // none-mode job has no branch
	})
	if err := e.Reconcile(context.Background(), "p"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("b").Status != pipeline.JobRunning || got.Job("b").SessionID != "p-b" {
		t.Fatalf("b should now be running: %+v", got.Job("b"))
	}
}

func TestOnTransitionFailsJobAndSkipsDescendants(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // a running, session p-a inserted
	// the running session errors out.
	sess, _ := ss.Get(context.Background(), "p-a")
	e.OnTransition(sess, store.StatusWorking, store.StatusErrored)
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobFailed {
		t.Fatalf("a should be failed, got %s", got.Job("a").Status)
	}
	if got.Job("b").Status != pipeline.JobSkipped {
		t.Fatalf("b (descendant) should be skipped, got %s", got.Job("b").Status)
	}
	if got.Status != pipeline.StatusStalled {
		t.Fatalf("pipeline should be stalled, got %s", got.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestReconcile|TestOnTransition'`
Expected: FAIL — `undefined: NewExecutor` / `Executor`.

- [ ] **Step 3: Write minimal implementation**

Create `internal/daemon/executor.go`:

```go
package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/store"
)

// Executor performs the side effects the pure pipeline.Plan decides: spawning
// ready jobs, skipping failed branches, and persisting status. It is driven by
// Reconcile (after start/emit) and OnTransition (a job session errored).
type Executor struct {
	pstore *pipeline.Store
	sstore store.Store
	life   Lifecycle
	cstore *ctxstore.Store
	notify func() // signals SSE subscribers that state changed (may be nil)
}

func NewExecutor(ps *pipeline.Store, ss store.Store, life Lifecycle, cs *ctxstore.Store, notify func()) *Executor {
	return &Executor{pstore: ps, sstore: ss, life: life, cstore: cs, notify: notify}
}

// Reconcile advances a pipeline: spawn newly-ready jobs, skip failed branches,
// and update the pipeline + job statuses.
func (e *Executor) Reconcile(ctx context.Context, pid string) error {
	p, err := e.pstore.Get(pid)
	if err != nil {
		return err
	}
	if p.Status == pipeline.StatusCanceled || p.Status == pipeline.StatusDone {
		return nil
	}
	d := pipeline.Plan(p)

	// Spawn ready jobs (outside the store lock; capture results to persist after).
	type spawned struct {
		jobID, sessionID string
		sess             *store.Session
	}
	var ok []spawned
	for _, jobID := range d.Spawn {
		job := p.Job(jobID)
		worktree, base := e.resolveWorktree(p, job)
		req := lifecycle.JobSpawnRequest{
			PipelineID: p.ID, JobID: job.ID, Repo: p.Repo,
			Prompt: pipeline.ComposePrompt(p, job), Worktree: worktree,
			BaseBranch: base, Type: store.NormalizeType(job.Type), Supervised: job.Supervised,
		}
		sess, serr := e.life.SpawnJob(ctx, req)
		if serr != nil {
			// Spawn failure fails the job; descendants get skipped on next Plan.
			e.markJob(pid, job.ID, func(j *pipeline.Job) { j.Status = pipeline.JobFailed })
			continue
		}
		if ierr := e.sstore.Insert(ctx, sess); ierr != nil {
			tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = e.life.Teardown(tctx, sess)
			cancel()
			e.markJob(pid, job.ID, func(j *pipeline.Job) { j.Status = pipeline.JobFailed })
			continue
		}
		ok = append(ok, spawned{job.ID, sess.ID, sess})
	}

	// Persist statuses: spawned→running, skipped→skipped, and pipeline status.
	if err := e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		for _, s := range ok {
			if j := p.Job(s.jobID); j != nil {
				j.Status = pipeline.JobRunning
				j.SessionID = s.sessionID
			}
		}
		for _, id := range d.Skip {
			if j := p.Job(id); j != nil && j.Status == pipeline.JobPending {
				j.Status = pipeline.JobSkipped
			}
		}
		p.Status = pipeline.Plan(p).Status // recompute from the just-applied statuses
	}); err != nil {
		return err
	}
	if e.notify != nil {
		e.notify()
	}
	return nil
}

// resolveWorktree maps a job's worktree spec to (createWorktree, baseBranch).
func (e *Executor) resolveWorktree(p *pipeline.Pipeline, job *pipeline.Job) (bool, string) {
	mode, from := pipeline.ParseWorktree(job.Worktree)
	switch mode {
	case "fresh":
		return true, ""
	case "from":
		if up := p.Job(from); up != nil {
			return true, up.Branch // "" if the upstream had no worktree
		}
		return true, ""
	default: // "none"
		return false, ""
	}
}

func (e *Executor) markJob(pid, jobID string, fn func(*pipeline.Job)) {
	_ = e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		if j := p.Job(jobID); j != nil {
			fn(j)
		}
	})
}

// OnTransition is the poller hook: when a job's session errors or is orphaned,
// the job is marked failed and the pipeline reconciled (skipping descendants).
// Job completion is NOT inferred here — that comes only via `emit`.
func (e *Executor) OnTransition(sess *store.Session, _ store.Status, to store.Status) {
	if sess.PipelineID == "" {
		return
	}
	if to != store.StatusErrored && to != store.StatusOrphaned {
		return
	}
	e.markJob(sess.PipelineID, sess.JobID, func(j *pipeline.Job) {
		if j.Status == pipeline.JobRunning {
			j.Status = pipeline.JobFailed
		}
	})
	_ = e.Reconcile(context.Background(), sess.PipelineID)
}

// Emit records a job's handoff: write to shared context, capture its branch from
// its session, mark it done, then reconcile to unblock dependents.
func (e *Executor) Emit(ctx context.Context, pid, jobID, text string) error {
	p, err := e.pstore.Get(pid)
	if err != nil {
		return err
	}
	job := p.Job(jobID)
	if job == nil {
		return fmt.Errorf("unknown job %q in pipeline %q", jobID, pid)
	}
	branch := ""
	if job.SessionID != "" {
		if sess, gerr := e.sstore.Get(ctx, job.SessionID); gerr == nil {
			branch = sess.Branch
		}
	}
	if e.cstore != nil {
		_, _ = e.cstore.Set("pipeline."+pid+"."+jobID+".output", text, "pipeline:"+pid)
	}
	if err := e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		if j := p.Job(jobID); j != nil {
			j.Output = text
			j.Branch = branch
			j.Status = pipeline.JobDone
		}
	}); err != nil {
		return err
	}
	return e.Reconcile(ctx, pid)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestReconcile|TestOnTransition'`
Expected: PASS.

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/executor.go internal/daemon/executor_test.go
git commit -m "feat(daemon): pipeline Executor (reconcile, spawn, emit, failure)"
```

---

## Task 10: daemon pipeline routes

**Files:**
- Create: `internal/daemon/pipeline_routes.go`
- Modify: `internal/daemon/api.go` (add `exec *Executor` field; register routes)
- Test: `internal/daemon/pipeline_routes_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/pipeline_routes_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/pipeline"
)

func newPipeServer(t *testing.T) (*httptest.Server, *pipeline.Store) {
	t.Helper()
	ps, _ := pipeline.NewStore(t.TempDir())
	cs, _ := ctxstore.New(t.TempDir())
	ss := newFakeStore()
	exec := NewExecutor(ps, ss, &fakeLife{}, cs, func() {})
	srv := &Server{store: ss, life: &fakeLife{}, exec: exec, hub: newHub(), done: make(chan struct{})}
	return httptest.NewServer(srv.router()), ps
}

const yamlBody = `{"spec":"name: demo\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    worktree: none\n"}`

func TestPipelineCreateThenList(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status %d", resp.StatusCode)
	}
	var p pipeline.Pipeline
	json.NewDecoder(resp.Body).Decode(&p)
	if p.ID != "demo" || len(p.Jobs) != 1 {
		t.Fatalf("created pipeline wrong: %+v", p)
	}

	resp2, _ := http.Get(ts.URL + "/pipelines")
	defer resp2.Body.Close()
	var lr struct {
		Pipelines []pipeline.Pipeline `json:"pipelines"`
	}
	json.NewDecoder(resp2.Body).Decode(&lr)
	if len(lr.Pipelines) != 1 {
		t.Fatalf("list want 1, got %d", len(lr.Pipelines))
	}
}

func TestPipelineCreateInvalidYAML400(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	bad := `{"spec":"name: demo\nrepo: /r\njobs:\n  - id: a\n    prompt: go\n    depends_on: [ghost]\n"}`
	resp, _ := http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(bad))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestPipelineStartSpawnsRoot(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))

	resp, _ := http.Post(ts.URL+"/pipelines/demo/start", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("root not spawned on start: %+v", got.Job("a"))
	}
}

func TestPipelineEmitMarksDone(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	http.Post(ts.URL+"/pipelines/demo/start", "application/json", nil)

	resp, _ := http.Post(ts.URL+"/pipelines/demo/jobs/a/emit", "application/json", strings.NewReader(`{"text":"all done"}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("emit status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobDone || got.Job("a").Output != "all done" {
		t.Fatalf("emit did not complete job: %+v", got.Job("a"))
	}
	if got.Status != pipeline.StatusDone {
		t.Fatalf("single-job pipeline should be done, got %s", got.Status)
	}
}

func TestPipelineShow404(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	resp, _ := http.Get(ts.URL + "/pipelines/ghost")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	_ = context.Background()
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestPipeline`
Expected: FAIL — `unknown field 'exec' in struct literal of type Server`.

- [ ] **Step 3a: Add the field + route registration**

In `internal/daemon/api.go`, add to the `Server` struct (after `mbox`):

```go
	mbox *mailbox.Store
	// exec drives pipeline execution (nil if pipelines are unused).
	exec *Executor
}
```

In `router()`, register after `s.registerMessageRoutes(r)` and before `s.registerStatic(r)`:

```go
	s.registerMessageRoutes(r)
	s.registerPipelineRoutes(r)
	s.registerStatic(r) // catch-all; must be last
```

- [ ] **Step 3b: Write the handlers**

Create `internal/daemon/pipeline_routes.go`:

```go
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/pipeline"
)

type createPipelineRequest struct {
	Spec string `json:"spec"` // raw YAML
}
type pipelinesResponse struct {
	Pipelines []*pipeline.Pipeline `json:"pipelines"`
}
type emitRequest struct {
	Text string `json:"text"`
}

func (s *Server) registerPipelineRoutes(r chi.Router) {
	r.Post("/pipelines", s.handleCreatePipeline)
	r.Get("/pipelines", s.handleListPipelines)
	r.Get("/pipelines/{pid}", s.handleShowPipeline)
	r.Post("/pipelines/{pid}/start", s.handleStartPipeline)
	r.Post("/pipelines/{pid}/cancel", s.handleCancelPipeline)
	r.Post("/pipelines/{pid}/jobs/{job}/emit", s.handleEmit)
}

func (s *Server) handleCreatePipeline(w http.ResponseWriter, r *http.Request) {
	var req createPipelineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	p, err := pipeline.ParseSpec([]byte(req.Spec))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.exec.pstore.Create(p); errors.Is(err, pipeline.ErrExists) {
		writeErr(w, http.StatusConflict, "pipeline "+p.ID+" already exists")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleListPipelines(w http.ResponseWriter, r *http.Request) {
	ps, err := s.exec.pstore.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pipelinesResponse{Pipelines: ps})
}

func (s *Server) handleShowPipeline(w http.ResponseWriter, r *http.Request) {
	p, err := s.exec.pstore.Get(chi.URLParam(r, "pid"))
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleStartPipeline(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "pid")
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p.Status != pipeline.StatusPending {
		writeErr(w, http.StatusConflict, "pipeline already started (status "+string(p.Status)+")")
		return
	}
	if err := s.exec.pstore.Update(pid, func(p *pipeline.Pipeline) { p.Status = pipeline.StatusRunning }); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.exec.Reconcile(r.Context(), pid); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *Server) handleCancelPipeline(w http.ResponseWriter, r *http.Request) {
	pid := chi.URLParam(r, "pid")
	p, err := s.exec.pstore.Get(pid)
	if errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Terminate any running job sessions (best-effort), then mark canceled.
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if j.Status == pipeline.JobRunning && j.SessionID != "" {
			_ = s.life.Terminate(r.Context(), j.SessionID)
		}
	}
	if err := s.exec.pstore.Update(pid, func(p *pipeline.Pipeline) {
		for i := range p.Jobs {
			if p.Jobs[i].Status == pipeline.JobPending || p.Jobs[i].Status == pipeline.JobRunning {
				p.Jobs[i].Status = pipeline.JobSkipped
			}
		}
		p.Status = pipeline.StatusCanceled
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (s *Server) handleEmit(w http.ResponseWriter, r *http.Request) {
	pid, job := chi.URLParam(r, "pid"), chi.URLParam(r, "job")
	var req emitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Text == "" {
		writeErr(w, http.StatusBadRequest, "empty emit text")
		return
	}
	if err := s.exec.Emit(r.Context(), pid, job, req.Text); errors.Is(err, pipeline.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "emitted"})
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run TestPipeline`
Expected: PASS (all 5).

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/pipeline_routes.go internal/daemon/pipeline_routes_test.go internal/daemon/api.go
git commit -m "feat(daemon): pipeline routes (create/list/show/start/cancel/emit)"
```

---

## Task 11: Wire the Executor into NewServer + daemon command

**Files:**
- Modify: `internal/daemon/server.go`
- Modify: `internal/cli/daemon.go`
- Modify: `internal/daemon/server_test.go`

- [ ] **Step 1: Add the `*Executor` param to NewServer**

In `internal/daemon/server.go`, add `exec *Executor` as the LAST param and set `exec: exec`:

```go
func NewServer(st store.Store, life Lifecycle, p *poller.Poller, interval time.Duration, approvals bool, cstore *ctxstore.Store, mbox *mailbox.Store, exec *Executor) *Server {
	h := newHub()
	if p != nil {
		p.OnChange = h.publish
	}
	return &Server{
		store: st, life: life, poller: p, pollInterval: interval,
		hub: h, done: make(chan struct{}), approvals: approvals, cstore: cstore, mbox: mbox, exec: exec,
	}
}
```

- [ ] **Step 2: Run build to verify it fails at the call sites**

Run: `go build ./...`
Expected: FAIL — `not enough arguments in call to daemon.NewServer` at `internal/cli/daemon.go` and `internal/daemon/server_test.go`.

- [ ] **Step 3: Update daemon.go + server_test.go**

In `internal/cli/daemon.go`, add the import `"github.com/srajanpathak/agentctl/internal/pipeline"`, then after the `mbox` construction build the pipeline store + executor and compose the OnTransition hook. Replace the existing `pl.OnTransition = ...` line and the `NewServer(...)` line:

```go
			mbox, err := mailbox.New(filepath.Join(cfg.DataDir, "inbox"))
			if err != nil {
				return err
			}

			pstore, err := pipeline.NewStore(filepath.Join(cfg.DataDir, "pipelines"))
			if err != nil {
				return err
			}
			srv := daemon.NewServer(st, life, pl, 10*time.Second, cfg.ApprovalsEnabled, cstore, mbox, nil)
			exec := daemon.NewExecutor(pstore, st, life, cstore, srv.Notify)
			srv.SetExecutor(exec)

			notifyHook := daemon.NotifyOnTransition(notify.New(cfg.NotifyEnabled))
			pl.OnTransition = func(sess *store.Session, from, to store.Status) {
				notifyHook(sess, from, to)
				exec.OnTransition(sess, from, to)
			}
```

(Remove the old standalone `pl.OnTransition = daemon.NotifyOnTransition(...)` line so it isn't set twice.)

This needs two tiny helpers on `Server` (the executor and server reference each other, so wire after construction). In `internal/daemon/api.go` add:

```go
// Notify is the exported SSE-notify hook the Executor calls after it changes state.
func (s *Server) Notify() { s.notify() }

// SetExecutor wires the executor after construction (executor needs Server.Notify).
func (s *Server) SetExecutor(e *Executor) { s.exec = e }
```

And change the `NewServer` last arg usage: since we now wire the executor via `SetExecutor`, pass `nil` for `exec` in `NewServer` (as shown above) — keep the param for symmetry/tests.

In `internal/daemon/server_test.go`, update the `NewServer(...)` call to pass `nil` for the new arg:

```go
	srv := NewServer(newFakeStore(), &fakeLife{}, nil, time.Second, false, nil, nil, nil)
```

- [ ] **Step 4: Run build + tests**

Run: `go build ./... && go test ./internal/daemon/... ./internal/cli/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/server.go internal/daemon/api.go internal/cli/daemon.go internal/daemon/server_test.go
git commit -m "feat(daemon): wire pipeline Executor into NewServer + OnTransition"
```

---

## Task 12: Client methods + CLI `pipeline` command

**Files:**
- Modify: `internal/client/client.go`
- Create: `internal/cli/pipeline.go`
- Modify: `internal/cli/root.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/client/client_test.go`:

```go
func TestPipelineCreateAndEmit(t *testing.T) {
	var createBody, emitPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/pipelines" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			createBody = string(b)
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"id":"demo","name":"demo","repo":"/r","status":"pending","jobs":[]}`))
		case strings.HasSuffix(r.URL.Path, "/emit"):
			emitPath = r.URL.Path
			w.Write([]byte(`{"status":"emitted"}`))
		}
	}))
	defer ts.Close()
	c := New(ts.URL)

	p, err := c.PipelineCreate(context.Background(), "name: demo\nrepo: /r\njobs: []\n")
	if err != nil {
		t.Fatalf("PipelineCreate: %v", err)
	}
	if p.ID != "demo" || !strings.Contains(createBody, `"spec"`) {
		t.Fatalf("create wrong: p=%+v body=%s", p, createBody)
	}
	if err := c.PipelineEmit(context.Background(), "demo", "a", "done"); err != nil {
		t.Fatalf("PipelineEmit: %v", err)
	}
	if emitPath != "/pipelines/demo/jobs/a/emit" {
		t.Fatalf("emit path %s", emitPath)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestPipeline`
Expected: FAIL — `c.PipelineCreate undefined`.

- [ ] **Step 3: Add client methods**

Append to `internal/client/client.go` (add the import `"github.com/srajanpathak/agentctl/internal/pipeline"` to the import block):

```go
// PipelineCreate sends a YAML spec to the daemon, which parses, validates, and
// stores it.
func (c *Client) PipelineCreate(ctx context.Context, specYAML string) (*pipeline.Pipeline, error) {
	var p pipeline.Pipeline
	if err := c.do(ctx, http.MethodPost, "/pipelines", map[string]string{"spec": specYAML}, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error) {
	var resp struct {
		Pipelines []*pipeline.Pipeline `json:"pipelines"`
	}
	if err := c.do(ctx, http.MethodGet, "/pipelines", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Pipelines, nil
}

func (c *Client) PipelineGet(ctx context.Context, id string) (*pipeline.Pipeline, error) {
	var p pipeline.Pipeline
	if err := c.do(ctx, http.MethodGet, "/pipelines/"+url.PathEscape(id), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) PipelineStart(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/pipelines/"+url.PathEscape(id)+"/start", nil, nil)
}

func (c *Client) PipelineCancel(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/pipelines/"+url.PathEscape(id)+"/cancel", nil, nil)
}

func (c *Client) PipelineEmit(ctx context.Context, pid, job, text string) error {
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/emit"
	return c.do(ctx, http.MethodPost, path, map[string]string{"text": text}, nil)
}
```

- [ ] **Step 4: Run client test to verify it passes**

Run: `go test ./internal/client/ -run TestPipeline`
Expected: PASS.

- [ ] **Step 5: Add the CLI command**

Create `internal/cli/pipeline.go`:

```go
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newPipelineCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pipeline",
		Short: "Define and run DAG pipelines of agent jobs",
	}
	cmd.AddCommand(newPipelineCreateCmd(), newPipelineListCmd(), newPipelineShowCmd(),
		newPipelineStartCmd(), newPipelineCancelCmd(), newPipelineEmitCmd())
	return cmd
}

func newPipelineCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create -f <spec.yaml>",
		Short: "Create a pipeline from a YAML spec",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			file, _ := cmd.Flags().GetString("file")
			if file == "" {
				return fmt.Errorf("provide a spec with -f <spec.yaml>")
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			p, err := clientFor(cmd).PipelineCreate(cmd.Context(), string(data))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created pipeline %s (%d jobs) — start it with `agentctl pipeline start %s`\n", p.ID, len(p.Jobs), p.ID)
			return nil
		},
	}
	cmd.Flags().StringP("file", "f", "", "path to the pipeline YAML spec")
	return cmd
}

func newPipelineListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ps, err := clientFor(cmd).PipelineList(cmd.Context())
			if err != nil {
				return err
			}
			for _, p := range ps {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%d jobs\n", p.ID, p.Status, len(p.Jobs))
			}
			return nil
		},
	}
}

func newPipelineShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <pipeline>",
		Short: "Show a pipeline's jobs and their status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := clientFor(cmd).PipelineGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s [%s] repo=%s\n", p.ID, p.Status, p.Repo)
			for _, j := range p.Jobs {
				deps := ""
				if len(j.DependsOn) > 0 {
					deps = fmt.Sprintf(" (depends: %v)", j.DependsOn)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %-9s%s\n", j.ID, j.Status, deps)
			}
			return nil
		},
	}
}

func newPipelineStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start <pipeline>",
		Short: "Start a pipeline (spawns jobs with no dependencies)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineStart(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "started %s\n", args[0])
			return nil
		},
	}
}

func newPipelineCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <pipeline>",
		Short: "Cancel a pipeline (terminates running jobs)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineCancel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "canceled %s\n", args[0])
			return nil
		},
	}
}

func newPipelineEmitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "emit <text>",
		Short: "Publish this job's handoff (run from inside a pipeline job)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pid, _ := cmd.Flags().GetString("pipeline")
			job, _ := cmd.Flags().GetString("job")
			if pid == "" {
				pid = os.Getenv("AGENTCTL_PIPELINE_ID")
			}
			if job == "" {
				job = os.Getenv("AGENTCTL_JOB_ID")
			}
			if pid == "" || job == "" {
				return fmt.Errorf("no pipeline/job: run inside a pipeline job, or pass --pipeline and --job")
			}
			text := strings.Join(args, " ")
			if err := clientFor(cmd).PipelineEmit(cmd.Context(), pid, job, text); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "emitted handoff for %s/%s\n", pid, job)
			return nil
		},
	}
	cmd.Flags().String("pipeline", "", "pipeline id (defaults to $AGENTCTL_PIPELINE_ID)")
	cmd.Flags().String("job", "", "job id (defaults to $AGENTCTL_JOB_ID)")
	return cmd
}
```

Add `"strings"` to the import block of `internal/cli/pipeline.go` (used by `emit`).

In `internal/cli/root.go`, register after `newMsgCmd()`:

```go
	root.AddCommand(newMsgCmd())
	root.AddCommand(newPipelineCmd())
```

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/client/... ./internal/cli/... && go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go internal/cli/pipeline.go internal/cli/root.go
git commit -m "feat(client,cli): pipeline create/list/show/start/cancel/emit"
```

---

## Task 13: Docs + full verification

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Add a pipelines section**

Read `docs/USAGE.md` for the heading style, then append a "Pipelines" section (numbered `##`, fenced ```` ```sh ```` blocks, `---` rule) conveying:

```markdown
## Pipelines

Run a DAG of agent jobs. Each job spawns as a normal agent when its dependencies
finish; outputs (and branch names) flow downstream automatically.

Author a spec (`refactor.yaml`):

    name: refactor-auth
    repo: /Users/me/workspace/app
    jobs:
      - id: analyze
        prompt: "Analyze the auth module; no code yet."
        worktree: none
      - id: implement
        prompt: "Implement the refactor described upstream."
        depends_on: [analyze]
        worktree: fresh
        handoff: "the branch name and a 2-line summary"
      - id: review
        prompt: "Merge the implement branch, review, run the suite."
        depends_on: [implement]
        worktree: from:implement

Then:

    agentctl pipeline create -f refactor.yaml   # validates the DAG (cycles, refs)
    agentctl pipeline start refactor-auth        # spawns jobs with no deps
    agentctl pipeline show refactor-auth         # DAG + per-job status
    agentctl pipeline cancel refactor-auth       # terminate running jobs

Each job's agent finishes by running `agentctl pipeline emit "<handoff>"` (the
pipeline/job are auto-set in its environment). Emitting publishes the handoff to
shared context, marks the job done, and unblocks its dependents. `worktree:
from:<job>` bases a job's git worktree on an upstream job's branch; a fan-in job
runs `git merge` itself. A job whose session errors marks the job failed and
skips its descendants (pipeline → `stalled`).
```

- [ ] **Step 2: Run the full suite (with -race on new packages)**

Run: `go build ./... && go test ./... && go test -race ./internal/pipeline/... ./internal/daemon/... && make lint`
Expected: PASS across all packages; lint (go vet) clean. If anything fails, do NOT commit — report it.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document agentctl pipeline DAG executor"
```

---

## Verification checklist (after all tasks)

- [ ] `go build ./...` clean; `go test ./...` green; `go test -race ./internal/pipeline/... ./internal/daemon/...` green; `make lint` clean.
- [ ] Manual smoke (rebuild + restart daemon: `./scripts/reinstall.sh`), in a throwaway git repo:
  - Write a 2-job linear spec (`analyze` → `implement`, both `worktree: none` against a real repo path).
  - `agentctl pipeline create -f spec.yaml` → "created pipeline …".
  - `agentctl pipeline start <name>` → `agentctl pipeline show <name>` shows `analyze` running, `implement` pending; `agentctl ls` shows the `<name>-analyze` agent.
  - In the `analyze` agent (attach, or it does so itself), run `agentctl pipeline emit "done analyzing"` → `pipeline show` now shows `analyze` done and `implement` running, with the analyze output injected into implement's prompt.
  - `agentctl pipeline cancel <name>` terminates any running job and marks the pipeline canceled.

## Deferred to Phase 4b (not in this plan)
- `agentctl pipeline edit-job <pid> <job> --prompt/--handoff` (edit a pending job).
- `agentctl pipeline retry <pid> <job>` (re-spawn a failed job).
- "Done-but-no-emit" grace window → `needs-attention` (here, a job completes only via `emit`; a session that errors/orphans marks the job failed).
```
