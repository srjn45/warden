# Pipeline Control Refinements (Phase 4b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the three pipeline control refinements deferred from Phase 4a: `pipeline edit-job` (tweak a pending job's prompt/handoff), `pipeline retry` (re-run a failed/stuck job and reopen its skipped descendants), and a `needs_attention` job state so a job whose agent goes quiet without emitting is flagged rather than leaving the pipeline silently stuck.

**Architecture:** Builds on the Phase 4a `internal/pipeline` + `daemon.Executor`. The grace window needs no new timer: the poller's existing 5-minute stuck-detection already turns a quiet `working` session into `idle`, so `Executor.OnTransition` simply maps a running pipeline-job's session `→ idle` to `needs_attention`. New executor methods `EditJob`/`Retry` mirror the existing pattern (mutex-serialized where they reconcile), with new routes/client/CLI surface.

**Tech Stack:** Go, chi, cobra. Module: `github.com/srajanpathak/agentctl`. Reference: design doc §8 (grace window / needs-attention) and §11 (edit-job/retry).

## Key behaviors
- **`needs_attention`** is recoverable, not a failure: it does NOT skip descendants and does NOT unblock them; the pipeline stays `running` with the job flagged. It self-heals if the agent later emits (emit is allowed on a `needs_attention` job), or is cleared by `retry`.
- **`retry`** tears down the old (failed/stuck) job session + its worktree, deletes the stale session record (so the re-spawn's session id `<pid>-<job>` is free), resets the job to `pending`, reopens all `skipped` jobs (the reconcile's `Plan` re-skips any still blocked by other failures), and reconciles.
- **`edit-job`** only touches a `pending` job (a running/done job's prompt is meaningless to change).

---

## File Structure

- **Modify** `internal/pipeline/pipeline.go` — add the `JobNeedsAttention` status const.
- **Modify** `internal/pipeline/plan.go` — `pipelineStatus` counts `needs_attention` as in-progress.
- **Modify** `internal/pipeline/plan_test.go` — needs_attention status test.
- **Modify** `internal/daemon/executor.go` — `ErrJobNotPending`/`ErrJobNotRetryable`; `EditJob`; `Retry`; `OnTransition` idle→needs_attention; relax `Emit` guard.
- **Modify** `internal/daemon/executor_test.go` — EditJob/Retry/needs_attention/emit tests.
- **Modify** `internal/daemon/pipeline_routes.go` — `edit` + `retry` routes/handlers.
- **Modify** `internal/daemon/pipeline_routes_test.go` — route tests.
- **Modify** `internal/client/client.go` — `PipelineEditJob` + `PipelineRetry`.
- **Modify** `internal/client/client_test.go` — client tests.
- **Modify** `internal/cli/pipeline.go` — `edit-job` + `retry` subcommands.
- **Modify** `docs/USAGE.md` — document the new commands + needs_attention.

---

## Task 1: `needs_attention` status + Plan handling

**Files:**
- Modify: `internal/pipeline/pipeline.go`
- Modify: `internal/pipeline/plan.go`
- Test: `internal/pipeline/plan_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/pipeline/plan_test.go`:

```go
func TestPlanNeedsAttentionKeepsRunningAndBlocks(t *testing.T) {
	// b is needs_attention: pipeline stays running, b does NOT unblock its
	// dependent d, and b is NOT a failure so c/d are not skipped.
	d := Plan(diamond(map[string]JobStatus{"a": JobDone, "b": JobNeedsAttention, "c": JobDone, "d": JobPending}))
	if d.Status != StatusRunning {
		t.Fatalf("needs_attention should keep pipeline running, got %s", d.Status)
	}
	for _, id := range d.Spawn {
		if id == "d" {
			t.Fatalf("d must not spawn while dep b is needs_attention")
		}
	}
	if len(d.Skip) != 0 {
		t.Fatalf("needs_attention must not skip anything, got %v", d.Skip)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pipeline/ -run TestPlanNeedsAttention`
Expected: FAIL — `undefined: JobNeedsAttention` (the const doesn't exist yet).

- [ ] **Step 3: Write minimal implementation**

In `internal/pipeline/pipeline.go`, add the const to the `JobStatus` block (after `JobSkipped`):

```go
	JobSkipped        JobStatus = "skipped"
	JobNeedsAttention JobStatus = "needs_attention"
```

In `internal/pipeline/plan.go`, in `pipelineStatus`, count `needs_attention` as in-progress so the pipeline reads `running` (not `pending`/`done`) while a job is flagged. Change the `anyRunning` assignment:

```go
		if s == JobRunning || s == JobNeedsAttention {
			anyRunning = true
		}
```

(No other change is needed: the spawn loop only considers `JobPending`, and dependents require deps to be `JobDone`, so a `needs_attention` job neither spawns, unblocks, nor — since only `JobFailed` triggers skipping — skips anything. The existing `allTerminal` check already treats `needs_attention` as non-terminal.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pipeline/...`
Expected: PASS (all pipeline tests, including the new one).

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/pipeline.go internal/pipeline/plan.go internal/pipeline/plan_test.go
git commit -m "feat(pipeline): add needs_attention job status (keeps pipeline running)"
```

---

## Task 2: Executor.EditJob

**Files:**
- Modify: `internal/daemon/executor.go`
- Test: `internal/daemon/executor_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/executor_test.go`:

```go
func TestEditJobOnlyWhenPending(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())

	newPrompt := "do it differently"
	newHandoff := "the branch name"
	if err := e.EditJob("p", "a", &newPrompt, &newHandoff); err != nil {
		t.Fatalf("EditJob pending: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Prompt != "do it differently" || got.Job("a").Handoff != "the branch name" {
		t.Fatalf("edit not applied: %+v", got.Job("a"))
	}

	// only one field provided → the other is untouched.
	onlyPrompt := "again"
	e.EditJob("p", "a", &onlyPrompt, nil)
	got, _ = ps.Get("p")
	if got.Job("a").Prompt != "again" || got.Job("a").Handoff != "the branch name" {
		t.Fatalf("partial edit wrong: %+v", got.Job("a"))
	}

	// running job → rejected.
	ps.Update("p", func(p *pipeline.Pipeline) { p.Job("a").Status = pipeline.JobRunning })
	if err := e.EditJob("p", "a", &onlyPrompt, nil); !errors.Is(err, ErrJobNotPending) {
		t.Fatalf("editing a running job should fail with ErrJobNotPending, got %v", err)
	}

	// unknown job → ErrJobNotFound.
	if err := e.EditJob("p", "ghost", &onlyPrompt, nil); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}
```

Ensure `internal/daemon/executor_test.go` imports `"errors"` (add it to the import block if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestEditJob`
Expected: FAIL — `e.EditJob undefined` / `undefined: ErrJobNotPending`.

- [ ] **Step 3: Write minimal implementation**

In `internal/daemon/executor.go`, add the sentinel to the existing `var (...)` error block:

```go
	ErrJobNotFound    = errors.New("job not found in pipeline")
	ErrJobNotRunning  = errors.New("job is not running")
	ErrJobNotPending  = errors.New("job is not pending")
	ErrJobNotRetryable = errors.New("job is not in a retryable state")
```

Add the method:

```go
// EditJob updates a PENDING job's prompt and/or handoff (nil = leave unchanged).
// Held under the executor lock so it can't race a Reconcile that's about to
// spawn the same job.
func (e *Executor) EditJob(pid, jobID string, prompt, handoff *string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ferr error
	if err := e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		j := p.Job(jobID)
		if j == nil {
			ferr = ErrJobNotFound
			return
		}
		if j.Status != pipeline.JobPending {
			ferr = ErrJobNotPending
			return
		}
		if prompt != nil {
			j.Prompt = *prompt
		}
		if handoff != nil {
			j.Handoff = *handoff
		}
	}); err != nil {
		return err
	}
	if ferr != nil {
		return ferr
	}
	if e.notify != nil {
		e.notify()
	}
	return nil
}
```

(`ErrJobNotRetryable` is unused until Task 3 — that's fine for a package-level var; Go does not flag unused package-level declarations.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestEditJob`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/executor.go internal/daemon/executor_test.go
git commit -m "feat(daemon): Executor.EditJob (edit a pending job's prompt/handoff)"
```

---

## Task 3: Executor.Retry

**Files:**
- Modify: `internal/daemon/executor.go`
- Test: `internal/daemon/executor_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/executor_test.go`:

```go
func TestRetryResetsFailedJobAndReopensDescendants(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain()) // a -> b
	// Put the pipeline in a stalled state: a failed (with a stale session), b skipped.
	ss.Insert(context.Background(), &store.Session{ID: "p-a", TmuxSession: "p-a", PipelineID: "p", JobID: "a", Status: store.StatusOrphaned})
	ps.Update("p", func(p *pipeline.Pipeline) {
		p.Job("a").Status = pipeline.JobFailed
		p.Job("a").SessionID = "p-a"
		p.Job("b").Status = pipeline.JobSkipped
		p.Status = pipeline.StatusStalled
	})

	if err := e.Retry(context.Background(), "p", "a"); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("retried a should be running, got %s", got.Job("a").Status)
	}
	if got.Job("b").Status != pipeline.JobPending {
		t.Fatalf("skipped descendant b should be reopened to pending, got %s", got.Job("b").Status)
	}
	if got.Status != pipeline.StatusRunning {
		t.Fatalf("pipeline should be running again, got %s", got.Status)
	}
	// the stale session was deleted so the re-spawn could reuse the id.
	if _, err := ss.Get(context.Background(), "p-a"); err == nil {
		// a fresh p-a was inserted by the re-spawn; confirm it's the new running one.
		s, _ := ss.Get(context.Background(), "p-a")
		if s.Status == store.StatusOrphaned {
			t.Fatalf("stale orphaned session was not replaced")
		}
	}
}

