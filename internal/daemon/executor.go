package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/curate"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/store"
)

// OwnerWaker delivers a delegated pipeline's push-wake to its owning orchestrator
// (design §2.3): body is landed durably in the owner's inbox and, if the owner is
// parked, its pane is nudged so an idle orchestrator blocked on wait_for_message
// actually wakes. Wired by the Server to the mailbox + lifecycle input path; nil ⇒
// delegated push-wake disabled (a server built without messaging simply never
// wakes anyone, and the DAG runs unchanged).
type OwnerWaker func(ownerID, body string)

// Curator is the memory auto-curation seam (#53 PR-2): the executor hands it one
// Signal per completed job on the EXISTING completion-digest hook, and it debounces a
// per-repo curation pass that PROPOSES unverified memory entries into the working
// tree (never commits/pushes). nil ⇒ curation disabled (the memory_curate default).
type Curator interface {
	Enqueue(ctx context.Context, workdir string, sig curate.Signal)
}

// digestSnapshotTimeout bounds the background completion-digest snapshot taken
// when a job emits. The builder may shell out to a `claude -p` narrator, so an
// untimed context.Background() could leak a goroutine for the daemon's lifetime
// if that call hangs; the snapshot degrades gracefully when it is cut short.
const digestSnapshotTimeout = 5 * time.Second

// commitTimeout bounds the auto-commit done on emit. It runs on a background
// context (not the emit request's) so a cancelled request can't abort the commit
// and leave the branch un-advanced for downstream from:<job> jobs.
const commitTimeout = 30 * time.Second

// Emit error sentinels (mapped to HTTP status by the route handler).
var (
	ErrJobNotFound     = errors.New("job not found in pipeline")
	ErrJobNotRunning   = errors.New("job is not running")
	ErrJobNotPending   = errors.New("job is not pending")
	ErrJobNotRetryable = errors.New("job is not in a retryable state")

	// Pause/resume guards (mapped to HTTP 409 by the route handlers).
	ErrNotPausable = errors.New("only a running pipeline can be paused")
	ErrNotPaused   = errors.New("pipeline is not paused")
)

// Executor performs the side effects the pure pipeline.Plan decides: spawning
// ready jobs, skipping failed branches, and persisting status. It is driven by
// Reconcile (after start/emit) and OnTransition (a job session errored).
type Executor struct {
	// mu serializes Reconcile end-to-end (read → Plan → spawn → persist) so two
	// concurrent triggers — an HTTP emit and a poller OnTransition — can never
	// both spawn the same newly-ready job (which would orphan a live agent and
	// falsely stall the pipeline). Pipelines are low-frequency, so a single
	// executor lock is simpler than per-pipeline locks and plenty fast.
	mu     sync.Mutex
	pstore *pipeline.Store
	sstore store.Store
	life   Lifecycle
	cstore *ctxstore.Store
	notify func() // signals SSE subscribers that state changed (may be nil)

	digestFn func(context.Context, *store.Session) digest.Digest // nil ⇒ skip snapshot
	curator  Curator                                             // nil ⇒ memory auto-curation disabled
	waker    OwnerWaker                                          // nil ⇒ delegated push-wake disabled
	keepDone bool                                                // pipeline_keep_done config setting — keep done agents alive
	snapWG   sync.WaitGroup                                      // tracks in-flight digest snapshots (test sync)
}

func NewExecutor(ps *pipeline.Store, ss store.Store, life Lifecycle, cs *ctxstore.Store, notify func()) *Executor {
	return &Executor{pstore: ps, sstore: ss, life: life, cstore: cs, notify: notify}
}

// Both setters are called once at server construction, before any concurrent use.

// SetDigestFn wires the digest builder used to snapshot a job's completion digest
// (bound to Server.buildDigest in production). nil ⇒ no snapshot.
func (e *Executor) SetDigestFn(fn func(context.Context, *store.Session) digest.Digest) {
	e.digestFn = fn
}

// SetKeepDoneAgents, when true, leaves a completed job's agent alive (skips the
// reap) so its tmux pane stays attachable for debugging.
func (e *Executor) SetKeepDoneAgents(v bool) { e.keepDone = v }

// SetCurator wires the memory auto-curation seam (#53 PR-2); nil (the default) leaves
// curation off. Set once at construction, before concurrent use.
func (e *Executor) SetCurator(c Curator) { e.curator = c }

// SetOwnerWaker wires the delegated-monitoring push-wake seam (A4); nil (the
// default) leaves delegated wakes off. Set once at construction, before concurrent
// use.
func (e *Executor) SetOwnerWaker(w OwnerWaker) { e.waker = w }

