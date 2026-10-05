package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

// ErrPipelineRestartRefused marks a pipeline that is healthy and active; the
// caller may retry with force.
var ErrPipelineRestartRefused = errors.New("executor is healthy and active; pass --force to restart")

var errPipelineComplete = errors.New("complete pipeline is terminal")

// pipelineRestartInput configures RestartPipeline.
type pipelineRestartInput struct {
	Force      bool
	Store      autopilot.CtxStore // nil ⇒ context is not persisted (still injected)
	CtxKey     string
	ReasonKind string // "" ⇒ derived from pipeline state
	Reason     string
}

// pipelineRestartResult reports what a RestartPipeline reset.
type pipelineRestartResult struct {
	ResetJobs       []string
	KeptJobs        []string // jobs left in done
	AgentsRemoved   int
	BranchesKept    []string
	BranchesDeleted []string
	ReasonKind      string
	RestartCount    int
}

const restartPromptHeading = "## Restart context"

// RestartPipeline is the restart-only counterpart of Resume/Retry (their
// semantics are untouched): it tears down every live job agent, keeps done jobs
// and their handoffs, resets everything else to pending with its agent binding
// cleared, records kept branches, attaches the restart context to each reset
// job's prompt and reopens the pipeline (canceled → running). It does NOT
// reconcile: the caller syncs plan progress first, then calls Reconcile, so a
// freshly spawned job's in_progress mark is not overwritten.
func (e *Executor) RestartPipeline(ctx context.Context, pid string, in pipelineRestartInput) (pipelineRestartResult, error) {
	var res pipelineRestartResult
	e.mu.Lock()
	defer e.mu.Unlock()
	p, err := e.pstore.Get(pid)
	if err != nil {
		return res, err
	}
	if p.Status == pipeline.StatusDone {
		return res, errPipelineComplete
	}
	sessions, err := e.sstore.List(ctx)
	if err != nil {
		return res, err
	}
	byJob := map[string]*agentstore.Agent{} // non-done job → its live agent record
	var orphans []*agentstore.Agent         // pipeline agents with no matching job
	for _, sess := range sessions {
		if sess == nil || sess.PipelineID != pid {
			continue
		}
		j := p.Job(sess.JobID)
		switch {
		case j == nil:
			orphans = append(orphans, sess)
		case j.Status == pipeline.JobDone:
			// keepDone agent of a finished job: leave it be.
		default:
			byJob[j.ID] = sess
		}
	}
	if !in.Force && restartNeedsForce(p, byJob) {
		return res, ErrPipelineRestartRefused
	}

	kind, reason := in.ReasonKind, in.Reason
	if kind == "" {
		kind, reason = pipelineRestartReason(p, in.Force)
	}
	prober := restartGitProber{repo: p.Repo}

	rc := &autopilot.RestartContext{
		RestartedAt: time.Now().UTC(), Reason: reason, ReasonKind: kind,
		FinishedTasks: []autopilot.RestartFinishedTask{}, Unfinished: []autopilot.RestartUnfinishedTask{},
		Journal: []autopilot.JournalEntry{},
	}
	kept := map[string]string{} // job id → branch to continue from
	// Probe + tear down agents first (git facts need the branch, which survives).
	for i := range p.Jobs {
		j := &p.Jobs[i]
		if j.Status == pipeline.JobDone {
			rc.FinishedTasks = append(rc.FinishedTasks, autopilot.RestartFinishedTask{ID: j.ID, Branch: j.Branch})
			res.KeptJobs = append(res.KeptJobs, j.ID)
			continue
		}
		_, base := e.resolveWorktree(p, j)
		if base == "" {
			base = "HEAD"
		}
		u := autopilot.RestartUnfinishedTask{ID: j.ID}
		branch := j.RestartBranch
		sess := byJob[j.ID]
		if sess != nil && sess.Worktree != "" && sess.Branch != "" {
			branch = sess.Branch
		}
		if branch != "" {
			if _, verr := gitOut(ctx, p.Repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); verr != nil {
				branch = "" // never created (or already gone): nothing to keep
			}
		}
		if branch != "" {
			facts, ferr := prober.BranchFacts(ctx, branch, base)
			if ferr != nil || facts.HasUnmergedCommits { // unknown ⇒ keep
				kept[j.ID] = branch
				u.PreviousBranch, u.HasUnmergedCommits = branch, ferr != nil || facts.HasUnmergedCommits
				u.OpenPR, u.PRBase = facts.OpenPR, facts.PRBase
			}
		}
		rc.Unfinished = append(rc.Unfinished, u)
		if sess != nil {
			if terr := e.teardownRestartAgent(ctx, sess, kept[j.ID] != "", &res); terr != nil {
				return res, fmt.Errorf("tear down %s: %w", sess.ID, terr)
			}
		}
	}
	for _, sess := range orphans {
		if terr := e.teardownRestartAgent(ctx, sess, false, &res); terr != nil {
			return res, fmt.Errorf("tear down %s: %w", sess.ID, terr)
		}
	}

	if in.Store != nil {
		if prev, lerr := autopilot.LoadRestartContext(in.Store, in.CtxKey); lerr == nil && prev != nil {
			rc.RestartCount = prev.RestartCount
		}
	}
	rc.RestartCount++
	res.RestartCount, res.ReasonKind = rc.RestartCount, kind
	if in.Store != nil {
		if perr := autopilot.PersistRestartContext(in.Store, in.CtxKey, rc); perr != nil {
			return res, fmt.Errorf("restart context: persist: %w", perr)
		}
	}

	if err := e.pstore.Update(pid, func(up *pipeline.Pipeline) {
		for i := range up.Jobs {
			j := &up.Jobs[i]
			if j.Status == pipeline.JobDone {
				continue
			}
			j.Status = pipeline.JobPending
			j.SetAgentID("")
			j.Output = ""
			j.Branch = ""
			j.RestartBranch = kept[j.ID]
			j.Digest = nil
			j.Prompt = withRestartSection(j.Prompt, rc, j.ID)
			res.ResetJobs = append(res.ResetJobs, j.ID)
		}
		up.Status = pipeline.StatusRunning // ReopenForRestart: canceled/stalled/paused → running
	}); err != nil {
		return res, err
	}
	if e.notify != nil {
		e.notify()
	}
	return res, nil
}