func TestRetryRejectsNonRetryable(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	// a is pending (not failed/needs_attention) → not retryable.
	if err := e.Retry(context.Background(), "p", "a"); !errors.Is(err, ErrJobNotRetryable) {
		t.Fatalf("want ErrJobNotRetryable, got %v", err)
	}
	if err := e.Retry(context.Background(), "p", "ghost"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run TestRetry`
Expected: FAIL — `e.Retry undefined`.

- [ ] **Step 3: Write minimal implementation**

In `internal/daemon/executor.go`, add:

```go
// Retry re-runs a failed or needs_attention job: it tears down the job's stale
// session + worktree, drops the stale session record (so the re-spawn can reuse
// the <pid>-<job> id), resets the job to pending, reopens all skipped jobs (the
// reconcile's Plan re-skips any still blocked by another failure), and
// reconciles. Not held under e.mu across the whole call because Reconcile takes
// the lock itself.
func (e *Executor) Retry(ctx context.Context, pid, jobID string) error {
	p, err := e.pstore.Get(pid)
	if err != nil {
		return err
	}
	job := p.Job(jobID)
	if job == nil {
		return ErrJobNotFound
	}
	if job.Status != pipeline.JobFailed && job.Status != pipeline.JobNeedsAttention {
		return ErrJobNotRetryable
	}
	// Clean up the stale session + worktree so the re-spawn's id is free.
	if job.SessionID != "" {
		if sess, gerr := e.sstore.Get(ctx, job.SessionID); gerr == nil {
			_ = e.life.Teardown(ctx, sess)
			_ = e.sstore.Delete(ctx, sess.ID)
		}
	}
	var ferr error
	if err := e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		j := p.Job(jobID)
		if j == nil {
			ferr = ErrJobNotFound
			return
		}
		j.Status = pipeline.JobPending
		j.SessionID = ""
		j.Output = ""
		j.Branch = ""
		for i := range p.Jobs {
			if p.Jobs[i].Status == pipeline.JobSkipped {
				p.Jobs[i].Status = pipeline.JobPending
			}
		}
		p.Status = pipeline.StatusRunning
	}); err != nil {
		return err
	}
	if ferr != nil {
		return ferr
	}
	return e.Reconcile(ctx, pid)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run TestRetry`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/executor.go internal/daemon/executor_test.go
git commit -m "feat(daemon): Executor.Retry (re-run failed/stuck job, reopen descendants)"
```

---

## Task 4: needs_attention detection + relaxed emit guard

**Files:**
- Modify: `internal/daemon/executor.go` (`OnTransition`, `Emit`)
- Test: `internal/daemon/executor_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/executor_test.go`:

```go
func TestOnTransitionIdleFlagsNeedsAttention(t *testing.T) {
	e, ps, ss := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p") // a running, session p-a inserted
	sess, _ := ss.Get(context.Background(), "p-a")

	e.OnTransition(sess, store.StatusWorking, store.StatusIdle)
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobNeedsAttention {
		t.Fatalf("idle running job should be needs_attention, got %s", got.Job("a").Status)
	}
	if got.Status != pipeline.StatusRunning {
		t.Fatalf("pipeline should stay running, got %s", got.Status)
	}
}

func TestEmitAllowedOnNeedsAttention(t *testing.T) {
	e, ps, _ := newTestExecutor(t)
	ps.Create(chain())
	e.Reconcile(context.Background(), "p")
	ps.Update("p", func(p *pipeline.Pipeline) { p.Job("a").Status = pipeline.JobNeedsAttention })

	if err := e.Emit(context.Background(), "p", "a", "finished after all"); err != nil {
		t.Fatalf("emit on needs_attention should be allowed: %v", err)
	}
	got, _ := ps.Get("p")
	if got.Job("a").Status != pipeline.JobDone {
		t.Fatalf("emit should mark needs_attention job done, got %s", got.Job("a").Status)
	}
	if got.Job("b").Status != pipeline.JobRunning {
		t.Fatalf("dependent b should now run, got %s", got.Job("b").Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestOnTransitionIdle|TestEmitAllowedOnNeedsAttention'`
Expected: FAIL — idle isn't handled (job stays running), and emit rejects non-running.

- [ ] **Step 3: Write minimal implementation**

In `internal/daemon/executor.go`, replace the body of `OnTransition` with a switch that also handles `idle`:

```go
func (e *Executor) OnTransition(sess *store.Session, _ store.Status, to store.Status) {
	if sess.PipelineID == "" {
		return
	}
	switch to {
	case store.StatusErrored, store.StatusOrphaned:
		// The session died → the job failed; descendants get skipped on reconcile.
		e.markJob(sess.PipelineID, sess.JobID, func(j *pipeline.Job) {
			if j.Status == pipeline.JobRunning {
				j.Status = pipeline.JobFailed
			}
		})
	case store.StatusIdle:
		// The poller's stuck-detection (quiet ≥ stuckAfter) is the grace window:
		// a running job whose agent went quiet without emitting is flagged for
		// attention (recoverable — it self-heals on a later emit, or via retry).
		e.markJob(sess.PipelineID, sess.JobID, func(j *pipeline.Job) {
			if j.Status == pipeline.JobRunning {
				j.Status = pipeline.JobNeedsAttention
			}
		})
	default:
		return
	}
	_ = e.Reconcile(context.Background(), sess.PipelineID)
}
```

Then relax the `Emit` guard so a `needs_attention` job can emit (the agent finished after the grace window, or a human emits on its behalf). Change the guard in `Emit`:

```go
	if job.Status != pipeline.JobRunning && job.Status != pipeline.JobNeedsAttention {
		return fmt.Errorf("%w (status %s)", ErrJobNotRunning, job.Status)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/daemon/ -run 'TestOnTransition|TestEmit'`
Expected: PASS (the new tests and the existing `TestOnTransitionFailsJobAndSkipsDescendants` / `TestPipelineEmitMarksDone`).

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/executor.go internal/daemon/executor_test.go
git commit -m "feat(daemon): flag idle pipeline jobs needs_attention; allow emit on them"
```

---

## Task 5: edit + retry routes

**Files:**
- Modify: `internal/daemon/pipeline_routes.go`
- Test: `internal/daemon/pipeline_routes_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/daemon/pipeline_routes_test.go`:

```go
func TestPipelineEditJobRoute(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) // job "a", pending

	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/edit", "application/json", strings.NewReader(`{"prompt":"new prompt"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Prompt != "new prompt" {
		t.Fatalf("prompt not edited: %q", got.Job("a").Prompt)
	}
}

