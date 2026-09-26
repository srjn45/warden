package daemon

import (
	"context"
	"log/slog"
	"time"

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
	}
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
			if w.autoRetry && j.AutoRetryCount == 0 {
				if rerr := w.exec.Retry(ctx, pid, j.ID); rerr != nil {
					slog.Warn("pipeline_watcher: auto-retry failed", "pipeline", pid, "job", j.ID, "err", rerr)
				} else {
					delete(w.needsAttentionSince, key)
				}
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
