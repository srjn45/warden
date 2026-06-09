# Pipeline Reap-On-Done (+ Digest Snapshot) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a pipeline job completes (`emit`), terminate its agent process+tmux (freeing the slot + RAM), snapshot a completion digest into the job record, and render terminal-status jobs as inline details in the TUI/web instead of attaching to a dead session.

**Architecture:** Extend the executor's `Emit` to `Terminate` (never `Teardown`) the job session and async-snapshot a digest into a new `pipeline.Job.Digest` field. Extract the digest-building logic into a reusable `Server.buildDigest`, inject it into the executor, and serve the stored snapshot from the session digest endpoint for reaped jobs. TUI/web branch on job status to render details vs. attach. Default-on with an `AGENTCTL_PIPELINE_KEEP_DONE=1` escape hatch.

**Tech Stack:** Go (chi, testify), Bubble Tea TUI, React/TypeScript (vitest) web.

**Spec:** `docs/superpowers/specs/2026-06-05-agentctl-pipeline-reap-on-done-design.md`

**Worktree:** `.claude/worktrees/pipeline-reap-on-done` (baseRef=head; no origin). NOTE: a concurrent agent is adding a TUI/web `delete` verb touching `internal/tui/{pipeline_view,keys,model}.go` and `web/src/components/PipelinesTab.tsx` — the same files Tasks 7–8 touch. Build the Go core (Tasks 1–6) first; do TUI/web last; **rebase onto master and re-run the full suite before ff-merging.**

---

### Task 1: Add `Digest` field to `pipeline.Job`

**Files:**
- Modify: `internal/pipeline/pipeline.go` (imports + `Job` struct, runtime block ~line 46-50)
- Test: `internal/pipeline/pipeline_test.go`

- [ ] **Step 1: Write the failing test** — append to `pipeline_test.go`:

```go
func TestJobDigestRoundTrips(t *testing.T) {
	j := Job{ID: "impl", Prompt: "do it", Digest: &digest.Digest{Summary: "did it", Turns: 3}}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Job
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Digest == nil || got.Digest.Summary != "did it" || got.Digest.Turns != 3 {
		t.Fatalf("digest did not round-trip: %+v", got.Digest)
	}
}
```

Add imports `encoding/json` and `"github.com/srajanpathak/agentctl/internal/digest"` to the test file if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestJobDigestRoundTrips`
Expected: FAIL — `Job` has no field `Digest`.

- [ ] **Step 3: Add the field + import**

In `internal/pipeline/pipeline.go`, add to the import block:

```go
"github.com/srajanpathak/agentctl/internal/digest"
```

In the `Job` struct's runtime block (after `Branch string ...`):

```go
	Digest *digest.Digest `json:"digest,omitempty" yaml:"-"` // completion snapshot (nil until reaped)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pipeline/`
Expected: PASS (whole package; confirms no import cycle).

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/pipeline.go internal/pipeline/pipeline_test.go
git commit -m "feat(pipeline): add Job.Digest completion-snapshot field"
```

---

### Task 2: Extract `Server.buildDigest` from `handleDigest`

**Files:**
- Modify: `internal/daemon/digest_routes.go`
- Test: `internal/daemon/digest_routes_test.go` (existing tests must still pass — pure refactor)

- [ ] **Step 1: Run the existing digest tests to capture the green baseline**

Run: `go test ./internal/daemon/ -run Digest`
Expected: PASS (record which tests exist; they guard the refactor).

- [ ] **Step 2: Extract the builder** — replace the body of `handleDigest` (`digest_routes.go:17-61`) with:

```go
func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	d := s.buildDigest(r.Context(), sess)
	writeJSON(w, http.StatusOK, d)
}