func TestPipelineEditJobNothing400(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	resp, _ := http.Post(ts.URL+"/pipelines/demo/jobs/a/edit", "application/json", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty edit want 400, got %d", resp.StatusCode)
	}
}

func TestPipelineRetryRoute(t *testing.T) {
	ts, ps := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody))
	// force job a into a failed state.
	ps.Update("demo", func(p *pipeline.Pipeline) {
		p.Job("a").Status = pipeline.JobFailed
		p.Status = pipeline.StatusStalled
	})

	resp, err := http.Post(ts.URL+"/pipelines/demo/jobs/a/retry", "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry status %d", resp.StatusCode)
	}
	got, _ := ps.Get("demo")
	if got.Job("a").Status != pipeline.JobRunning {
		t.Fatalf("retried job should be running, got %s", got.Job("a").Status)
	}
}

func TestPipelineRetryNotRetryable409(t *testing.T) {
	ts, _ := newPipeServer(t)
	defer ts.Close()
	http.Post(ts.URL+"/pipelines", "application/json", strings.NewReader(yamlBody)) // a pending
	resp, _ := http.Post(ts.URL+"/pipelines/demo/jobs/a/retry", "application/json", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("retry pending want 409, got %d", resp.StatusCode)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/daemon/ -run 'TestPipelineEditJob|TestPipelineRetry'`
Expected: FAIL — routes return 404/405 (not registered) so the status assertions fail.

- [ ] **Step 3: Write the handlers + register**

In `internal/daemon/pipeline_routes.go`, add the two routes to `registerPipelineRoutes` (after the emit route):

```go
	r.Post("/pipelines/{pid}/jobs/{job}/emit", s.handleEmit)
	r.Post("/pipelines/{pid}/jobs/{job}/edit", s.handleEditJob)
	r.Post("/pipelines/{pid}/jobs/{job}/retry", s.handleRetry)
```

Add the request type near the other route types:

```go
type editJobRequest struct {
	Prompt  *string `json:"prompt,omitempty"`
	Handoff *string `json:"handoff,omitempty"`
}
```

Add the handlers:

```go
func (s *Server) handleEditJob(w http.ResponseWriter, r *http.Request) {
	pid, job := chi.URLParam(r, "pid"), chi.URLParam(r, "job")
	var req editJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Prompt == nil && req.Handoff == nil {
		writeErr(w, http.StatusBadRequest, "nothing to edit (provide prompt and/or handoff)")
		return
	}
	err := s.exec.EditJob(pid, job, req.Prompt, req.Handoff)
	switch {
	case errors.Is(err, pipeline.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotPending):
		writeErr(w, http.StatusConflict, "job is not pending (can only edit before it starts)")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "edited"})
	}
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	pid, job := chi.URLParam(r, "pid"), chi.URLParam(r, "job")
	// Background context: retry reconciles and may spawn worktree jobs.
	err := s.exec.Retry(context.Background(), pid, job)
	switch {
	case errors.Is(err, pipeline.ErrNotFound):
		writeErr(w, http.StatusNotFound, "pipeline not found")
	case errors.Is(err, ErrJobNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, ErrJobNotRetryable):
		writeErr(w, http.StatusConflict, "job is not failed or needs-attention")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "retrying"})
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/daemon/ -run 'TestPipelineEditJob|TestPipelineRetry'`
Expected: PASS.

Run: `go test ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/pipeline_routes.go internal/daemon/pipeline_routes_test.go
git commit -m "feat(daemon): pipeline edit-job + retry routes"
```

---

## Task 6: Client methods + CLI subcommands

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/cli/pipeline.go`
- Test: `internal/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/client/client_test.go`:

