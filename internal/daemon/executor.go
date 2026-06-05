package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/srajanpathak/agentctl/internal/ctxstore"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/pipeline"
	"github.com/srajanpathak/agentctl/internal/pressure"
	"github.com/srajanpathak/agentctl/internal/store"
)

// Emit error sentinels (mapped to HTTP status by the route handler).
var (
	ErrJobNotFound     = errors.New("job not found in pipeline")
	ErrJobNotRunning   = errors.New("job is not running")
	ErrJobNotPending   = errors.New("job is not pending")
	ErrJobNotRetryable = errors.New("job is not in a retryable state")
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
	keepDone bool                                                 // AGENTCTL_PIPELINE_KEEP_DONE — keep done agents alive
	snapWG   sync.WaitGroup                                       // tracks in-flight digest snapshots (test sync)
}

func NewExecutor(ps *pipeline.Store, ss store.Store, life Lifecycle, cs *ctxstore.Store, notify func()) *Executor {
	return &Executor{pstore: ps, sstore: ss, life: life, cstore: cs, notify: notify}
}

// Both setters are called once at server construction, before any concurrent use.

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
		if lvl, _ := e.life.MemoryPressure(ctx); lvl >= pressure.Warn {
			log.Printf("pipeline %s job %s: spawning under memory pressure (%s)", req.PipelineID, jobID, lvl)
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

// OnTransition is the poller hook: when a job's session errors, is orphaned, or
// goes idle (stuck-detection grace window elapsed), the job status is updated
// and the pipeline reconciled accordingly.
// Job completion is NOT inferred here — that comes only via `emit`.
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
		// Background ctx: the reap must complete even if the emit request is cancelled.
		_ = e.life.Terminate(context.Background(), sess.TmuxSession)
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
}