// buildDigest assembles a completion digest from a session's on-disk transcript
// and git stats, enriched by a best-effort narrator that degrades to the last
// assistant message. Side-effect-free; reused by the pipeline executor to snapshot
// a job's digest at completion.
func (s *Server) buildDigest(ctx context.Context, sess *store.Session) digest.Digest {
	d := digest.Digest{Status: string(sess.Status)}
	path := s.life.TranscriptPath(sess)
	if path == "" {
		d.Summary = "no transcript available"
		return d
	}
	f, err := os.Open(path)
	if err != nil {
		d.Summary = "no transcript available"
		return d
	}
	defer f.Close()
	facts, _ := digest.ParseTranscript(f)
	stats := digest.ParseNumstat(s.life.GitNumstat(ctx, sess.Workdir))
	d.Files = digest.MergeFiles(facts.EditedFiles, stats)
	d.Branch = s.life.GitBranch(ctx, sess.Workdir)
	d.Turns = facts.Turns
	d.Task = facts.Task
	d.Summary = facts.LastMessage
	if s.narrator != nil {
		if out, err := s.narrator.Summarize(ctx, facts); err == nil && strings.TrimSpace(out) != "" {
			d.Summary = out
		}
	}
	return d
}
```

Add `"context"` to the import block if not already present.

- [ ] **Step 3: Run the digest tests again**

Run: `go test ./internal/daemon/ -run Digest`
Expected: PASS — identical behavior, now via `buildDigest`.

- [ ] **Step 4: Commit**

```bash
git add internal/daemon/digest_routes.go
git commit -m "refactor(daemon): extract Server.buildDigest from handleDigest"
```

---

### Task 3: Executor — digest-fn injection, keep-done flag, JobDigest accessor

**Files:**
- Modify: `internal/daemon/executor.go` (struct + imports + setters + accessor)
- Test: `internal/daemon/executor_test.go`

- [ ] **Step 1: Write the failing test** — append to `executor_test.go`:

```go
func TestExecutorJobDigestAccessor(t *testing.T) {
	ps := newTestPipelineStore(t) // use the existing helper the other executor tests use
	_ = ps.Create(&pipeline.Pipeline{
		ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Prompt: "x", Status: pipeline.JobDone,
			Digest: &digest.Digest{Summary: "snap"}}},
	})
	e := NewExecutor(ps, newFakeStore(), &fakeLife{}, nil, func() {})
	if got := e.JobDigest("p", "a"); got == nil || got.Summary != "snap" {
		t.Fatalf("want snap, got %+v", got)
	}
	if got := e.JobDigest("p", "missing"); got != nil {
		t.Fatalf("want nil for unknown job, got %+v", got)
	}
}
```

> If the executor tests construct the pipeline store differently (read the top of `executor_test.go` for the existing setup helper, e.g. a `t.TempDir()` + `pipeline.NewStore`), mirror that exact pattern instead of `newTestPipelineStore`. Add the `digest` import to the test file.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/daemon/ -run TestExecutorJobDigestAccessor`
Expected: FAIL — `e.JobDigest` undefined.

- [ ] **Step 3: Add fields, setters, accessor** — in `internal/daemon/executor.go`:

Add to imports: `"github.com/srajanpathak/agentctl/internal/digest"`.

Add to the `Executor` struct (after `notify func()`):

```go
	digestFn func(context.Context, *store.Session) digest.Digest // nil ⇒ skip snapshot
	keepDone bool                                                // AGENTCTL_PIPELINE_KEEP_DONE — keep done agents alive
	snapWG   sync.WaitGroup                                      // tracks in-flight digest snapshots (test sync)
```

Add the setters + accessor (anywhere in the file, e.g. after `NewExecutor`):

```go
// SetDigestFn wires the digest builder used to snapshot a job's completion digest
// (bound to Server.buildDigest in production). nil ⇒ no snapshot.
func (e *Executor) SetDigestFn(fn func(context.Context, *store.Session) digest.Digest) { e.digestFn = fn }

// SetKeepDoneAgents, when true, leaves a completed job's agent alive (skips the
// reap) so its tmux pane stays attachable for debugging.
func (e *Executor) SetKeepDoneAgents(v bool) { e.keepDone = v }

// JobDigest returns a job's stored completion-digest snapshot, or nil.
func (e *Executor) JobDigest(pid, jobID string) *digest.Digest {
	p, err := e.pstore.Get(pid)
	if err != nil {
		return nil
	}
	if j := p.Job(jobID); j != nil {
		return j.Digest
	}
	return nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/daemon/ -run TestExecutorJobDigestAccessor`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/executor.go internal/daemon/executor_test.go