```go
func TestPipelineEditJobAndRetry(t *testing.T) {
	var editBody, retryPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/edit"):
			b, _ := io.ReadAll(r.Body)
			editBody = string(b)
			w.Write([]byte(`{"status":"edited"}`))
		case strings.HasSuffix(r.URL.Path, "/retry"):
			retryPath = r.URL.Path
			w.Write([]byte(`{"status":"retrying"}`))
		}
	}))
	defer ts.Close()
	c := New(ts.URL)

	p := "new prompt"
	if err := c.PipelineEditJob(context.Background(), "demo", "a", &p, nil); err != nil {
		t.Fatalf("PipelineEditJob: %v", err)
	}
	if !strings.Contains(editBody, `"prompt":"new prompt"`) || strings.Contains(editBody, "handoff") {
		t.Fatalf("edit body wrong: %s", editBody)
	}
	if err := c.PipelineRetry(context.Background(), "demo", "a"); err != nil {
		t.Fatalf("PipelineRetry: %v", err)
	}
	if retryPath != "/pipelines/demo/jobs/a/retry" {
		t.Fatalf("retry path %s", retryPath)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/client/ -run TestPipelineEditJobAndRetry`
Expected: FAIL — `c.PipelineEditJob undefined` / `c.PipelineRetry undefined`.