// wakeOwner push-wakes a delegated pipeline's owning orchestrator with body, but
// only when the pipeline has subscribed to monitoring (NotifyOwner) and carries an
// owner link. Best-effort and off the DAG's critical path: a delivery failure is
// swallowed by the waker so it can never block or fail a reconcile/emit.
func (e *Executor) wakeOwner(p *pipeline.Pipeline, body string) {
	if e.waker == nil || !p.NotifyOwner || p.OwnerID == "" {
		return
	}
	e.waker(p.OwnerID, body)
}

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

// Reconcile advances a pipeline: spawn newly-ready jobs, skip failed branches,
// and update the pipeline + job statuses.
func (e *Executor) Reconcile(ctx context.Context, pid string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, err := e.pstore.Get(pid)
	if err != nil {
		return err
	}
	if p.Status == pipeline.StatusCanceled || p.Status == pipeline.StatusDone {
		return nil
	}
	// Paused: in-flight jobs keep running (and may still emit/fail), but no new
	// job is spawned and no failed-branch skipping happens until Resume. Resume
	// re-enters Reconcile and settles the DAG from wherever it was left.
	if p.Status == pipeline.StatusPaused {
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
		// Convert job.Supervised (bool) to permission mode (string)
		permissionMode := ""
		if job.Supervised {
			permissionMode = "acceptEdits"
		}
		req := lifecycle.JobSpawnRequest{
			PipelineID: p.ID, JobID: job.ID, Repo: p.Repo,
			Prompt: pipeline.ComposePrompt(p, job), Worktree: worktree,
			BaseBranch: base, Type: store.NormalizeType(job.Type), PermissionMode: permissionMode,
			Role: job.Role, Tier: job.Tier, Backend: job.Backend, Model: job.Model,
			Tags: p.Tags, ScheduleID: p.ScheduleID, ScheduleName: p.ScheduleName,
		}
		if lvl, _ := e.life.MemoryPressure(ctx); lvl >= pressure.Warn {
			slog.Warn("pipeline: spawning job under memory pressure", "pipeline", req.PipelineID, "job", jobID, "pressure", lvl.String())
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
	prevStatus := p.Status
	var newStatus pipeline.Status
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
		newStatus = p.Status
	}); err != nil {
		return err
	}
	if e.notify != nil {
		e.notify()
	}
	// Delegated monitoring (A4): on the transition into a terminal state the DAG
	// has stopped and needs the owner's judgment, so push-wake the owning
	// orchestrator once. Fires exactly once — the prev!=new guard makes a repeated
	// stalled reconcile idempotent, and the top-of-Reconcile guard bails on an
	// already-done pipeline. Stalled is included so an owner blocked on
	// wait_for_message is released when a job fails, not left to time out.
	if newStatus != prevStatus && (newStatus == pipeline.StatusDone || newStatus == pipeline.StatusStalled) {
		e.wakeOwnerOnCompletion(pid, newStatus)
	}
	return nil
}