git commit -m "feat(executor): digest-fn injection, keep-done flag, JobDigest accessor"
```

---

### Task 4: Executor `Emit` — reap the agent + async digest snapshot

**Files:**
- Modify: `internal/daemon/executor.go` (`Emit`, ~line 267-303)
- Test: `internal/daemon/executor_test.go`

- [ ] **Step 1: Write the failing tests** — append to `executor_test.go`:

```go
// On emit→done, the job's agent is reaped via Terminate (never Teardown), the
// digest snapshot is persisted, and downstream still spawns off the branch.
func TestEmitReapsAgentAndSnapshotsDigest(t *testing.T) {
	ps := newTestPipelineStore(t)
	_ = ps.Create(&pipeline.Pipeline{
		ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{
			{ID: "a", Prompt: "analyze", Worktree: "fresh"},
			{ID: "b", Prompt: "impl", DependsOn: []string{"a"}, Worktree: "from:a"},
		},
	})
	fl := &fakeLife{}
	ss := newFakeStore()
	e := NewExecutor(ps, ss, fl, nil, func() {})
	e.digestFn = func(_ context.Context, s *store.Session) digest.Digest {
		return digest.Digest{Summary: "snap for " + s.ID}
	}
	// start spawns job a
	if err := e.Reconcile(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	// emit a's handoff
	if err := e.Emit(context.Background(), "p", "a", "done analyzing"); err != nil {
		t.Fatal(err)
	}
	e.snapWG.Wait() // wait for the async snapshot

	p, _ := ps.Get("p")
	ja := p.Job("a")
	if ja.Status != pipeline.JobDone {
		t.Fatalf("a not done: %s", ja.Status)
	}
	if fl.terminated != "p-a" { // fake SpawnJob sets TmuxSession == "<pid>-<job>"
		t.Fatalf("expected Terminate(p-a), got %q", fl.terminated)
	}
	if fl.tornDown != "" {
		t.Fatalf("must NOT Teardown a done job, got %q", fl.tornDown)
	}
	if ja.Digest == nil || ja.Digest.Summary != "snap for p-a" {
		t.Fatalf("digest snapshot not persisted: %+v", ja.Digest)
	}
	if p.Job("b").Status != pipeline.JobRunning {
		t.Fatalf("dependent b should have spawned, got %s", p.Job("b").Status)
	}
}

// With keep-done enabled, the agent is left alive (no Terminate, no snapshot).
func TestEmitKeepDoneSkipsReap(t *testing.T) {
	ps := newTestPipelineStore(t)
	_ = ps.Create(&pipeline.Pipeline{
		ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusRunning,
		Jobs: []pipeline.Job{{ID: "a", Prompt: "analyze", Worktree: "fresh"}},
	})
	fl := &fakeLife{}
	e := NewExecutor(ps, newFakeStore(), fl, nil, func() {})
	e.keepDone = true
	e.digestFn = func(_ context.Context, s *store.Session) digest.Digest { return digest.Digest{Summary: "x"} }
	_ = e.Reconcile(context.Background(), "p")
	if err := e.Emit(context.Background(), "p", "a", "done"); err != nil {
		t.Fatal(err)
	}
	e.snapWG.Wait()
	if fl.terminated != "" {
		t.Fatalf("keep-done must not Terminate, got %q", fl.terminated)
	}
	if p, _ := ps.Get("p"); p.Job("a").Digest != nil {
		t.Fatalf("keep-done must not snapshot a digest")
	}
}
```

> Match the pipeline-store + `newFakeStore` setup the existing executor tests use (read the file top). If `fakeStore.Insert`/`Get` need the spawned session present for `Emit`'s `sstore.Get(job.SessionID)`, the existing `Reconcile` path already inserts it via `e.sstore.Insert` — confirm `newFakeStore` persists it so `Emit` can read `TmuxSession`/`Branch`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/daemon/ -run 'TestEmitReapsAgent|TestEmitKeepDone'`
Expected: FAIL — agent not terminated / digest not snapshotted.

- [ ] **Step 3: Implement the reap + snapshot in `Emit`** — replace the branch-capture block in `Emit` (currently `branch := ""` … through the `e.pstore.Update(... j.Status = JobDone)`), keeping the ctxstore write, with:

```go
	var sess *store.Session
	if job.SessionID != "" {
		sess, _ = e.sstore.Get(ctx, job.SessionID)
	}
	branch := ""
	if sess != nil {
		branch = sess.Branch
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
	// Reap the completed agent (free the slot + RAM) and snapshot its digest.
	// Terminate ONLY — never Teardown — so the worktree + branch survive for
	// downstream `from:<job>` jobs (same invariant as `rotate`).
	if sess != nil && !e.keepDone {
		_ = e.life.Terminate(ctx, sess.TmuxSession)
		if e.digestFn != nil {
			e.snapWG.Add(1)
			go func(s *store.Session) {
				defer e.snapWG.Done()
				d := e.digestFn(context.Background(), s)
				d.Status = string(store.StatusDone)
				_ = e.pstore.Update(pid, func(p *pipeline.Pipeline) {
					if j := p.Job(jobID); j != nil {
						j.Digest = &d
					}
				})
				if e.notify != nil {
					e.notify()
				}
			}(sess)
		}
	}
	return e.Reconcile(ctx, pid)
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestEmitReapsAgent|TestEmitKeepDone'`
Expected: PASS.

- [ ] **Step 5: Run the whole daemon package (guards OnTransition no-op on a done job)**

Run: `go test ./internal/daemon/`
Expected: PASS — the existing `OnTransition` guards already no-op for a `JobDone` job (no spurious failed/needs_attention).

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/executor.go internal/daemon/executor_test.go
git commit -m "feat(executor): reap done-job agent + async digest snapshot on emit"
```

---

### Task 5: Serve the stored snapshot from the digest endpoint

**Files:**
- Modify: `internal/daemon/digest_routes.go` (`handleDigest`)
- Test: `internal/daemon/digest_routes_test.go`

- [ ] **Step 1: Write the failing test** — append to `digest_routes_test.go`:

```go
// A reaped pipeline job's session returns the stored snapshot, not a rebuild.
func TestHandleDigestServesPipelineSnapshot(t *testing.T) {
	ps := newTestPipelineStore(t)
	_ = ps.Create(&pipeline.Pipeline{
		ID: "p", Name: "p", Repo: "/r", Status: pipeline.StatusDone,
		Jobs: []pipeline.Job{{ID: "a", Status: pipeline.JobDone,
			Digest: &digest.Digest{Summary: "frozen snapshot", Turns: 7}}},
	})
	fs := newFakeStore()
	_ = fs.Insert(context.Background(), &store.Session{
		ID: "p-a", TmuxSession: "p-a", PipelineID: "p", JobID: "a", Status: store.StatusDone,
	})
	fl := &fakeLife{}
	srv := &Server{store: fs, life: fl, exec: NewExecutor(ps, fs, fl, nil, func() {})}
	ts := httptest.NewServer(srv.router())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/sessions/p-a/digest")
	require.NoError(t, err)
	defer resp.Body.Close()
	var got digest.Digest
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Equal(t, "frozen snapshot", got.Summary)
	require.Equal(t, 7, got.Turns)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/daemon/ -run TestHandleDigestServesPipelineSnapshot`
Expected: FAIL — handler rebuilds (empty/"no transcript") instead of serving the snapshot.

- [ ] **Step 3: Serve the snapshot** — in `handleDigest`, after the `sess` is fetched and before `d := s.buildDigest(...)`:

```go
	if s.exec != nil && sess.PipelineID != "" && sess.JobID != "" {
		if snap := s.exec.JobDigest(sess.PipelineID, sess.JobID); snap != nil {
			writeJSON(w, http.StatusOK, *snap)
			return
		}
	}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/daemon/ -run TestHandleDigest`
Expected: PASS (snapshot path + existing on-demand path both green).

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/digest_routes.go internal/daemon/digest_routes_test.go
git commit -m "feat(daemon): serve stored job-digest snapshot for reaped pipeline jobs"
```

---

### Task 6: Wire injection in the daemon assembly

**Files:**
- Modify: `internal/cli/daemon.go` (~line 67-72, after `srv.SetExecutor(exec)` / `srv.SetNarrator(...)`)

- [ ] **Step 1: Add the wiring** — after `srv.SetNarrator(digest.ClaudeNarrator{Run: lc.RunClaudeP})`:

```go
			exec.SetDigestFn(srv.BuildDigest)
			exec.SetKeepDoneAgents(os.Getenv("AGENTCTL_PIPELINE_KEEP_DONE") != "")
```

> `buildDigest` is unexported and `cli` is a different package, so export a thin wrapper in `internal/daemon/digest_routes.go`:
> ```go
> // BuildDigest is the exported entry point for wiring buildDigest into the executor.
> func (s *Server) BuildDigest(ctx context.Context, sess *store.Session) digest.Digest { return s.buildDigest(ctx, sess) }
> ```
> Ensure `os` is imported in `internal/cli/daemon.go` (it likely already is).

- [ ] **Step 2: Build the daemon**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Run the full Go suite**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/cli/daemon.go internal/daemon/digest_routes.go
git commit -m "feat(daemon): wire digest snapshot + keep-done env into the executor"
```

---

### Task 7: TUI — render terminal-job details instead of attaching

**Files:**
- Modify: `internal/tui/pipeline_view.go` (add `renderPipelineJob` + `jobIsTerminal`)
- Modify: `internal/tui/view.go` (detail dispatch, ~line 94-112)
- Modify: `internal/tui/keys.go` (`a` attach guard, ~line 72-80)
- Test: `internal/tui/pipeline_view_test.go`

- [ ] **Step 1: Write the failing test** — append to `pipeline_view_test.go`:

```go
func TestRenderPipelineJobShowsDetails(t *testing.T) {
	j := &pipeline.Job{
		ID: "impl", Status: pipeline.JobDone, Prompt: "implement the thing",
		Handoff: "branch + summary", Output: "did the thing", Branch: "p-impl",
		Digest: &digest.Digest{Summary: "narrated summary"},
	}
	out := renderPipelineJob(j, 80, 40)
	for _, want := range []string{"impl", "implement the thing", "branch + summary", "did the thing", "p-impl", "narrated summary"} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered job missing %q:\n%s", want, out)
		}
	}
}