- [ ] **Step 3: Add client methods**

Append to `internal/client/client.go`:

```go
// PipelineEditJob updates a pending job's prompt and/or handoff (nil = unchanged).
func (c *Client) PipelineEditJob(ctx context.Context, pid, job string, prompt, handoff *string) error {
	body := map[string]*string{}
	if prompt != nil {
		body["prompt"] = prompt
	}
	if handoff != nil {
		body["handoff"] = handoff
	}
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/edit"
	return c.do(ctx, http.MethodPost, path, body, nil)
}

// PipelineRetry re-runs a failed/needs-attention job.
func (c *Client) PipelineRetry(ctx context.Context, pid, job string) error {
	path := "/pipelines/" + url.PathEscape(pid) + "/jobs/" + url.PathEscape(job) + "/retry"
	// longTimeout: retry reconciles and may spawn a worktree job.
	return c.doT(ctx, longTimeout, http.MethodPost, path, nil, nil)
}
```

- [ ] **Step 4: Run client test to verify it passes**

Run: `go test ./internal/client/ -run TestPipelineEditJobAndRetry`
Expected: PASS.

- [ ] **Step 5: Add the CLI subcommands**

In `internal/cli/pipeline.go`, register them in `newPipelineCmd`'s `AddCommand` call:

```go
	cmd.AddCommand(newPipelineCreateCmd(), newPipelineListCmd(), newPipelineShowCmd(),
		newPipelineStartCmd(), newPipelineCancelCmd(), newPipelineEmitCmd(),
		newPipelineEditJobCmd(), newPipelineRetryCmd())
```

