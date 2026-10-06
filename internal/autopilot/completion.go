package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Completion and the single final PR (run-to-final-pr spec §E). Once every plan
// task has landed and no run-owned PR is open, the daemon brings the integration
// branch current with the default branch, asks the manager to verify done_when,
// opens ONE PR (integration → default) with a generated body, gates it like any
// PR and — only when it is green — completes the run. Autopilot never merges,
// approves or closes the final PR (spec §E.6): that is the owner's act.
//
// A run being "finalizing" is a sub-phase of active: the internal state stays
// StateActive (so every pause/stop/guardian path keeps working) and the phase is
// derived from the ledger + plan each tick, so a daemon restart simply resumes.

// StateFinalizing is how a run in its completion phase is reported in status.
const StateFinalizing RunState = "finalizing"

// KindFinalPRUnfixable: the final PR stayed red past the fix bound.
const KindFinalPRUnfixable FailureKind = "final_pr_unfixable"

// KindFinalPRClosed: the final PR was closed without merging.
const KindFinalPRClosed FailureKind = "final_pr_closed"

// StateAwaitingMerge is how a run whose final PR is green and waiting for the
// owner to merge it is reported (plan-finish-flow §1). Like finalizing it is a
// derived sub-phase of active: pause, stop and the kill switch work unchanged.
const StateAwaitingMerge RunState = "awaiting_merge"

// Merge-poll interval bounds (plan-finish-flow §2).
const (
	DefaultMergePollInterval = 2 * time.Minute
	MinMergePollInterval     = 30 * time.Second
)

// AwaitingMerge is the persisted "waiting for the final PR to be merged" record.
type AwaitingMerge struct {
	Since    string `json:"since"`
	GreenSHA string `json:"green_sha,omitempty"`
	Notified bool   `json:"notified"`
	PR       int    `json:"pr,omitempty"`

	// In-memory only: the stale-result generation and the next poll instant.
	gen        int
	nextPollAt time.Time
}

// Final-PR gate values reported in RunStatus.final_pr.gate.
const (
	FinalGatePending = "pending"
	FinalGateRed     = "red"
	FinalGateGreen   = "green"
)

// DefaultManagerVerifyTimeout bounds how long the manager has to verify
// done_when before the resolver is called (spec §E.3).
const DefaultManagerVerifyTimeout = 30 * time.Minute

// DefaultMaxFinalFixes bounds the red final-PR head SHAs fixed before parking.
const DefaultMaxFinalFixes = DefaultMaxRedSHAs

