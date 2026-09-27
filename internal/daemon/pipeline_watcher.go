package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// pwPipelineStore is the pipeline-store subset PipelineWatcher needs.
type pwPipelineStore interface {
	List() ([]*pipeline.Pipeline, error)
	Update(id string, fn func(*pipeline.Pipeline)) error
}

// pwSessionStore is the session-store subset PipelineWatcher needs for orphan
// cross-checks.
type pwSessionStore interface {
	Get(ctx context.Context, id string) (*store.Session, error)
}

// PipelineWatcher monitors running pipelines at a fixed cadence and nudges the
// Executor when it detects abnormal states: stuck jobs, stalls, and orphaned
// agents. It never spawns jobs — that is Executor.Reconcile's responsibility.
type PipelineWatcher struct {
	pstore          pwPipelineStore
	sstore          pwSessionStore
	exec            *Executor
	stuckRetryAfter time.Duration
	stallAfter      time.Duration
	autoRetry       bool

	// brain consult fields — all nil/zero when feature is off.
	consultor  brainconsult.Consultor // nil = feature off
	consultSem chan struct{}          // buffered semaphore (cap = brainConsultMaxCon)
	life       Lifecycle              // needed for nudge_agent path
	consulted  map[string]bool        // dedupe: key = "pid/jobID+generation"

	// internal timing state — mutated only by tick, which is called serially.
	needsAttentionSince map[string]time.Time                     // "pid/jobID" → first seen in needs_attention
	pipelineJobStates   map[string]map[string]pipeline.JobStatus // pid → {jobID → status}
	pipelineLastChange  map[string]time.Time                     // pid → last time any job's status changed
}

// NewPipelineWatcher constructs a PipelineWatcher. stuckRetryAfter is how long a
// job must be in needs_attention before it is auto-retried. stallAfter is how long
// a pipeline must have no state progress before a safety Reconcile is triggered.
func NewPipelineWatcher(
	pstore pwPipelineStore,
	sstore pwSessionStore,
	exec *Executor,
	stuckRetryAfter, stallAfter time.Duration,
	autoRetry bool,
) *PipelineWatcher {
	return &PipelineWatcher{
		pstore:              pstore,
		sstore:              sstore,
		exec:                exec,
		stuckRetryAfter:     stuckRetryAfter,
		stallAfter:          stallAfter,
		autoRetry:           autoRetry,
		needsAttentionSince: make(map[string]time.Time),
		pipelineJobStates:   make(map[string]map[string]pipeline.JobStatus),
		pipelineLastChange:  make(map[string]time.Time),
		consulted:           make(map[string]bool),
	}
}

// SetBrainConsult wires the brain consult feature into the watcher. maxCon caps
// the number of simultaneous brain consult agents (default 1). life is used for
// the nudge_agent action path. Call before Run; not concurrency-safe.
func (w *PipelineWatcher) SetBrainConsult(c brainconsult.Consultor, maxCon int, life Lifecycle) {
	if maxCon <= 0 {
		maxCon = 1
	}
	w.consultor = c
	w.consultSem = make(chan struct{}, maxCon)
	w.life = life
}

// Run ticks at interval until ctx is cancelled.
func (w *PipelineWatcher) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx, first)
			first = false
		}
	}
}