// wakeOwnerOnCompletion re-reads the pipeline and push-wakes its owner with an
// aggregate completion summary. Split out of Reconcile so the summary is composed
// off the reconcile fast path; a no-op when the pipeline is not a subscribed
// delegation.
func (e *Executor) wakeOwnerOnCompletion(pid string, status pipeline.Status) {
	p, err := e.pstore.Get(pid)
	if err != nil {
		return
	}
	e.wakeOwner(p, composeCompletionWake(p, status))
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

// Pause halts DAG progress: in-flight jobs keep running and may still emit or
// fail, but Reconcile spawns no new jobs while paused. Only a running pipeline
// can be paused. Held under e.mu so the status flip can't interleave with a
// Reconcile that's mid read→plan→spawn — that reconcile finishes its current
// batch, then the pause takes effect for the next one.
func (e *Executor) Pause(pid string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ferr error
	if err := e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		if p.Status != pipeline.StatusRunning {
			ferr = fmt.Errorf("%w (status %s)", ErrNotPausable, p.Status)
			return
		}
		p.Status = pipeline.StatusPaused
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

// Resume lifts a pause: flips paused→running and reconciles, spawning whatever
// became ready while paused (and skipping branches under any job that failed
// mid-pause). Only a paused pipeline can be resumed. Not held under e.mu across
// the whole call because Reconcile takes the lock itself.
func (e *Executor) Resume(ctx context.Context, pid string) error {
	var ferr error
	if err := e.pstore.Update(pid, func(p *pipeline.Pipeline) {
		if p.Status != pipeline.StatusPaused {
			ferr = fmt.Errorf("%w (status %s)", ErrNotPaused, p.Status)
			return
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

// OnTransition reconciles a job when its session changes state: errored/orphaned/
// done end the agent, idle flags stuck, working clears the flag. Driven by the
// poller for errored/orphaned/idle/working, and by handleEvent for the SessionEnd
// (done) hook — the poller skips terminal sessions, so done would otherwise never
// be reconciled. Job *completion* is still inferred only via `emit`; a done
// transition with the job still running means it exited without emitting.
func (e *Executor) OnTransition(sess *store.Session, _ store.Status, to store.Status) {
	if sess.PipelineID == "" {
		return
	}
	switch to {
	case store.StatusErrored, store.StatusOrphaned, store.StatusDone:
		// The session ended: errored/orphaned = died; done = the CLI exited (the
		// SessionEnd hook). A job still "running" here never emitted its handoff
		// (emit completes a job synchronously before the agent exits), so it failed
		// its contract → mark failed (descendants skip on reconcile). A job already
		// completed via emit is JobDone here and the guard leaves it untouched.
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
	case store.StatusWorking:
		// The agent resumed after being flagged idle — clear the attention flag.
		e.markJob(sess.PipelineID, sess.JobID, func(j *pipeline.Job) {
			if j.Status == pipeline.JobNeedsAttention {
				j.Status = pipeline.JobRunning
			}
		})
	default:
		return
	}
	_ = e.Reconcile(context.Background(), sess.PipelineID)
}

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

// Emit records a job's handoff: write to shared context, capture its branch from
// its session, mark it done, reap the agent, snapshot its digest, then reconcile
// to unblock dependents.
func (e *Executor) Emit(ctx context.Context, pid, jobID, text string) error {
	p, err := e.pstore.Get(pid)
	if err != nil {
		return err
	}
	job := p.Job(jobID)
	if job == nil {
		return ErrJobNotFound
	}
	// Only a running or needs_attention job may emit. This protects the
	// failure-skip invariant: a skipped job (failed ancestor) must not be
	// resurrected to "done", which would spawn its descendants off work that
	// never really completed. needs_attention is recoverable (the agent
	// finished after the grace window, or a human emits on its behalf).
	if job.Status != pipeline.JobRunning && job.Status != pipeline.JobNeedsAttention {
		return fmt.Errorf("%w (status %s)", ErrJobNotRunning, job.Status)
	}
	var sess *store.Session
	if job.SessionID != "" {
		sess, _ = e.sstore.Get(ctx, job.SessionID)
	}
	branch := ""
	if sess != nil {
		branch = sess.Branch
	}
	// Auto-commit the job's work before reaping so its branch advances. A
	// downstream from:<job> forks the branch ref, so an agent that finished
	// without committing would silently hand its dependents an empty base.
	// Only jobs with their OWN worktree: a worktree:none job runs in the repo
	// root (read-only/analysis) and must never be committed.
	if sess != nil && sess.Worktree != "" {
		cctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
		committed, cerr := e.life.CommitWorktree(cctx, sess.Workdir,
			fmt.Sprintf("pipeline %s/%s: %s", pid, jobID, text))
		cancel()
		switch {
		case cerr != nil:
			slog.Warn("pipeline: auto-commit failed (work may be uncommitted)", "pipeline", pid, "job", jobID, "workdir", sess.Workdir, "err", cerr)
		case !committed:
			slog.Info("pipeline: job produced no commits — downstream forks an empty base", "pipeline", pid, "job", jobID)
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
	// Delegated monitoring (A4): if this job is a declared callback point, push-wake
	// the owning orchestrator now — a decision point it asked to be woken for. The
	// pipeline's own completion is a separate wake from Reconcile. p, job, and the
	// owner/subscription fields are unchanged by the update above, so the snapshot is
	// safe to read here.
	if job.Callback {
		e.wakeOwner(p, composeCallbackWake(p, jobID, text))
	}
	// Reap the completed agent (free the slot + RAM) and snapshot its digest.
	// Terminate ONLY — never Teardown — so the worktree + branch survive for
	// downstream `from:<job>` jobs (same invariant as `rotate`).
	if sess != nil && !e.keepDone {
		// Background ctx: the reap must complete even if the emit request is cancelled.
		_ = e.life.Terminate(context.Background(), sess.TmuxSession)
		// Drop the now-redundant session record: the completed-job details
		// (output/branch/digest) all live on the pipeline job, so the agent row
		// would otherwise linger only to be re-classified "orphaned" by the poller
		// once its tmux session is gone. The digest snapshot below reads the
		// in-memory sess struct + on-disk transcript, so deletion can't race it.
		_ = e.sstore.Delete(context.Background(), sess.ID)
		if e.digestFn != nil {
			e.snapWG.Add(1)
			go func(s *store.Session) {
				defer e.snapWG.Done()
				dctx, cancel := context.WithTimeout(context.Background(), digestSnapshotTimeout)
				defer cancel()
				d := e.digestFn(dctx, s)
				d.Status = string(store.StatusDone)
				_ = e.pstore.Update(pid, func(p *pipeline.Pipeline) {
					if j := p.Job(jobID); j != nil {
						j.Digest = &d
					}
				})
				// Feed the completion digest to memory auto-curation (#53 PR-2). This
				// is the EXISTING completion-digest hook the design attaches to; the
				// curator DEBOUNCES per repo so a burst of job completions coalesces
				// into one pass, and it writes PROPOSALS to the working tree only —
				// never commits or pushes. Best-effort and off the critical path.
				if e.curator != nil && s.Workdir != "" {
					e.curator.Enqueue(context.Background(), s.Workdir, signalFromDigest(s, d))
				}
				if e.notify != nil {
					e.notify()
				}
			}(sess)
		}
	}
	return e.Reconcile(ctx, pid)
}

// composeCallbackWake builds the directed-message body warden delivers to a
// delegated pipeline's owner when a callback-point job completes. It carries the
// job's handoff output inline so the orchestrator can decide without a follow-up
// read (spending a turn only on the decision itself).
func composeCallbackWake(p *pipeline.Pipeline, jobID, output string) string {
	return fmt.Sprintf(
		"⏸ delegated pipeline %q reached callback point %q.\n\nJob output:\n%s\n\n"+
			"You are being woken because this job is a declared decision point. Inspect it "+
			"(show_pipeline %s), then act — the pipeline continues on its own until the next "+
			"callback or completion.",
		p.Name, jobID, strings.TrimSpace(output), p.ID)
}

// composeCompletionWake builds the aggregate wake body warden delivers to a
// delegated pipeline's owner when the DAG reaches a terminal state — a compact
// per-job status roll-up so the orchestrator sees the outcome without reading each
// job (details remain in show_pipeline / shared context).
func composeCompletionWake(p *pipeline.Pipeline, status pipeline.Status) string {
	var head string
	switch status {
	case pipeline.StatusStalled:
		head = fmt.Sprintf("⚠️ delegated pipeline %q stalled — a job failed with nothing downstream to handle it and it needs your attention (retry or cancel).", p.Name)
	default: // StatusDone
		head = fmt.Sprintf("✅ delegated pipeline %q completed.", p.Name)
	}
	var b strings.Builder
	b.WriteString(head)
	b.WriteString("\n\nJobs:")
	for i := range p.Jobs {
		j := &p.Jobs[i]
		fmt.Fprintf(&b, "\n  - %s: %s", j.ID, j.Status)
		if j.Branch != "" {
			fmt.Fprintf(&b, " [%s]", j.Branch)
		}
	}
	fmt.Fprintf(&b, "\n\nFull outputs are in show_pipeline %s.", p.ID)
	return b.String()
}

// signalFromDigest projects a completed job's digest + session into the neutral
// curate.Signal the curation pass reads — the agent id and any produced branch become
// the entry provenance, the files/summary the extraction evidence. It carries no
// digest/curate coupling beyond this one adapter.
func signalFromDigest(s *store.Session, d digest.Digest) curate.Signal {
	files := make([]string, 0, len(d.Files))
	for _, f := range d.Files {
		files = append(files, f.Path)
	}
	return curate.Signal{
		Task:    d.Task,
		Summary: d.Summary,
		Files:   files,
		Branch:  d.Branch,
		Agent:   s.ID,
	}
}

// SweepDoneJobSessions deletes the session records of pipeline jobs that have
// already completed (JobDone). Emit reaps such sessions going forward; this
// retroactively clears the backlog left by older builds that killed the agent's
// tmux session without dropping its store record — where the poller then
// re-classified it "orphaned" and it piled up on the dashboard. Only JobDone
// sessions are touched: running/failed/needs-attention jobs keep their records
// (still needed for attach and retry), as do non-pipeline agents. It returns the
// number of records removed and is a no-op under keep-done (those agents are
// intentionally kept alive). Best-effort: a session whose pipeline has since been
// deleted, or whose delete fails, is simply left in place.
func (e *Executor) SweepDoneJobSessions(ctx context.Context) (int, error) {
	if e.keepDone {
		return 0, nil
	}
	sessions, err := e.sstore.List(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, s := range sessions {
		if s.PipelineID == "" || s.JobID == "" {
			continue
		}
		p, err := e.pstore.Get(s.PipelineID)
		if err != nil {
			continue
		}
		j := p.Job(s.JobID)
		if j == nil || j.Status != pipeline.JobDone {
			continue
		}
		// The job already emitted; ensure the agent's tmux is reaped (no-op if it
		// already died) and drop the redundant record.
		_ = e.life.Terminate(ctx, s.TmuxSession)
		if err := e.sstore.Delete(ctx, s.ID); err == nil {
			removed++
		}
	}
	return removed, nil
}
