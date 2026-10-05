package autopilot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Fix loop (run-to-final-pr spec §B). The landing pass hands every red or
// conflicted run-owned PR here. At most one dispatch happens per red head SHA;
// a flaky/infrastructure failure is re-run once per SHA first (Fast-Brain
// classified, failing open to "real"); a real failure goes to the owning worker
// when its session is alive, else to a fix-up worker on the same branch.

// Fix-loop defaults (spec §G): generous, so normal work is never paced.
const (
	DefaultMaxRedSHAs    = 5                // consecutive red head SHAs before the resolver seam
	DefaultMaxFixes      = 10               // total worker fix dispatches per task
	DefaultMaxReruns     = 3                // total CI re-runs per task (and one per SHA)
	DefaultFixBusyDefer  = 10 * time.Minute // wait for a busy live owner before waking it anyway
	fixFlakyMinConf      = 0.8              // model verdicts below this are treated as real
	fixEvidenceByteLimit = 8 << 10
)

// FixPolicy bounds the fix loop. Zero fields take the defaults above.
type FixPolicy struct {
	MaxRedSHAs int
	MaxFixes   int
	MaxReruns  int
	BusyDefer  time.Duration
}

func (p FixPolicy) withDefaults() FixPolicy {
	if p.MaxRedSHAs <= 0 {
		p.MaxRedSHAs = DefaultMaxRedSHAs
	}
	if p.MaxFixes <= 0 {
		p.MaxFixes = DefaultMaxFixes
	}
	if p.MaxReruns <= 0 {
		p.MaxReruns = DefaultMaxReruns
	}
	if p.BusyDefer <= 0 {
		p.BusyDefer = DefaultFixBusyDefer
	}
	return p
}

// SetFixPolicy overrides the fix-loop bounds (zero fields keep the defaults).
func (c *Controller) SetFixPolicy(p FixPolicy) {
	c.mu.Lock()
	c.fixPolicy = p
	c.mu.Unlock()
}

// FixEvidence is the failure evidence collected for one red head SHA.
type FixEvidence struct {
	Jobs   []string // failing workflow/job names
	RunIDs []string // failing CI run ids (for `gh run rerun --failed`)
	Log    string   // trimmed, sanitized failed-log excerpt
}

// FixLiveness is the owning worker's dispatchability.
type FixLiveness int

const (
	FixWorkerGone FixLiveness = iota // no live session: spawn a fix-up worker
	FixWorkerBusy                    // alive and working: defer
	FixWorkerIdle                    // alive and awaiting input: send the fix
)

// FixSpawn describes a fix-up worker spawned on the PR branch.
type FixSpawn struct {
	RunID    string
	Repo     string
	TaskID   string
	Branch   string
	Worktree string // the dead owner's worktree to take over; "" = make one
	Prompt   string
}

// FixRuntime is the optional daemon seam for the fix loop. A runtime without it
// leaves the landing pass on LandingRuntime.DispatchFix.
type FixRuntime interface {
	// CIEvidence collects failing job names, run ids and a trimmed log excerpt for
	// the head SHA. An empty result means "no CI evidence" (e.g. a local gate).
	CIEvidence(ctx context.Context, repo, dir, branch, headSHA string) FixEvidence
	// ClassifyCI asks Fast-Brain whether a failure is flaky/infrastructure.
	// It must fail open to flaky=false.
	ClassifyCI(ctx context.Context, agentID, check, log string) (flaky bool, confidence float64, source string)
	// RerunFailed re-runs the failed jobs of the given CI runs (gh run rerun --failed).
	RerunFailed(ctx context.Context, repo, dir string, runIDs []string) error
	// WorkerLiveness reports whether the owning worker session can take the fix.
	WorkerLiveness(ctx context.Context, workerID string) FixLiveness
	// SendFix delivers the fix instruction to a live worker as an input turn.
	SendFix(ctx context.Context, workerID, msg string) error
	// SpawnFixWorker spawns a fix-up worker on the PR branch and returns its id.
	SpawnFixWorker(ctx context.Context, spec FixSpawn) (string, error)
	// AuditRunEvent records the event in the audit log.
	AuditRunEvent(ctx context.Context, runID, action, agentID, detail string)
}