func (w *PipelineWatcher) tick(ctx context.Context, firstTick bool) {
	pipelines, err := w.pstore.List()
	if err != nil {
		slog.Warn("pipeline_watcher: list failed", "err", err)
		return
	}

	for _, p := range pipelines {
		if p.Status != pipeline.StatusRunning {
			continue
		}
		pid := p.ID

		// 1. Startup reconcile: on the first tick, reconcile every running pipeline
		// to catch jobs that were in-flight when the daemon last stopped.
		if firstTick {
			if err := w.exec.Reconcile(ctx, pid); err != nil {
				slog.Warn("pipeline_watcher: startup reconcile failed", "pipeline", pid, "err", err)
			}
		}

		// 2. Stuck job watchdog.
		for i := range p.Jobs {
			j := &p.Jobs[i]
			key := pid + "/" + j.ID
			if j.Status != pipeline.JobNeedsAttention {
				delete(w.needsAttentionSince, key)
				w.clearConsulted(pid, j.ID)
				continue
			}
			since, seen := w.needsAttentionSince[key]
			if !seen {
				// First observation: record when we first saw this job stuck.
				w.needsAttentionSince[key] = time.Now()
				continue
			}
			if time.Since(since) < w.stuckRetryAfter {
				continue
			}
			consultKey := fmt.Sprintf("%s/%s+%d", pid, j.ID, j.AutoRetryCount)
			if w.autoRetry && j.AutoRetryCount == 0 {
				// Deterministic one-shot retry — consult never fires before this.
				if rerr := w.exec.Retry(ctx, pid, j.ID); rerr != nil {
					slog.Warn("pipeline_watcher: auto-retry failed", "pipeline", pid, "job", j.ID, "err", rerr)
				} else {
					delete(w.needsAttentionSince, key)
				}
			} else if w.consultor != nil && !w.consulted[consultKey] {
				// Brain consult fires after the deterministic retry has already been
				// attempted (AutoRetryCount >= 1) and the job is still stuck. Dedupe
				// prevents a second consult for the same stuck episode.
				w.consulted[consultKey] = true
				jCopy := *j
				go w.consultBrain(ctx, pid, jCopy)
			} else {
				slog.Warn("pipeline_watcher: job needs attention (no auto-retry)",
					"pipeline", pid, "job", j.ID,
					"auto_retry_count", j.AutoRetryCount,
					"stuck_for", time.Since(since).Round(time.Second))
			}
		}

		// 3. Stall detection: if no job status has changed since the last tick
		// for >= stallAfter, kick a safety Reconcile.
		current := snapshotJobStatuses(p)
		prev, hasPrev := w.pipelineJobStates[pid]
		if !hasPrev || !jobStatusMapsEqual(prev, current) {
			w.pipelineJobStates[pid] = current
			w.pipelineLastChange[pid] = time.Now()
		} else if since, ok := w.pipelineLastChange[pid]; ok && time.Since(since) >= w.stallAfter {
			slog.Info("pipeline_watcher: stall detected, reconciling",
				"pipeline", pid, "stalled_for", time.Since(since).Round(time.Second))
			if rerr := w.exec.Reconcile(ctx, pid); rerr != nil {
				slog.Warn("pipeline_watcher: stall reconcile failed", "pipeline", pid, "err", rerr)
			}
			w.pipelineLastChange[pid] = time.Now()
		}

		// 4. Orphaned job cross-check: a job whose agent session no longer exists
		// in the session store is marked failed so the executor can skip its
		// descendants.
		for i := range p.Jobs {
			j := &p.Jobs[i]
			if j.Status == pipeline.JobDone || j.Status == pipeline.JobFailed || j.Status == pipeline.JobSkipped {
				continue
			}
			agentRef := j.AgentRef()
			if agentRef == "" {
				continue
			}
			if _, serr := w.sstore.Get(ctx, agentRef); serr == nil {
				continue // session still exists
			}
			jobID := j.ID
			_ = w.pstore.Update(pid, func(up *pipeline.Pipeline) {
				if uj := up.Job(jobID); uj != nil && uj.AgentRef() == agentRef {
					uj.Status = pipeline.JobFailed
				}
			})
			if rerr := w.exec.Reconcile(ctx, pid); rerr != nil {
				slog.Warn("pipeline_watcher: orphan reconcile failed", "pipeline", pid, "job", jobID, "err", rerr)
			}
		}
	}
}

// clearConsulted removes all consulted entries for the given job across all
// generations. This handles the case where a brain-fired retry incremented the
// generation counter before the job left needs_attention.
func (w *PipelineWatcher) clearConsulted(pid, jobID string) {
	prefix := pid + "/" + jobID + "+"
	for k := range w.consulted {
		if strings.HasPrefix(k, prefix) {
			delete(w.consulted, k)
		}
	}
}