// restartNeedsForce implements spec OQ-1: no force when no job agent is
// working/spawning AND the pipeline is canceled/stalled or every non-done job is
// failed/needs_attention/skipped.
func restartNeedsForce(p *pipeline.Pipeline, live map[string]*agentstore.Agent) bool {
	for jobID, s := range live {
		// A canceled/skipped job's leftover record is not a live worker.
		if j := p.Job(jobID); j == nil || (j.Status != pipeline.JobRunning && j.Status != pipeline.JobNeedsAttention) {
			continue
		}
		if s.Status == store.StatusWorking || s.Status == store.StatusSpawning {
			return true
		}
	}
	if p.Status == pipeline.StatusCanceled || p.Status == pipeline.StatusStalled {
		return false
	}
	for i := range p.Jobs {
		switch p.Jobs[i].Status {
		case pipeline.JobDone, pipeline.JobFailed, pipeline.JobNeedsAttention, pipeline.JobSkipped:
		default:
			return true
		}
	}
	return false
}

func pipelineRestartReason(p *pipeline.Pipeline, force bool) (kind, reason string) {
	if force {
		return autopilot.RestartReasonOperatorForce, "operator forced a restart of an active pipeline"
	}
	var bad []string
	for i := range p.Jobs {
		if s := p.Jobs[i].Status; s == pipeline.JobFailed || s == pipeline.JobNeedsAttention {
			bad = append(bad, p.Jobs[i].ID+" ("+string(s)+")")
		}
	}
	switch {
	case p.Status == pipeline.StatusCanceled:
		return autopilot.RestartReasonOperatorStop, "pipeline was stopped"
	case len(bad) > 0:
		return autopilot.RestartReasonNeedsAttention, "jobs needing attention: " + strings.Join(bad, ", ")
	}
	return autopilot.RestartReasonUnknown, ""
}

// teardownRestartAgent terminates a job agent, removes its worktree and drops
// its record (freeing the <pid>-<job> id). A branch with commits is kept; an
// empty one is deleted so the respawn's `-b` works.
func (e *Executor) teardownRestartAgent(ctx context.Context, sess *agentstore.Agent, keepBranch bool, res *pipelineRestartResult) error {
	if sess.TmuxSession != "" {
		_ = e.life.Terminate(ctx, sess.TmuxSession)
	}
	if sess.Worktree != "" {
		wt := *sess
		wt.BranchCreated = false // RemoveWorktree must not -D; the policy below decides
		if err := e.life.RemoveWorktree(ctx, &wt, true, false); err != nil {
			return err
		}
		if sess.Branch != "" {
			if keepBranch {
				res.BranchesKept = append(res.BranchesKept, sess.Branch)
			} else if sess.BranchCreated {
				if out, derr := gitOut(ctx, sess.Repo, "branch", "-D", sess.Branch); derr == nil || strings.Contains(out, "not found") {
					res.BranchesDeleted = append(res.BranchesDeleted, sess.Branch)
				}
			}
		}
	}
	if err := e.sstore.Delete(ctx, sess.ID); err != nil && !errors.Is(err, agentstore.ErrNotFound) {
		return err
	}
	e.removeJobProjectMembership(sess)
	res.AgentsRemoved++
	return nil
}

// withRestartSection replaces any earlier restart section in prompt with one
// rendered for jobID: the finished jobs plus only this job's unfinished entry.
func withRestartSection(prompt string, rc *autopilot.RestartContext, jobID string) string {
	if i := strings.Index(prompt, "\n\n"+restartPromptHeading); i >= 0 {
		prompt = prompt[:i]
	} else if strings.HasPrefix(prompt, restartPromptHeading) {
		prompt = ""
	}
	mine := *rc
	mine.Unfinished = nil
	for _, u := range rc.Unfinished {
		if u.ID == jobID {
			mine.Unfinished = append(mine.Unfinished, u)
		}
	}
	return autopilot.AppendRestartContext(prompt, autopilot.RenderRestartContext(&mine))
}