// RedGateResolver is the seam t7-resolver fills: called once per red head SHA
// when a task's consecutive-red cap is reached. Returning true means the
// resolver took the blocker (worker dispatch pauses for that SHA); false (or an
// absent implementation) continues the loop — there is no park.
type RedGateResolver interface {
	ResolveRedGate(ctx context.Context, runID, taskID string, st FixState) bool
}

func (c *Controller) policy() FixPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fixPolicy.withDefaults()
}

// runFixLoop handles one red or conflicted PR. Never merges.
func (c *Controller) runFixLoop(ctx context.Context, fr FixRuntime, ledger *Ledger, s landSnapshot, fix LandFix) {
	if ledger == nil || fix.TaskID == "" {
		return
	}
	pol := c.policy()
	st, err := ledger.FixState(fix.TaskID)
	if err != nil {
		return
	}
	sha := fix.PR.HeadSHA
	if st.LastDispatchedSHA == sha && sha != "" {
		return // one dispatch per red head SHA
	}
	now := c.now()
	if st.HeadSHA != sha {
		// A new red/conflicting head: the streak counts consecutive red SHAs.
		st.RedStreak++
		st.HeadSHA, st.Evidence, st.BusySince = sha, "", ""
	}
	st.PR, st.Branch, st.Kind = fix.PR.Number, fix.PR.HeadRef, string(fix.Kind)
	save := func() {
		st.UpdatedAt = now.UTC().Format(time.RFC3339)
		_ = ledger.WriteFixState(st, "daemon-fix")
	}
	dir := firstNonEmpty(fix.Owner.Worktree, s.repo)

	var ev FixEvidence
	if fix.Kind == FixRed {
		ev = fr.CIEvidence(ctx, s.repo, dir, fix.PR.HeadRef, sha)
	}
	if st.Evidence == "" {
		st.Evidence = composeFixEvidence(fix, ev)
	}

	// One CI re-run per head SHA for a flaky/infra failure (fail open: real).
	if fix.Kind == FixRed && len(ev.RunIDs) > 0 && st.RerunDoneForSHA != sha && st.Reruns < pol.MaxReruns {
		flaky, conf, src := fr.ClassifyCI(ctx, fix.TaskID, strings.Join(ev.Jobs, ", "), ev.Log)
		if flaky && (src != "model" || conf >= fixFlakyMinConf) {
			st.RerunDoneForSHA = sha
			save() // record before acting: at-most-once across a restart
			if err := fr.RerunFailed(ctx, s.repo, dir, ev.RunIDs); err == nil {
				st.Reruns++
				save()
				fr.AuditRunEvent(ctx, s.runID, "autopilot.ci_rerun", fix.Owner.WorkerID,
					fmt.Sprintf("task=%s pr=%d sha=%s runs=%s reruns=%d", fix.TaskID, fix.PR.Number, sha, strings.Join(ev.RunIDs, ","), st.Reruns))
				return
			}
			// rerun failed: fall through to a real dispatch
		}
	}

	if st.FixAttempts >= pol.MaxFixes {
		if st.CappedAuditedSHA != sha {
			st.CappedAuditedSHA = sha
			save()
			fr.AuditRunEvent(ctx, s.runID, "autopilot.ci_fix_capped", fix.Owner.WorkerID,
				fmt.Sprintf("task=%s pr=%d sha=%s attempts=%d", fix.TaskID, fix.PR.Number, sha, st.FixAttempts))
		}
		c.callRedResolver(ctx, fr, s, &st, sha, save)
		return
	}
	if st.RedStreak >= pol.MaxRedSHAs && c.callRedResolver(ctx, fr, s, &st, sha, save) {
		return
	}

	msg := composeFixMessage(fix, st.Evidence)
	// Owning worker alive → send it; defer while it is busy (bounded).
	if fix.Owner.WorkerID != "" {
		switch fr.WorkerLiveness(ctx, fix.Owner.WorkerID) {
		case FixWorkerBusy:
			if st.BusySince == "" {
				st.BusySince = now.UTC().Format(time.RFC3339)
				save()
				return
			}
			if t, err := time.Parse(time.RFC3339, st.BusySince); err == nil && now.Sub(t) < pol.BusyDefer {
				return
			}
			fallthrough // deferred long enough: wake the live owner anyway
		case FixWorkerIdle:
			st.LastDispatchedSHA, st.FixAttempts, st.Fixing, st.BusySince = sha, st.FixAttempts+1, true, ""
			st.FixerSession = fix.Owner.WorkerID
			save()
			if err := fr.SendFix(ctx, fix.Owner.WorkerID, msg); err != nil {
				return
			}
			fr.AuditRunEvent(ctx, s.runID, "autopilot.ci_fix_dispatched", fix.Owner.WorkerID,
				fmt.Sprintf("task=%s pr=%d sha=%s kind=%s to=owner attempt=%d streak=%d", fix.TaskID, fix.PR.Number, sha, fix.Kind, st.FixAttempts, st.RedStreak))
			return
		}
	}
	// Owner gone: fix-up worker on the same branch (and worktree when it survives).
	st.LastDispatchedSHA, st.FixAttempts, st.Fixing, st.BusySince = sha, st.FixAttempts+1, true, ""
	save()
	id, err := fr.SpawnFixWorker(ctx, FixSpawn{
		RunID: s.runID, Repo: s.repo, TaskID: fix.TaskID, Branch: fix.PR.HeadRef,
		Worktree: fix.Owner.Worktree, Prompt: msg,
	})
	if err != nil {
		// Spawn failed: free the SHA so the next tick retries (attempt still counted).
		st.LastDispatchedSHA, st.Fixing = "", false
		save()
		fr.AuditRunEvent(ctx, s.runID, "autopilot.ci_fix_spawn_failed", "", fmt.Sprintf("task=%s pr=%d sha=%s err=%v", fix.TaskID, fix.PR.Number, sha, err))
		return
	}
	st.FixerSession = id
	save()
	fr.AuditRunEvent(ctx, s.runID, "autopilot.ci_fix_dispatched", id,
		fmt.Sprintf("task=%s pr=%d sha=%s kind=%s to=fixup attempt=%d streak=%d", fix.TaskID, fix.PR.Number, sha, fix.Kind, st.FixAttempts, st.RedStreak))
}