func TestJobIsTerminal(t *testing.T) {
	for _, s := range []pipeline.JobStatus{pipeline.JobDone, pipeline.JobSkipped, pipeline.JobFailed} {
		if !jobIsTerminal(s) {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range []pipeline.JobStatus{pipeline.JobPending, pipeline.JobRunning, pipeline.JobNeedsAttention} {
		if jobIsTerminal(s) {
			t.Fatalf("%s should NOT be terminal", s)
		}
	}
}
```

Add the `digest` import to the test file.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tui/ -run 'TestRenderPipelineJob|TestJobIsTerminal'`
Expected: FAIL — `renderPipelineJob` / `jobIsTerminal` undefined.

- [ ] **Step 3: Add the render + helper** — append to `internal/tui/pipeline_view.go`:

```go
// jobIsTerminal reports whether a job's agent is gone (done/skipped/failed), so the
// detail pane should render the job's stored details instead of attaching to tmux.
func jobIsTerminal(s pipeline.JobStatus) bool {
	return s == pipeline.JobDone || s == pipeline.JobSkipped || s == pipeline.JobFailed
}

// renderPipelineJob draws one job's full details in the detail pane — used for
// terminal-status jobs whose agent has been reaped (no live tmux to attach).
func renderPipelineJob(j *pipeline.Job, width, height int) string {
	var b strings.Builder
	b.WriteString(stMuted.Render(jobGlyph(j.Status)+" job "+j.ID+" — "+string(j.Status)) + "\n")
	if len(j.DependsOn) > 0 {
		b.WriteString(stMuted.Render("deps: "+strings.Join(j.DependsOn, ",")) + "\n")
	}
	if j.Branch != "" {
		b.WriteString(stMuted.Render("branch: "+j.Branch) + "\n")
	}
	b.WriteString("\n" + stMuted.Render("Prompt") + "\n" + trunc(j.Prompt, max(0, width)) + "\n")
	if j.Handoff != "" {
		b.WriteString("\n" + stMuted.Render("Handoff") + "\n" + trunc(j.Handoff, max(0, width)) + "\n")
	}
	if j.Output != "" {
		b.WriteString("\n" + stMuted.Render("Output") + "\n" + trunc(j.Output, max(0, width)) + "\n")
	}
	if j.Digest != nil && j.Digest.Summary != "" {
		b.WriteString("\n" + stMuted.Render("Digest") + "\n" + trunc(j.Digest.Summary, max(0, width)) + "\n")
	}
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}
```

> If `trunc` collapses newlines (check its impl), drop it for `Prompt`/`Output` so multi-line text survives — the assertion only needs the substrings present.

- [ ] **Step 4: Wire the detail dispatch** — in `internal/tui/view.go`, add a case BEFORE the `default:` (after the `case cur.pipeline != nil:` block):

```go
		case cur.pjJob != nil && jobIsTerminal(cur.pjJob.Status):
			detailTitle = cur.pjPipe + "/" + cur.pjJob.ID
			detailBody = renderPipelineJob(cur.pjJob, detailOuter-2, bodyH-2)
```

> Confirm the item field names by reading `itemAt`/the item struct — `keys.go` uses `it.pjJob` (`*pipeline.Job`) and `it.pjPipe` (pipeline id string), so `cur.pjJob`/`cur.pjPipe` are the same fields on the selected item.

- [ ] **Step 5: Guard the `a` attach key** — in `internal/tui/keys.go`, replace the pipeline-job attach (`if it.pjJob != nil && it.pjJob.SessionID != "" { return m, attachCmd(it.pjJob.SessionID) }`) with:

```go
				if it.pjJob != nil {
					if jobIsTerminal(it.pjJob.Status) {
						m.status = "agent reaped — showing job details (d for digest)"
						return m, nil
					}
					if it.pjJob.SessionID != "" {
						return m, attachCmd(it.pjJob.SessionID)
					}
				}
```

- [ ] **Step 6: Run the TUI tests + build**

Run: `go test ./internal/tui/ && go build ./...`
Expected: PASS / no errors.

- [ ] **Step 7: Commit**

```bash
git add internal/tui/pipeline_view.go internal/tui/view.go internal/tui/keys.go internal/tui/pipeline_view_test.go
git commit -m "feat(tui): render reaped-job details in detail pane; guard attach"
```

---

### Task 8: Web — surface branch + digest in the done-job drawer

**Files:**
- Modify: `web/src/lib/types.ts` (`PipelineJob` — add `branch?`, `digest?`)
- Modify: `web/src/components/PipelinesTab.tsx` (drawer body)
- Test: `web/src/lib/pipelines.test.ts` (or a component test, matching the existing harness)

> The drawer already shows prompt/handoff/output and already gates "Open terminal" on `status === 'running'`, so terminal jobs already hide the terminal link. The only gap is showing **branch + digest**.

- [ ] **Step 1: Add the types** — in `web/src/lib/types.ts`, extend the `PipelineJob` type:

```ts
  branch?: string;
  digest?: { summary: string; turns?: number; branch?: string; task?: string };
```

- [ ] **Step 2: Write the failing test** — add to `web/src/lib/pipelines.test.ts` a pure helper test driving a new `jobDigestSummary` helper (keeps logic testable without a DOM):

```ts
import { jobDigestSummary } from './pipelines';

test('jobDigestSummary returns the digest summary when present', () => {
  expect(jobDigestSummary({ id: 'a', status: 'done', digest: { summary: 'did it' } } as any)).toBe('did it');
});
test('jobDigestSummary returns empty string when absent', () => {
  expect(jobDigestSummary({ id: 'a', status: 'running' } as any)).toBe('');
});
```

- [ ] **Step 3: Run to verify it fails**

Run: `cd web && npx vitest run src/lib/pipelines.test.ts`
Expected: FAIL — `jobDigestSummary` not exported.

- [ ] **Step 4: Add the helper** — in `web/src/lib/pipelines.ts`:

```ts
export function jobDigestSummary(j: PipelineJob): string {
  return j.digest?.summary ?? '';
}
```

(Import the `PipelineJob` type if the file doesn't already.)

- [ ] **Step 5: Render branch + digest in the drawer** — in `PipelinesTab.tsx`, inside the `drawerJob` block, after the `output` line (line ~78):

```tsx
          {drawerJob.branch && (<><label>Branch</label><pre className="job-text">{drawerJob.branch}</pre></>)}
          {jobDigestSummary(drawerJob) && (<><label>Digest</label><pre className="job-text">{jobDigestSummary(drawerJob)}</pre></>)}
```

Add `jobDigestSummary` to the existing import from `'../lib/pipelines'`.

- [ ] **Step 6: Run web tests + build**

Run: `cd web && npx vitest run && npm run build`
Expected: PASS / build succeeds (the daemon embeds `web/dist`, so the build must succeed for the UI to ship).

- [ ] **Step 7: Commit**

```bash
git add web/src/lib/types.ts web/src/lib/pipelines.ts web/src/lib/pipelines.test.ts web/src/components/PipelinesTab.tsx
git commit -m "feat(web): show branch + digest snapshot in reaped-job drawer"
```

---

### Task 9: Full verification, review, rebase, merge

- [ ] **Step 1: Full Go suite + vet**

Run: `go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 2: Web suite + production build**

Run: `cd web && npx vitest run && npm run build`
Expected: PASS.

- [ ] **Step 3: Release build (embeds web/dist)**

Run: `make release` (or the Makefile's build-with-embed target — check `Makefile`)
Expected: binary builds.

- [ ] **Step 4: Request code review**

Use superpowers:requesting-code-review on the branch diff. Fix any findings (commit per fix).

- [ ] **Step 5: Rebase onto master and re-verify**

```bash
git fetch --all 2>/dev/null || true
git rebase master   # resolve collisions with the concurrent delete-verb work in TUI/web files
go test ./... && (cd web && npx vitest run && npm run build)
```

Expected: clean rebase (or resolved), full suite green.

- [ ] **Step 6: Fast-forward merge to master and STOP**

```bash
git checkout master && git merge --ff-only <branch>
```

Do NOT push to any remote. Leave for the user: rebuild + reinstall the daemon (`make install` / reinstall script) so the running daemon serves the new behavior + web UI, then manual smoke — run a 2-job pipeline, `emit` job 1, confirm its agent's tmux is gone (`agentctl ls`), the detail pane / web drawer shows job details + digest, and `agentctl digest <job-session>` returns the snapshot.

---

## Self-Review

**Spec coverage:**
- §3 reap on emit (Terminate-only) → Task 4. ✅
- §4 artifacts survive → relied on by Task 5 (transcript/git) + Task 4 (no Teardown). ✅
- §5 `Job.Digest` field → Task 1. ✅
- §6 Emit flow + injected digestFn + async + WaitGroup → Tasks 3, 4. ✅
- §6.1 OnTransition no-op → verified in Task 4 Step 5 (existing guards). ✅
- §7 buildDigest extraction + serve snapshot → Tasks 2, 5. ✅
- §8 keep record + transcript → no deletion added anywhere (default behavior). ✅
- §9 TUI detail render + attach guard → Task 7. ✅
- §10 web drawer details + no terminal link (already gated) + digest → Task 8. ✅
- §11 default-on + `AGENTCTL_PIPELINE_KEEP_DONE` escape hatch → Tasks 3, 4, 6. ✅
- §12 testing → tests in Tasks 1–8. ✅

**Placeholder scan:** none — every code/test step shows the actual content. Two "read the file to confirm field/helper names" notes are verification guidance, not deferred work.

**Type consistency:** `digest.Digest` used consistently; `digestFn func(context.Context, *store.Session) digest.Digest` matches `BuildDigest`'s signature and the executor field; `jobIsTerminal`/`renderPipelineJob` names match between definition (Task 7 Step 3) and use (Steps 4–5); `jobDigestSummary` matches between web helper and drawer/test usage.
