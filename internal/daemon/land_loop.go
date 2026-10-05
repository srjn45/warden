package daemon

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
)

// autopilot.LandingRuntime — the daemon seam for the landing loop
// (run-to-final-pr spec §A).

// LandingHost returns the gh/git host rooted at repo (a test fake via landHostFn
// when it also lists PRs).
func (rt autopilotRuntime) LandingHost(repo string) autopilot.LandingHost {
	if rt.s.landHostFn != nil {
		if lh, ok := rt.s.landHostFn(repo).(autopilot.LandingHost); ok {
			return lh
		}
		return nil
	}
	return daemonLandHost{s: rt.s, dir: repo}
}

// ResolveLandOwner maps a PR head to a run-owned task (spec §A.2 rules 1–2):
// an owning session of the run by branch, else a ledger task row by branch.
func (rt autopilotRuntime) ResolveLandOwner(ctx context.Context, runID, headRef string) (autopilot.LandOwner, bool) {
	if headRef == "" {
		return autopilot.LandOwner{}, false
	}
	if sessions, err := rt.s.store.List(ctx); err == nil {
		for _, sess := range sessions {
			if sess.Branch != headRef {
				continue
			}
			if owned, id := ownershipFromTags(sess.Tags); owned && id == runID {
				t := sessionLandTarget(sess, headRef)
				return autopilot.LandOwner{TaskID: t.taskID, WorkerID: sess.ID, Worktree: sess.Worktree}, true
			}
		}
	}
	if l := rt.NewLedger(runID); l != nil {
		if tasks, err := l.Tasks(); err == nil {
			for _, t := range tasks {
				if t.Branch == headRef {
					o := autopilot.LandOwner{TaskID: t.ID}
					if _, err := rt.s.store.Get(ctx, t.WorkerID); err == nil && t.WorkerID != "" {
						o.WorkerID = t.WorkerID
					}
					return o, true
				}
			}
		}
	}
	return autopilot.LandOwner{}, false
}

// FinalizeLanding runs the post-merge bookkeeping for a loop-landed PR. Every
// step is idempotent, so it also heals an AlreadyLanded result.
func (rt autopilotRuntime) FinalizeLanding(ctx context.Context, runID string, owner autopilot.LandOwner, res autopilot.LandResult) {
	s := rt.s
	params, _ := s.autopilot.LandParams(runID)
	ledger := rt.NewLedger(runID)
	tgt := landTarget{branch: res.Branch, runID: runID, taskID: owner.TaskID, agentID: owner.WorkerID, owned: true}
	if owner.WorkerID != "" {
		if sess, err := s.store.Get(ctx, owner.WorkerID); err == nil {
			tgt.sess, tgt.worktree, tgt.planID = sess, sess.Worktree, strings.TrimSpace(sess.PlanID)
		}
	}
	s.recordLanding(ctx, ledger, tgt, res, params.DeleteBranch)
	if res.AlreadyLanded && s.autopilot != nil && res.PR > 0 && owner.TaskID != "" {
		_, _ = s.autopilot.UpdateTaskStatus(runID, owner.TaskID, autopilot.TaskStatusDone, res.PR)
	}
	if ledger != nil && owner.TaskID != "" {
		if err := ledger.WriteTaskState(owner.TaskID, autopilot.LedgerLanded, "daemon-landing"); err != nil {
			slog.Warn("autopilot landing: write ledger state", "run", runID, "task", owner.TaskID, "err", err)
		}
	}
	if ledger != nil && owner.TaskID != "" {
		_ = ledger.ClearFixState(owner.TaskID, "daemon-landing") // landed: the red streak resets
	}
	rt.teardownLandedWorker(ctx, tgt.sess)
	s.recordAuditCtx(ctx, audit.ActionAutopilotAutoLand, runID, map[string]string{
		"branch": res.Branch, "pr": strconv.Itoa(res.PR), "sha": res.HeadSHA, "task_id": owner.TaskID,
		"already_landed": boolStr(res.AlreadyLanded),
	})
}

// teardownLandedWorker stops the worker through the existing stop path
// (terminate → remove worktree, order per contributor-agent teardown). The
// branch is not deleted here: that stays with `land` (delete_branch).
func (rt autopilotRuntime) teardownLandedWorker(ctx context.Context, sess *agentstore.Agent) {
	s := rt.s
	if sess == nil || s.life == nil {
		return
	}
	if _, err := s.store.Get(ctx, sess.ID); errors.Is(err, agentstore.ErrNotFound) {
		return
	}
	if liveStatus(sess.Status) {
		if err := s.life.Terminate(ctx, sess.TmuxSession); err != nil {
			slog.Debug("autopilot landing: terminate worker", "agent", sess.ID, "err", err)
		}
		_ = s.store.UpdateStatus(ctx, sess.ID, store.StatusDone)
	}
	if sess.Worktree != "" {
		if err := s.life.RemoveWorktree(ctx, sess, true, false); err != nil {
			slog.Warn("autopilot landing: remove worker worktree", "agent", sess.ID, "err", err)
		}
	}
	s.notify()
}

// DispatchFix is the fallback hook; the Controller routes red/conflicted PRs to
// the fix loop (FixRuntime, fix_runtime.go) instead. Never merges.
func (rt autopilotRuntime) DispatchFix(ctx context.Context, runID string, fix autopilot.LandFix) {
	slog.Debug("autopilot landing: PR needs a fix", "run", runID, "pr", fix.PR.Number, "kind", fix.Kind)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