// callRedResolver offers the blocker to the resolver seam once per head SHA.
func (c *Controller) callRedResolver(ctx context.Context, fr FixRuntime, s landSnapshot, st *FixState, sha string, save func()) bool {
	rr, ok := fr.(RedGateResolver)
	if !ok || st.ResolverCalledSHA == sha {
		return false
	}
	st.ResolverCalledSHA = sha
	save()
	return rr.ResolveRedGate(ctx, s.runID, st.Task, *st)
}

func composeFixEvidence(fix LandFix, ev FixEvidence) string {
	var b strings.Builder
	switch {
	case fix.Kind == FixConflict:
		b.WriteString("The PR conflicts with its base branch.\n")
	case len(ev.Jobs) > 0:
		b.WriteString("Failing CI jobs: " + strings.Join(ev.Jobs, ", ") + "\n")
	}
	if fix.Detail != "" {
		b.WriteString(fix.Detail + "\n")
	}
	if ev.Log != "" {
		b.WriteString("\nFailed-log excerpt:\n" + ev.Log + "\n")
	}
	out := b.String()
	if len(out) > fixEvidenceByteLimit {
		out = out[:fixEvidenceByteLimit] + "\n…(truncated)"
	}
	return out
}

func composeFixMessage(fix LandFix, evidence string) string {
	pr := strconv.Itoa(fix.PR.Number)
	var instr string
	if fix.Kind == FixConflict {
		instr = "Merge or rebase the PR's base branch into your branch, resolve the conflicts preserving both sides' intent, re-run `wd check`, and push."
	} else {
		instr = "Fix the failure on this branch, run `wd check`, and push."
	}
	return fmt.Sprintf("The PR #%s (branch %s, head %s) is not mergeable.\n\n%s\n%s\nWhen it is pushed and locally green, end with `wd job done`. Do NOT merge the PR — autopilot lands it.\n",
		pr, fix.PR.HeadRef, short(fix.PR.HeadSHA), evidence, instr)
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