// FinalPR is the single final PR as reported in run status.
type FinalPR struct {
	Number      int    `json:"number"`
	URL         string `json:"url,omitempty"`
	HeadSHA     string `json:"head_sha,omitempty"`
	Gate        string `json:"gate"` // pending | red | green
	FixAttempts int    `json:"fix_attempts"`
	// State is the PR's host state once polled: open | merged | closed.
	State    string `json:"state,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
}

// CompletionPolicy bounds the completion phase. Zero fields take the defaults.
type CompletionPolicy struct {
	// SkipMergeDefault disables bringing integration current with the default
	// branch (completion.merge_default=false); a stale base is then reported on
	// the final PR body instead.
	SkipMergeDefault     bool
	ManagerVerifyTimeout time.Duration
	MaxFinalFixes        int
	// MergePollInterval is how often the final PR is polled while awaiting its
	// merge; below MinMergePollInterval it is raised to it.
	MergePollInterval time.Duration
}

func (p CompletionPolicy) withDefaults() CompletionPolicy {
	if p.ManagerVerifyTimeout <= 0 {
		p.ManagerVerifyTimeout = DefaultManagerVerifyTimeout
	}
	if p.MaxFinalFixes <= 0 {
		p.MaxFinalFixes = DefaultMaxFinalFixes
	}
	if p.MergePollInterval <= 0 {
		p.MergePollInterval = DefaultMergePollInterval
	}
	if p.MergePollInterval < MinMergePollInterval {
		p.MergePollInterval = MinMergePollInterval
	}
	return p
}

// SetCompletionPolicy overrides the completion bounds.
func (c *Controller) SetCompletionPolicy(p CompletionPolicy) {
	c.mu.Lock()
	c.completionPolicy = p
	c.mu.Unlock()
}

// MergeDefaultResult is the outcome of merging the default branch into integration.
type MergeDefaultResult string

const (
	MergeUpToDate MergeDefaultResult = "up_to_date"
	MergeMerged   MergeDefaultResult = "merged"
	MergeConflict MergeDefaultResult = "conflict"
)

// FinalPRSpec is everything the host needs to open (or adopt) the final PR.
type FinalPRSpec struct {
	Integration   string
	DefaultBranch string
	// Title is the fallback/legacy title; a host that creates the PR may
	// replace it with FinalPRTitle(Name, RunID, <integration commits>).
	Title string
	Body  string
	// Name and RunID feed FinalPRTitle.
	Name, RunID string
}

// FinalPRState is a final PR's host-side state.
type FinalPRState struct {
	State   string // open | merged | closed
	HeadSHA string
	Gate    GateState
	Detail  string // failing check summary when red
	// MergedAt, Mergeable and MergeStateStatus come straight from gh pr view
	// (MERGED covers merge, squash and rebase alike).
	MergedAt         string
	Mergeable        string // MERGEABLE | CONFLICTING | UNKNOWN
	MergeStateStatus string // CLEAN | BEHIND | DIRTY | BLOCKED | ...
}

// needsBaseMerge reports a PR that conflicts with, or (under up-to-date branch
// protection) is behind, its base.
func (s FinalPRState) needsBaseMerge() bool {
	return strings.EqualFold(s.Mergeable, "CONFLICTING") ||
		strings.EqualFold(s.MergeStateStatus, "DIRTY") ||
		strings.EqualFold(s.MergeStateStatus, "BEHIND")
}

// ErrNothingToMerge: integration has no commits beyond the default branch, so
// there is nothing to open a PR for.
var ErrNothingToMerge = errors.New("autopilot completion: integration has no commits ahead of the default branch")

// CompletionRuntime is the optional daemon seam for the completion phase. A
// runtime without it leaves CompleteRun's legacy immediate-completion semantics.
type CompletionRuntime interface {
	// MergeDefault merges origin/<default> into integration in a throwaway
	// worktree and pushes integration. A conflict leaves nothing changed.
	MergeDefault(ctx context.Context, repo, integration, defaultBranch string) (MergeDefaultResult, error)
	// EnsureFinalPR opens (or adopts and refreshes the body of) the one PR
	// integration → default. ErrNothingToMerge when there is nothing to open.
	EnsureFinalPR(ctx context.Context, repo string, spec FinalPRSpec) (FinalPR, error)
	// FinalPRStatus reports the PR's state and its gate on the integration head.
	FinalPRStatus(ctx context.Context, repo, gate string, pr FinalPR, integration string) (FinalPRState, error)
	// FinalPRView reports only the PR's host state (one gh pr view, no gate):
	// the cheap poll used while awaiting the merge.
	FinalPRView(ctx context.Context, repo string, pr FinalPR) (FinalPRState, error)
}

// RunAgentReaper is the optional seam that terminates every agent still tagged
// to a run (strays, resolvers) and removes their worktrees. Used on entering
// awaiting_merge so no agent stays alive only to wait.
type RunAgentReaper interface {
	TerminateRunAgents(ctx context.Context, runID string) error
}

// completionState is the in-memory completion bookkeeping of one run. Nothing
// here needs persisting: every field is re-derived idempotently after a restart
// (the final PR is adopted, an unverified plan re-asks the manager).
type completionState struct {
	finalizing    bool
	verified      bool // manager confirmed done_when, or the verify window lapsed
	lapsed        bool // verified only because the window lapsed (body says so)
	verifyAskedAt time.Time
	finalPR       *FinalPR
	lastRedSHA    string
	baseMergeDone bool
	// resolverUntil holds off re-merging while a base_merge resolver works.
	resolverUntil time.Time
	// awaiting is set while the green final PR waits for its merge (hydrated from
	// the persisted surface record after a restart).
	awaiting *AwaitingMerge
	// awaitGen numbers awaiting episodes so a stale poll result is discarded.
	awaitGen int
}

// completionSnapshot is the slice of a run read under c.mu for the I/O pass.
type completionSnapshot struct {
	runID, repo, integration, defaultBranch, gate, name, goal string
	doneWhen                                                  []string
	tasks                                                     []PlanTask
	allDone                                                   bool
	cs                                                        completionState
	resolverAttempts                                          map[string]int
	awaiting                                                  *AwaitingMerge
	notified                                                  bool
}

// CompletionManaged reports whether the daemon owns completion (a runtime with
// the CompletionRuntime seam): the brain's completion signal then only confirms
// done_when and the run completes when the final PR is green.
func (c *Controller) CompletionManaged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.runtime.(CompletionRuntime)
	return ok
}

// MarkVerified records the manager's done_when verification for a managed run.
// Idempotent; it never completes the run itself.
func (c *Controller) MarkVerified(runID string) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return c.statusLocked(), fmt.Errorf("autopilot: unknown run %q", runID)
	}
	r.completion.verified = true
	return c.statusLocked(), nil
}

// completionTick advances every landable run's completion phase once. Like the
// landing pass it snapshots under c.mu and runs all gh/git I/O unlocked.
func (c *Controller) completionTick(ctx context.Context) {
	c.mu.Lock()
	cr, ok := c.runtime.(CompletionRuntime)
	lr, lok := c.runtime.(LandingRuntime)
	if !ok || !lok {
		c.mu.Unlock()
		return
	}
	var snaps []completionSnapshot
	for _, r := range c.runs {
		if r.state != StateActive || r.needsAttention != "" || r.integrationBranch == "" || r.defaultBranch == "" {
			continue
		}
		snaps = append(snaps, completionSnapshot{
			runID: r.runID, repo: r.repo, integration: r.integrationBranch,
			defaultBranch: r.defaultBranch, gate: c.runGate(r), name: firstNonEmpty(r.plan.Name, r.name),
			goal: r.plan.Goal, doneWhen: append([]string(nil), r.plan.DoneWhen...),
			tasks: append([]PlanTask(nil), r.plan.Tasks...), allDone: allTasksDone(r.plan.Tasks),
			cs: r.completion, resolverAttempts: copyAttempts(r.resolverAttempts),
		})
		last := &snaps[len(snaps)-1]
		c.surfaceLocked(r) // hydrate persisted awaiting/verified after a restart
		last.cs = r.completion
		if r.completion.finalPR != nil {
			fp := *r.completion.finalPR
			last.cs.finalPR = &fp
		}
		if aw := r.completion.awaiting; aw != nil {
			cp := *aw
			last.awaiting = &cp
			last.cs.awaiting = &cp
		}
		last.notified = c.surfaceLocked(r).FinalNotified
	}
	c.mu.Unlock()
	for _, s := range snaps {
		mu := c.landLock("complete:" + s.runID)
		if !mu.TryLock() {
			continue
		}
		func() {
			defer mu.Unlock()
			c.completeRunPass(ctx, cr, lr, s)
		}()
	}
}

func allTasksDone(tasks []PlanTask) bool {
	if len(tasks) == 0 {
		return false
	}
	for _, t := range tasks {
		if t.Status != TaskStatusDone {
			return false
		}
	}
	return true
}

// completeRunPass runs one completion step for one run.
func (c *Controller) completeRunPass(ctx context.Context, cr CompletionRuntime, lr LandingRuntime, s completionSnapshot) {
	if !s.allDone {
		if s.cs.finalizing || s.awaiting != nil {
			c.leaveAwaiting(ctx, s.runID, "tasks were appended to the plan") // back to active
			c.setFinalizing(s.runID, false)
		}
		return
	}
	if s.awaiting != nil {
		c.awaitMergePass(ctx, cr, s)
		return
	}
	// E.1: no open run-owned PR into integration (a final-fix PR counts).
	host := lr.LandingHost(s.repo)
	if host == nil {
		return
	}
	prs, err := host.ListOpenPRs(ctx, s.integration)
	if err != nil {
		return
	}
	for _, pr := range prs {
		if _, owned := lr.ResolveLandOwner(ctx, s.runID, pr.HeadRef); owned {
			return // landing / fixing still in flight
		}
	}
	if !s.cs.finalizing {
		c.setFinalizing(s.runID, true)
		c.auditRun(ctx, s.runID, "autopilot_finalizing", "integration="+s.integration)
	}
	pol := c.completionPolicyValue()

	// A recorded final PR is read BEFORE anything else (and before EnsureFinalPR,
	// which only finds open PRs): a PR merged while the daemon was down must not
	// get a second one opened (plan-finish-flow §2).
	if s.cs.finalPR != nil && s.cs.finalPR.Number > 0 {
		if v, err := cr.FinalPRView(ctx, s.repo, *s.cs.finalPR); err == nil {
			switch v.State {
			case "merged":
				c.finalPRMerged(ctx, s.runID, *s.cs.finalPR, v)
				return
			case "closed":
				c.finalPRClosed(ctx, s.runID, *s.cs.finalPR)
				return
			}
		}
	}

	// E.2: bring integration current with the default branch.
	if !pol.SkipMergeDefault && !s.cs.baseMergeDone {
		if c.now().Before(s.cs.resolverUntil) {
			return // a base_merge resolver is working
		}
		res, err := cr.MergeDefault(ctx, s.repo, s.integration, s.defaultBranch)
		if err != nil {
			c.auditRun(ctx, s.runID, "autopilot_base_merge_error", err.Error())
			return
		}
		if res == MergeConflict {
			c.callResolver(ctx, ResolverRequest{
				RunID: s.runID, Branch: s.integration, Class: BlockerBaseMerge,
				Detail: fmt.Sprintf("origin/%s has diverged from %s and merging it conflicts. In this worktree run `git fetch origin && git merge origin/%s`, resolve every conflict preserving both sides' intent, run `wd check`, commit and `git push origin HEAD:%s`.",
					s.defaultBranch, s.integration, s.defaultBranch, s.integration),
			})
			c.updateCompletion(s.runID, func(cs *completionState) { cs.resolverUntil = c.now().Add(resolverWait) })
			return
		}
		c.updateCompletion(s.runID, func(cs *completionState) { cs.baseMergeDone = true })
	}

	// E.3: verify done_when — manager first, resolver after the window.
	if len(s.doneWhenOrNil()) > 0 && !s.cs.verified {
		if s.cs.verifyAskedAt.IsZero() {
			c.askManagerToVerify(ctx, s)
			return
		}
		if c.now().Sub(s.cs.verifyAskedAt) < pol.ManagerVerifyTimeout {
			return
		}
		c.callResolver(ctx, ResolverRequest{
			RunID: s.runID, Branch: s.integration, Class: BlockerDoneWhen,
			Detail: "Every task has landed but the manager did not confirm the plan's done_when criteria in time. Verify each criterion against this integration branch, fix mechanical failures (a failing `wd check`), commit and push, and report per criterion.\n" + strings.Join(prefixLines("- ", s.doneWhen), "\n"),
		})
		c.updateCompletion(s.runID, func(cs *completionState) { cs.verified, cs.lapsed = true, true })
		return
	}

	// E.4: open (or adopt) the single final PR and gate it.
	spec := c.finalPRSpec(ctx, lr, s)
	fp, err := cr.EnsureFinalPR(ctx, s.repo, spec)
	if errors.Is(err, ErrNothingToMerge) {
		c.completeFinal(ctx, s.runID, 0, "integration has nothing beyond "+s.defaultBranch)
		return
	}
	if err != nil {
		c.auditRun(ctx, s.runID, "autopilot_final_pr_error", err.Error())
		return
	}
	if s.cs.finalPR == nil || s.cs.finalPR.Number != fp.Number {
		c.auditRun(ctx, s.runID, "autopilot_final_pr_opened", fmt.Sprintf("pr=%d url=%s", fp.Number, fp.URL))
	}
	st, err := cr.FinalPRStatus(ctx, s.repo, s.gate, fp, s.integration)
	if err != nil {
		c.setFinalPR(s.runID, fp, FinalGatePending)
		return
	}
	switch st.State {
	case "merged":
		c.finalPRMerged(ctx, s.runID, fp, st)
		return
	case "closed":
		c.finalPRClosed(ctx, s.runID, fp)
		return
	}
	fp.HeadSHA = firstNonEmpty(st.HeadSHA, fp.HeadSHA)
	switch st.Gate {
	case GateGreen:
		c.setFinalPR(s.runID, fp, FinalGateGreen)
		c.auditRun(ctx, s.runID, "autopilot_final_pr_green", fmt.Sprintf("pr=%d sha=%s", fp.Number, fp.HeadSHA))
		c.enterAwaitingMerge(ctx, s.runID, fp)
	case GateRed:
		c.setFinalPR(s.runID, fp, FinalGateRed)
		c.fixFinalPR(ctx, s, fp, st.Detail)
	default:
		c.setFinalPR(s.runID, fp, FinalGatePending)
	}
}

func (s completionSnapshot) doneWhenOrNil() []string { return s.doneWhen }

// resolverWait is how long a base_merge resolver is given before the merge is retried.
const resolverWait = 30 * time.Minute

// Resolver classes added by the completion phase.
const (
	BlockerBaseMerge = "base_merge"
	BlockerDoneWhen  = "done_when"
)

func prefixLines(p string, xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = p + x
	}
	return out
}

// fixFinalPR handles a red final PR (spec §E.4/E.5): one fix per red head SHA via
// a resolver on a NEW branch off integration (its PR into integration is landed
// by the landing pass, moving the final PR head), bounded by MaxFinalFixes, past
// which the run parks as final_pr_unfixable.
func (c *Controller) fixFinalPR(ctx context.Context, s completionSnapshot, fp FinalPR, detail string) {
	pol := c.completionPolicyValue()
	if s.cs.lastRedSHA == fp.HeadSHA {
		return // already dispatched for this head; wait for it to move
	}
	attempts := 0
	if s.cs.finalPR != nil {
		attempts = s.cs.finalPR.FixAttempts
	}
	if attempts >= pol.MaxFinalFixes {
		c.mu.Lock()
		defer c.mu.Unlock()
		r, ok := c.runs[s.runID]
		if !ok || r.needsAttention != "" {
			return
		}
		if gr, ok := c.runtime.(GuardianRuntime); ok {
			c.park(gr, r, KindFinalPRUnfixable, fmt.Errorf("the final PR #%d is still red after %d automated fixes. Failing check: %s. Fix integration, then run \"wd plan resume\"; autopilot will not merge the final PR", fp.Number, attempts, firstNonEmpty(strings.TrimSpace(detail), "unknown")))
			c.persistRunLocked(r)
		}
		return
	}
	n := attempts + 1
	branch := fmt.Sprintf("final-fix-%d-%s", n, shortRun(s.runID))
	started, err := c.SpawnResolver(ctx, ResolverRequest{
		RunID: s.runID, TaskID: fmt.Sprintf("final-fix-%d", n), Branch: branch, BaseBranch: s.integration,
		Class: BlockerRedGate,
		Detail: fmt.Sprintf("The FINAL PR #%d (%s → %s) is red: %s\nThis worktree is a new branch %s off %s. Fix the failure here, run `wd check`, commit, push, and open a PR with `gh pr create --base %s`; autopilot lands it into %s and re-gates the final PR. Never touch or merge the final PR itself.",
			fp.Number, s.integration, s.defaultBranch, firstNonEmpty(strings.TrimSpace(detail), "see the failing checks"), branch, s.integration, s.integration, s.integration),
	})
	if err != nil || !started {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.runs[s.runID]; ok {
		r.completion.lastRedSHA = fp.HeadSHA
		if r.completion.finalPR != nil {
			r.completion.finalPR.FixAttempts = n
		}
	}
}

func shortRun(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// askManagerToVerify wakes the manager once with the verification instruction.
func (c *Controller) askManagerToVerify(ctx context.Context, s completionSnapshot) {
	c.mu.Lock()
	r, ok := c.runs[s.runID]
	if !ok {
		c.mu.Unlock()
		return
	}
	gr, _ := c.runtime.(GuardianRuntime)
	id := brainAgentID(r)
	c.mu.Unlock()
	msg := "All tasks are landed. Verify every done_when criterion against the integration branch (" + s.integration +
		") and call autopilot_complete. The daemon then opens the single final PR; do not open or merge it yourself.\n" +
		strings.Join(prefixLines("- ", s.doneWhen), "\n")
	if gr != nil && id != "" {
		if err := gr.NudgeBrain(ctx, id, msg); err != nil {
			return // retry next tick; the window starts only once delivered
		}
	}
	c.updateCompletion(s.runID, func(cs *completionState) { cs.verifyAskedAt = c.now() })
	c.auditRun(ctx, s.runID, "autopilot_verify_requested", "done_when="+strconv.Itoa(len(s.doneWhen)))
}

// callResolver spawns a resolver for a completion blocker (capped per branch by
// SpawnResolver, which parks as resolver_exhausted past MaxResolverAttempts).
func (c *Controller) callResolver(ctx context.Context, req ResolverRequest) {
	if _, err := c.SpawnResolver(ctx, req); err != nil {
		c.auditRun(ctx, req.RunID, "autopilot_resolver_error", err.Error())
	}
}

// finalPRSpec renders the deterministic final-PR title and body (spec §E.4).
func (c *Controller) finalPRSpec(ctx context.Context, lr LandingRuntime, s completionSnapshot) FinalPRSpec {
	in := FinalPRBodyInput{
		RunID: s.runID, Name: s.name, Goal: s.goal, Integration: s.integration,
		DefaultBranch: s.defaultBranch, Tasks: s.tasks, DoneWhen: s.doneWhen,
		Verified: s.cs.verified && !s.cs.lapsed, ResolverCalls: sumAttempts(s.resolverAttempts),
	}
	if s.cs.finalPR != nil {
		in.FinalFixes = s.cs.finalPR.FixAttempts
	}
	if rt, ok := c.runtime.(interface{ NewLedger(string) *Ledger }); ok {
		if l := rt.NewLedger(s.runID); l != nil {
			in.Landings, _ = l.Landings()
		}
	}
	return FinalPRSpec{
		Integration: s.integration, DefaultBranch: s.defaultBranch,
		Title: FinalPRTitle(s.name, s.runID, nil),
		Body:  FinalPRBody(in),
		Name:  s.name, RunID: s.runID,
	}
}

func sumAttempts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// FinalPRBodyInput feeds FinalPRBody.
type FinalPRBodyInput struct {
	RunID, Name, Goal, Integration, DefaultBranch string
	Tasks                                         []PlanTask
	Landings                                      []Landing
	DoneWhen                                      []string
	Verified                                      bool
	ResolverCalls, FinalFixes                     int
}

// FinalPRBody renders the final PR body: deterministic (no model call, no clock)
// so re-rendering an adopted PR is idempotent.
func FinalPRBody(in FinalPRBodyInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Autopilot run %s — %s\n", in.RunID, firstNonEmpty(in.Name, "plan"))
	fmt.Fprintf(&b, "**Goal:** %s\n", strings.TrimSpace(in.Goal))
	fmt.Fprintf(&b, "**Integration branch:** %s (into %s)\n\n", in.Integration, in.DefaultBranch)
	b.WriteString("### Tasks landed\n| Task | PR | Branch | Merged at |\n|---|---|---|---|\n")
	landings := append([]Landing(nil), in.Landings...)
	sort.SliceStable(landings, func(i, j int) bool { return landings[i].LandedAt < landings[j].LandedAt })
	for _, t := range in.Tasks {
		pr, branch, at := "", "", ""
		for _, l := range landings {
			if (t.LandedPR != 0 && l.PR == t.LandedPR) || (t.LandedPR == 0 && strings.Contains(l.Branch, t.ID)) {
				pr, branch, at = "#"+strconv.Itoa(l.PR), l.Branch, l.LandedAt
				break
			}
		}
		if pr == "" && t.LandedPR != 0 {
			pr = "#" + strconv.Itoa(t.LandedPR)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", t.ID, pr, branch, at)
	}
	b.WriteString("\n### done_when\n")
	if len(in.DoneWhen) == 0 {
		b.WriteString("- none declared (every task landed)\n")
	}
	evidence := "verified by manager"
	if !in.Verified {
		evidence = "not confirmed by the manager — please review"
	}
	for _, d := range in.DoneWhen {
		mark := " "
		if in.Verified {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s — %s\n", mark, d, evidence)
	}
	fmt.Fprintf(&b, "\n### Run summary\nTasks landed: %d. Resolver calls: %d. Final-PR fixes: %d.\n", len(landings), in.ResolverCalls, in.FinalFixes)
	b.WriteString("\n### Not done / follow-ups\nnone\n")
	b.WriteString("\n---\nOpened by warden autopilot. Autopilot never merges this PR — review and merge it yourself.\n")
	return b.String()
}

// completeFinal runs the existing completion actions (marker, teardown, ledger
// retained) and notifies the owner once. Idempotent via CompleteRun.
func (c *Controller) completeFinal(ctx context.Context, runID string, pr int, why string) {
	c.mu.Lock()
	r, ok := c.runs[runID]
	already := ok && r.state == StateComplete
	var gr GuardianRuntime
	if ok {
		gr, _ = c.runtime.(GuardianRuntime)
	}
	c.mu.Unlock()
	if !ok || already {
		return
	}
	if _, err := c.CompleteRun(ctx, runID); err != nil {
		c.auditRun(ctx, runID, "autopilot_complete_error", err.Error())
		return
	}
	c.auditRun(ctx, runID, "autopilot_complete", why)
	if gr != nil {
		msg := fmt.Sprintf("autopilot run %s complete — %s.", runID, why)
		if pr > 0 {
			msg = fmt.Sprintf("autopilot run %s — final PR #%d merged — plan complete.", runID, pr)
		}
		gr.NotifyEscalation(runID, "autopilot run complete", msg)
	}
}

func (c *Controller) completionPolicyValue() CompletionPolicy {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completionPolicy.withDefaults()
}

func (c *Controller) updateCompletion(runID string, f func(*completionState)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.runs[runID]; ok {
		f(&r.completion)
	}
}

func (c *Controller) setFinalizing(runID string, on bool) {
	c.updateCompletion(runID, func(cs *completionState) {
		cs.finalizing = on
		if !on {
			*cs = completionState{}
		}
	})
}

func (c *Controller) setFinalPR(runID string, fp FinalPR, gate string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok {
		return
	}
	fix := 0
	if r.completion.finalPR != nil {
		fix = r.completion.finalPR.FixAttempts
	}
	fp.Gate, fp.FixAttempts = gate, fix
	fp.State = firstNonEmpty(fp.State, "open")
	r.completion.finalPR = &fp
	sf := c.surfaceLocked(r)
	sf.FinalPR = r.completion.finalPR.snapshot()
	sf.Verified = r.completion.verified
	c.persistSurfaceLocked(r)
}

func (c *Controller) auditRun(ctx context.Context, runID, action, detail string) {
	if gr, ok := c.runtime.(GuardianRuntime); ok {
		gr.AuditRunEvent(ctx, runID, action, "", detail)
	}
}

// reportedState is the state shown in status: a run in its completion phase
// reports finalizing while internally remaining active.
func (r *run) reportedState() RunState {
	if r.state == StateActive && r.completion.awaiting != nil {
		return StateAwaitingMerge
	}
	if r.state == StateActive && r.completion.finalizing {
		return StateFinalizing
	}
	return r.state
}

// awaitingMergeLocked reports whether r is only waiting for its final PR to be
// merged, hydrating the persisted record after a restart. Caller holds c.mu.
func (c *Controller) awaitingMergeLocked(r *run) bool {
	c.surfaceLocked(r)
	return r.completion.awaiting != nil
}

func (c *Controller) reportedStateLocked(r *run) RunState {
	c.surfaceLocked(r)
	return r.reportedState()
}

func (f *FinalPR) snapshot() *FinalPR {
	if f == nil {
		return nil
	}
	cp := *f
	return &cp
}

// enterAwaitingMerge moves a run whose final PR just went green into the
// awaiting_merge phase (plan-finish-flow §1): the record is persisted first (a
// crash then resumes polling), the manager and every remaining run agent are
// torn down, and the owner is notified once.
func (c *Controller) enterAwaitingMerge(ctx context.Context, runID string, fp FinalPR) {
	c.mu.Lock()
	r, ok := c.runs[runID]
	if !ok || r.state != StateActive || r.completion.awaiting != nil {
		c.mu.Unlock()
		return
	}
	sf := c.surfaceLocked(r)
	notify := !sf.FinalNotified
	r.completion.awaitGen++
	aw := &AwaitingMerge{Since: c.now().UTC().Format(time.RFC3339), GreenSHA: fp.HeadSHA, Notified: true, PR: fp.Number,
		gen: r.completion.awaitGen}
	r.completion.awaiting = aw
	r.completion.finalizing = false
	sf.AwaitingMerge, sf.FinalNotified, sf.Verified = aw, true, true
	c.persistSurfaceLocked(r)
	if err := c.teardownBrain(ctx, r); err != nil {
		slog.Warn("autopilot: manager teardown on awaiting_merge failed", "run", runID, "err", err)
	}
	c.persistRunLocked(r)
	gr, _ := c.runtime.(GuardianRuntime)
	reaper, _ := c.runtime.(RunAgentReaper)
	c.mu.Unlock()
	c.auditRun(ctx, runID, "autopilot_awaiting_merge", fmt.Sprintf("pr=%d sha=%s", fp.Number, fp.HeadSHA))
	if reaper != nil {
		if err := reaper.TerminateRunAgents(ctx, runID); err != nil {
			c.auditRun(ctx, runID, "autopilot_awaiting_merge_cleanup_error", err.Error())
		}
	}
	if notify && gr != nil {
		gr.NotifyEscalation(runID, "autopilot final PR ready",
			fmt.Sprintf("autopilot run %s — final PR #%d is green and waiting for your review (autopilot will not merge it).", runID, fp.Number))
	}
}

// leaveAwaiting clears the awaiting record (the run goes back to finalizing or
// active; the guardian respawns a manager for an active run as usual).
func (c *Controller) leaveAwaiting(ctx context.Context, runID, why string) {
	c.mu.Lock()
	r, ok := c.runs[runID]
	if !ok || r.completion.awaiting == nil {
		c.mu.Unlock()
		return
	}
	r.completion.awaiting = nil
	sf := c.surfaceLocked(r)
	sf.AwaitingMerge = nil
	c.persistSurfaceLocked(r)
	c.mu.Unlock()
	c.auditRun(ctx, runID, "autopilot_awaiting_merge_left", why)
}

// awaitMergePass polls the final PR once per merge_poll_interval and acts on
// what it finds (plan-finish-flow §2, §3). It runs off c.mu; the result is
// applied only if the run is still the same awaiting episode.
func (c *Controller) awaitMergePass(ctx context.Context, cr CompletionRuntime, s completionSnapshot) {
	aw := s.awaiting
	if !aw.nextPollAt.IsZero() && c.now().Before(aw.nextPollAt) {
		return
	}
	pol := c.completionPolicyValue()
	c.mu.Lock()
	if r, ok := c.runs[s.runID]; ok && r.completion.awaiting != nil {
		r.completion.awaiting.nextPollAt = c.now().Add(pol.MergePollInterval)
	}
	c.mu.Unlock()

	fp := FinalPR{Number: aw.PR, HeadSHA: aw.GreenSHA}
	if s.cs.finalPR != nil {
		fp = *s.cs.finalPR
	}
	st, err := cr.FinalPRView(ctx, s.repo, fp)
	if err != nil {
		return // retried at the next interval; a failed poll never parks the run
	}
	if !c.awaitingCurrent(s.runID, aw) {
		return // stale: the run left the episode while the poll was in flight
	}
	switch st.State {
	case "merged":
		c.finalPRMerged(ctx, s.runID, fp, st)
	case "closed":
		c.finalPRClosed(ctx, s.runID, fp)
	default:
		switch {
		case st.HeadSHA != "" && aw.GreenSHA != "" && st.HeadSHA != aw.GreenSHA:
			c.regress(ctx, s.runID, fp, st, false)
		case st.needsBaseMerge():
			c.regress(ctx, s.runID, fp, st, true)
		}
	}
}

// awaitingCurrent reports whether the run is still active and in the same
// awaiting episode (same generation and PR) a poll was started for.
func (c *Controller) awaitingCurrent(runID string, aw *AwaitingMerge) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	return ok && r.state == StateActive && r.completion.awaiting != nil &&
		r.completion.awaiting.gen == aw.gen && r.completion.awaiting.PR == aw.PR
}

// regress leaves awaiting_merge for finalizing so the existing completion pass
// re-gates the new head, or (baseMerge) re-merges the default branch first.
func (c *Controller) regress(ctx context.Context, runID string, fp FinalPR, st FinalPRState, baseMerge bool) {
	why := "final PR head moved to " + st.HeadSHA
	if baseMerge {
		why = fmt.Sprintf("final PR #%d is %s/%s", fp.Number, firstNonEmpty(st.Mergeable, "?"), firstNonEmpty(st.MergeStateStatus, "?"))
	}
	c.leaveAwaiting(ctx, runID, why)
	c.updateCompletion(runID, func(cs *completionState) {
		cs.finalizing = true
		cs.lastRedSHA = ""
		if baseMerge {
			cs.baseMergeDone = false
		}
		if cs.finalPR != nil {
			cs.finalPR.Gate = FinalGatePending
			cs.finalPR.HeadSHA = firstNonEmpty(st.HeadSHA, cs.finalPR.HeadSHA)
		}
	})
}

// finalPRMerged records the merge and completes the run (plan-finish-flow §4).
func (c *Controller) finalPRMerged(ctx context.Context, runID string, fp FinalPR, st FinalPRState) {
	fp.HeadSHA = firstNonEmpty(st.HeadSHA, fp.HeadSHA)
	c.mu.Lock()
	if r, ok := c.runs[runID]; ok {
		fp.Gate, fp.State, fp.MergedAt = FinalGateGreen, "merged", st.MergedAt
		if r.completion.finalPR != nil {
			fp.FixAttempts = r.completion.finalPR.FixAttempts
		}
		r.completion.finalPR = &fp
		sf := c.surfaceLocked(r)
		sf.FinalPR = fp.snapshot()
		sf.AwaitingMerge = nil
		r.completion.awaiting = nil
		c.persistSurfaceLocked(r)
	}
	c.mu.Unlock()
	c.auditRun(ctx, runID, "autopilot_final_pr_merged", fmt.Sprintf("pr=%d sha=%s", fp.Number, fp.HeadSHA))
	c.completeFinal(ctx, runID, fp.Number, fmt.Sprintf("final PR #%d merged", fp.Number))
}

// finalPRClosed parks the run: the PR was closed without merging.
func (c *Controller) finalPRClosed(ctx context.Context, runID string, fp FinalPR) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	if !ok || r.needsAttention != "" {
		return
	}
	gr, ok := c.runtime.(GuardianRuntime)
	if !ok {
		return
	}
	r.completion.awaiting = nil
	r.completion.finalizing = false
	fp.State = "closed"
	r.completion.finalPR = nil // a resumed run opens a new final PR
	sf := c.surfaceLocked(r)
	sf.AwaitingMerge = nil
	sf.FinalPR = fp.snapshot()
	c.park(gr, r, KindFinalPRClosed, fmt.Errorf("the final PR #%d was closed without merging. Reopen it, or run \"wd plan resume\" to open a new one; run \"wd plan stop\" to end the run and keep the branch", fp.Number))
	c.persistSurfaceLocked(r)
	c.persistRunLocked(r)
}