// consultBrain runs asynchronously. It acquires the semaphore, calls the
// Consultor, and executes the returned action via the existing Executor rails.
func (w *PipelineWatcher) consultBrain(ctx context.Context, pid string, j pipeline.Job) {
	// Non-blocking semaphore acquire: skip if at max concurrent consults.
	select {
	case w.consultSem <- struct{}{}:
	default:
		slog.Info("pipeline_watcher: brain consult semaphore full, skipping",
			"pipeline", pid, "job", j.ID)
		return
	}
	defer func() { <-w.consultSem }()

	// Capture the agent's pane excerpt for the Evidence field.
	var paneExcerpt string
	if agentID := j.AgentRef(); agentID != "" && w.life != nil {
		if sess, err := w.sstore.Get(ctx, agentID); err == nil {
			if out, err := w.life.Output(ctx, sess.TmuxSession, 50); err == nil {
				paneExcerpt = out
			}
		}
	}

	alreadyTried := []string{}
	if j.AutoRetryCount > 0 {
		alreadyTried = []string{string(brainconsult.ActionRetryJob)}
	}

	req := brainconsult.Request{
		Intent: "stuck pipeline job",
		Situation: fmt.Sprintf("pipeline %s job %s has been in needs_attention after %d auto-retries",
			pid, j.ID, j.AutoRetryCount),
		Goal:         "determine the best action to unblock the pipeline job",
		AlreadyTried: alreadyTried,
		Evidence:     paneExcerpt,
		PipelineID:   pid,
		JobID:        j.ID,
	}

	result, err := w.consultor.Consult(ctx, req)
	if err != nil {
		slog.Warn("pipeline_watcher: brain consult failed",
			"pipeline", pid, "job", j.ID, "err", err)
		return
	}

	slog.Info("pipeline_watcher: brain consult result",
		"pipeline", pid, "job", j.ID,
		"action", result.Action, "reason", result.Reason, "brain_id", result.BrainID)

	w.executeConsultResult(ctx, pid, j, result)
}

// executeConsultResult maps a brainconsult.Result onto the existing Executor /
// send / notify rails. No new merge paths are introduced.
func (w *PipelineWatcher) executeConsultResult(ctx context.Context, pid string, j pipeline.Job, result brainconsult.Result) {
	switch result.Action {
	case brainconsult.ActionRetryJob:
		if rerr := w.exec.Retry(ctx, pid, j.ID); rerr != nil {
			slog.Warn("pipeline_watcher: brain retry_job failed",
				"pipeline", pid, "job", j.ID, "err", rerr)
		}

	case brainconsult.ActionMarkFailed:
		w.exec.markJob(pid, j.ID, func(jj *pipeline.Job) { jj.Status = pipeline.JobFailed })
		if rerr := w.exec.Reconcile(ctx, pid); rerr != nil {
			slog.Warn("pipeline_watcher: brain mark_failed reconcile failed",
				"pipeline", pid, "job", j.ID, "err", rerr)
		}

	case brainconsult.ActionSkipJob:
		w.exec.markJob(pid, j.ID, func(jj *pipeline.Job) { jj.Status = pipeline.JobSkipped })
		if rerr := w.exec.Reconcile(ctx, pid); rerr != nil {
			slog.Warn("pipeline_watcher: brain skip_job reconcile failed",
				"pipeline", pid, "job", j.ID, "err", rerr)
		}

	case brainconsult.ActionNudgeAgent:
		if agentID := j.AgentRef(); agentID != "" && w.life != nil {
			if sess, err := w.sstore.Get(ctx, agentID); err == nil {
				if ierr := w.life.Input(ctx, sess.TmuxSession, "continue"); ierr != nil {
					slog.Warn("pipeline_watcher: brain nudge_agent input failed",
						"pipeline", pid, "job", j.ID, "err", ierr)
				}
			}
		}

	case brainconsult.ActionEscalate:
		slog.Warn("pipeline_watcher: brain escalated stuck job — operator action required",
			"pipeline", pid, "job", j.ID,
			"reason", result.Reason, "brain_id", result.BrainID)

	case brainconsult.ActionWait, brainconsult.ActionNoop:
		// no-op: re-evaluate on the next watcher cycle.

	default:
		slog.Warn("pipeline_watcher: unrecognised brain action, treating as noop",
			"pipeline", pid, "job", j.ID, "action", result.Action)
	}
}

func snapshotJobStatuses(p *pipeline.Pipeline) map[string]pipeline.JobStatus {
	m := make(map[string]pipeline.JobStatus, len(p.Jobs))
	for _, j := range p.Jobs {
		m[j.ID] = j.Status
	}
	return m
}

func jobStatusMapsEqual(a, b map[string]pipeline.JobStatus) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