Add the two commands (the file already imports `fmt`, `os`, `strings`, cobra):

```go
func newPipelineEditJobCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit-job <pipeline> <job>",
		Short: "Edit a pending job's prompt and/or handoff",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var prompt, handoff *string
			if cmd.Flags().Changed("prompt") {
				v, _ := cmd.Flags().GetString("prompt")
				prompt = &v
			}
			if cmd.Flags().Changed("handoff") {
				v, _ := cmd.Flags().GetString("handoff")
				handoff = &v
			}
			if prompt == nil && handoff == nil {
				return fmt.Errorf("provide --prompt and/or --handoff")
			}
			if err := clientFor(cmd).PipelineEditJob(cmd.Context(), args[0], args[1], prompt, handoff); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "edited %s/%s\n", args[0], args[1])
			return nil
		},
	}
	cmd.Flags().String("prompt", "", "new prompt for the job")
	cmd.Flags().String("handoff", "", "new handoff hint for the job")
	return cmd
}

func newPipelineRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <pipeline> <job>",
		Short: "Re-run a failed or needs-attention job (reopens skipped descendants)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := clientFor(cmd).PipelineRetry(cmd.Context(), args[0], args[1]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "retrying %s/%s\n", args[0], args[1])
			return nil
		},
	}
}
```

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/client/... ./internal/cli/... && go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/client/client.go internal/client/client_test.go internal/cli/pipeline.go
git commit -m "feat(client,cli): pipeline edit-job + retry"
```

---

## Task 7: Docs + full verification

**Files:**
- Modify: `docs/USAGE.md`

- [ ] **Step 1: Extend the Pipelines section**

Read `docs/USAGE.md`'s Pipelines section (added in Phase 4a) and append, in the same style, coverage of the new commands + the needs_attention state:

```markdown
Editing and recovery:

    agentctl pipeline edit-job <pipeline> <job> --prompt "..." --handoff "..."
    agentctl pipeline retry <pipeline> <job>

`edit-job` tweaks a job's prompt and/or handoff *before it starts* (pending jobs
only). If a job's agent goes quiet without emitting (its session is flagged
`idle` by stuck-detection), the job is marked **`needs_attention`** rather than
silently stalling — the pipeline stays `running` and the job is shown flagged.
Resolve it by `pipeline emit`-ing on the job's behalf (if the agent actually
finished) or `pipeline retry`, which tears down the stale job session/worktree,
resets the job, reopens any descendants that were skipped, and re-runs from there.
```

- [ ] **Step 2: Run the full suite (with -race)**

Run: `go build ./... && go test ./... && go test -race ./internal/pipeline/... ./internal/daemon/... && make lint`
Expected: PASS across all packages; lint (go vet) clean. If anything fails, do NOT commit — report it.

- [ ] **Step 3: Commit**

```bash
git add docs/USAGE.md
git commit -m "docs: document pipeline edit-job, retry, and needs_attention"
```

---

## Verification checklist (after all tasks)

- [ ] `go build ./...` clean; `go test ./...` green; `go test -race ./internal/pipeline/... ./internal/daemon/...` green; `make lint` clean.
- [ ] Manual smoke (rebuild + restart daemon: `./scripts/reinstall.sh`):
  - On a running pipeline, `agentctl pipeline edit-job <p> <pendingJob> --prompt "..."` before it starts, then `pipeline show` reflects the new prompt.
  - Force a job to fail (terminate its agent so its session orphans), confirm `pipeline show` shows it `failed` and its descendant `skipped` and the pipeline `stalled`; then `agentctl pipeline retry <p> <job>` → the job re-spawns (`running`) and the descendant returns to `pending`.
  - Leave a job's agent idle for the stuck window and confirm `pipeline show` flags it `needs_attention` while the pipeline stays `running`; `pipeline emit` (or `retry`) clears it.
```
