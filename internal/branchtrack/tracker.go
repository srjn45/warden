// Package branchtrack provides a daemon-side monitor that reports, per active
// agent, the state of its branch relative to GitHub CI and to origin/main. It
// delivers informational alerts — never blocking — through the existing
// mailbox (to the agent) and the operator's desktop notifier (CI failures
// only).
//
// For each tracked agent with a branch it answers two questions per tick:
//   - CI status: success / failure / pending / none, from `gh run list` run
//     inside the agent's worktree (so gh infers the remote).
//   - Branch vs main: how many commits behind/ahead origin/main, and whether
//     the branch tip is already merged into origin/main.
//
// Every subprocess fails open: a missing binary, an unauthenticated gh, a
// timeout, or a not-a-repo worktree skips that branch this tick rather than
// propagating. State is in-memory and ephemeral; a dedup window suppresses
// re-alerting the same (branch, signal-state) within the window.
//
// Design: docs/superpowers/specs/2026-06-14-intelligent-inter-agent-collaboration-design.md
// (BranchTracker). Mirrors internal/collab's Monitor structurally.
package branchtrack

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/notify"
	"github.com/srjn45/warden/internal/store"
)

const (
	// dedupWindow suppresses re-alerting the same (branch, signal-state) pair
	// within this span. Mirrors collab's window: without it an open signal
	// (e.g. a red build) would re-alert every tick.
	dedupWindow = 5 * time.Minute
	// subprocessTimeout bounds each gh/git subprocess so one wedged worktree or
	// a slow GitHub call can't stall the scan.
	subprocessTimeout = 10 * time.Second
	// daemonSender is the reserved provenance id stamped on tracker alerts (same
	// convention as collab). Agents can't forge it; daemon-internal writes call
	// Append directly.
	daemonSender = "daemon"
	// behindThreshold is how far behind origin/main a branch may fall before the
	// tracker nudges the agent to sync. Hardcoded (warden runs ≤10 agents; a knob
	// would be noise).
	behindThreshold = 10
)

// CI state values reported in BranchStatus.CI.State.
const (
	ciSuccess = "success"
	ciFailure = "failure"
	ciPending = "pending"
	ciNone    = "none"
)

// Lister is the slice of the session store the tracker needs. store.Store
// satisfies it; tests supply a fake.
type Lister interface {
	List(ctx context.Context) ([]*store.Session, error)
}

// CIStatus is the latest CI run observed for a branch.
type CIStatus struct {
	State    string `json:"state"`              // success | failure | pending | none
	Workflow string `json:"workflow,omitempty"` // workflow name of the run
	URL      string `json:"url,omitempty"`      // run URL
}

// BranchStatus is one agent's branch+CI snapshot.
type BranchStatus struct {
	AgentID string   `json:"agent_id"`
	Name    string   `json:"name,omitempty"`
	Branch  string   `json:"branch"`
	CI      CIStatus `json:"ci"`
	Behind  int      `json:"behind"` // commits in origin/main not in HEAD
	Ahead   int      `json:"ahead"`  // commits in HEAD not in origin/main
	Merged  bool     `json:"merged"` // branch tip already in origin/main and main moved past it
}

// Tracker monitors active agents' branches against CI and origin/main.
type Tracker struct {
	store    Lister
	mbox     *mailbox.Store
	notifier notify.Notifier
	// ci and branchCmp are the subprocess seams; overridable in tests.
	ci        func(ctx context.Context, worktree, branch string) CIStatus
	branchCmp func(ctx context.Context, worktree string) (behind, ahead int, merged bool)

	mu    sync.Mutex
	dedup map[string]time.Time // "branch\x00signal-state" -> last alerted
}

// NewTracker returns a Tracker backed by the session store, mailbox, and the
// operator notifier (CI failures fan out to the desktop too).
func NewTracker(st Lister, mbox *mailbox.Store, notifier notify.Notifier) *Tracker {
	return &Tracker{
		store:     st,
		mbox:      mbox,
		notifier:  notifier,
		ci:        ghCIStatus,
		branchCmp: gitBranchCompare,
		dedup:     map[string]time.Time{},
	}
}

// SetNotifier swaps the operator notifier. NewTracker installs a log-only
// default; the daemon wires the real one post-construction (the feature is
// opt-in, so construction can't depend on config).
func (t *Tracker) SetNotifier(n notify.Notifier) { t.notifier = n }

// Run scans until ctx is cancelled. A non-positive interval disables the
// tracker (returns immediately) — the daemon passes 0 when the feature is off.
func (t *Tracker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.tick(ctx)
		}
	}
}

// tick recomputes every tracked branch's status and alerts on newly observed
// signals (subject to the dedup window).
func (t *Tracker) tick(ctx context.Context) {
	statuses, err := t.Statuses(ctx)
	if err != nil {
		slog.Warn("branchtrack: status scan failed", "err", err)
		return
	}
	t.pruneDedup()
	for _, bs := range statuses {
		// CI failure → inbox + desktop (the spec reserves desktop alerts for CI
		// failures). Keying the dedup on the CI state re-alerts on a state change
		// (e.g. pending→failure) but suppresses a steady red build every tick.
		if bs.CI.State == ciFailure && t.shouldAlert(bs.Branch, "ci:"+bs.CI.State) {
			t.alertCIFailure(bs)
		}
		// Branch merged into main → inbox note.
		if bs.Merged && t.shouldAlert(bs.Branch, "merged") {
			body := fmt.Sprintf("✅ Branch %s is merged into main — consider `wd rotate` or finishing up.", bs.Branch)
			t.deliver(bs.AgentID, body)
		}
		// Branch fallen well behind main → inbox nudge.
		if bs.Behind > behindThreshold && t.shouldAlert(bs.Branch, "behind") {
			body := fmt.Sprintf("⏬ Branch %s is %d commits behind main — consider `wd sync`.", bs.Branch, bs.Behind)
			t.deliver(bs.AgentID, body)
		}
	}
}

// alertCIFailure delivers a CI-failure note to the agent's inbox and a desktop
// notification to the operator.
func (t *Tracker) alertCIFailure(bs BranchStatus) {
	body := fmt.Sprintf("❌ CI failed on %s\nWorkflow: %s\n%s", bs.Branch, bs.CI.Workflow, bs.CI.URL)
	t.deliver(bs.AgentID, body)
	if t.notifier != nil {
		title := "CI failed: " + agentLabel(bs)
		t.notifier.Notify(title, fmt.Sprintf("%s on %s\n%s", bs.CI.Workflow, bs.Branch, bs.CI.URL))
	}
}

// deliver appends an informational note to an agent's inbox (best-effort).
func (t *Tracker) deliver(agentID, body string) {
	if _, err := t.mbox.Append(mailbox.Message{To: agentID, From: daemonSender, Body: body}); err != nil {
		slog.Warn("branchtrack: deliver alert failed", "agent", agentID, "err", err)
	}
}

// Statuses returns the current branch+CI snapshot for every tracked agent with
// a branch. Recomputed on demand (cheap at warden's scale; no shared cache).
// gh/git work is deduped by branch — multiple agents rarely share a branch, but
// the same branch is never queried twice in a scan.
func (t *Tracker) Statuses(ctx context.Context) ([]BranchStatus, error) {
	sessions, err := t.store.List(ctx)
	if err != nil {
		return nil, err
	}
	type branchData struct {
		ci            CIStatus
		behind, ahead int
		merged        bool
	}
	cache := map[string]branchData{}
	var out []BranchStatus
	for _, s := range sessions {
		if !tracked(s) || s.Branch == "" {
			continue
		}
		bd, ok := cache[s.Branch]
		if !ok {
			bd.ci = t.ci(ctx, s.Worktree, s.Branch)
			bd.behind, bd.ahead, bd.merged = t.branchCmp(ctx, s.Worktree)
			cache[s.Branch] = bd
		}
		out = append(out, BranchStatus{
			AgentID: s.ID,
			Name:    s.Name,
			Branch:  s.Branch,
			CI:      bd.ci,
			Behind:  bd.behind,
			Ahead:   bd.ahead,
			Merged:  bd.merged,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

// tracked reports whether a session should be scanned: it has a worktree and is
// not in a terminal state (matches collab's filter). A paused agent still owns a
// branch worth tracking.
func tracked(s *store.Session) bool {
	if s.Worktree == "" {
		return false
	}
	switch s.Status {
	case store.StatusDone, store.StatusErrored, store.StatusOrphaned:
		return false
	}
	return true
}

// ghCIStatus reads the latest CI run for branch by running `gh run list` inside
// worktree so gh infers the remote. Any error — gh missing, unauthenticated,
// timeout, not a repo, no runs — yields state "none", so the branch is simply
// skipped for CI purposes this tick.
func ghCIStatus(ctx context.Context, worktree, branch string) CIStatus {
	cctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", "run", "list", "--branch", branch,
		"--json", "status,conclusion,headSha,workflowName,url", "--limit", "1")
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return CIStatus{State: ciNone}
	}
	var runs []struct {
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		HeadSha      string `json:"headSha"`
		WorkflowName string `json:"workflowName"`
		URL          string `json:"url"`
	}
	if err := json.Unmarshal(out, &runs); err != nil || len(runs) == 0 {
		return CIStatus{State: ciNone}
	}
	return parseCIRun(runs[0].Status, runs[0].Conclusion, runs[0].WorkflowName, runs[0].URL)
}

// StatusForSHA reports the CI conclusion for a specific head SHA, used by
// autopilot's `land` gate (docs/specs/autopilot.md §6.1) which must gate on the
// exact PR head, not merely the branch's latest run. It lists the branch's
// recent runs in worktree (so gh infers the remote) and returns the status of
// the run whose headSha matches. When the branch has runs but none match the
// SHA yet, it reports pending (a run for that commit hasn't appeared); when the
// branch has NO runs at all it reports none, which the `ci` gate maps to
// ci_missing. Fails open to none, exactly like ghCIStatus.
func StatusForSHA(ctx context.Context, worktree, branch, headSHA string) CIStatus {
	cctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "gh", "run", "list", "--branch", branch,
		"--json", "status,conclusion,headSha,workflowName,url", "--limit", "20")
	cmd.Dir = worktree
	out, err := cmd.Output()
	if err != nil {
		return CIStatus{State: ciNone}
	}
	var runs []struct {
		Status       string `json:"status"`
		Conclusion   string `json:"conclusion"`
		HeadSha      string `json:"headSha"`
		WorkflowName string `json:"workflowName"`
		URL          string `json:"url"`
	}
	if err := json.Unmarshal(out, &runs); err != nil || len(runs) == 0 {
		return CIStatus{State: ciNone}
	}
	for _, r := range runs {
		if r.HeadSha == headSHA {
			return parseCIRun(r.Status, r.Conclusion, r.WorkflowName, r.URL)
		}
	}
	// Runs exist for the branch but none for this head yet — the check for this
	// commit is still pending, not missing.
	return CIStatus{State: ciPending}
}

// State constants for CIStatus, exported so callers outside branchtrack (the
// land gate) can classify a StatusForSHA result without string literals.
const (
	CISuccess = ciSuccess
	CIFailure = ciFailure
	CIPending = ciPending
	CINone    = ciNone
)

// parseCIRun maps a gh run's status/conclusion to a CIStatus. A completed run
// is success/failure by conclusion (cancelled/skipped/neutral are not
// actionable → none); anything still in flight is pending.
func parseCIRun(status, conclusion, workflow, url string) CIStatus {
	st := ciPending
	if status == "completed" {
		switch conclusion {
		case "success":
			st = ciSuccess
		case "failure", "timed_out", "startup_failure":
			st = ciFailure
		default:
			st = ciNone
		}
	}
	return CIStatus{State: st, Workflow: workflow, URL: url}
}

// gitBranchCompare returns how far HEAD is behind/ahead of origin/main and
// whether it is already merged. Fails open: on any git error each value is its
// zero (0 behind, 0 ahead, not merged), so a broken worktree produces no
// spurious alerts.
//
// "merged" requires the branch tip to be an ancestor of origin/main AND main to
// have moved past it (behind > 0). The bare ancestor check alone is true for a
// freshly-created branch sitting exactly on the main tip — that is not a merge.
func gitBranchCompare(ctx context.Context, worktree string) (behind, ahead int, merged bool) {
	ahead = gitRevCount(ctx, worktree, "origin/main..HEAD")
	behind = gitRevCount(ctx, worktree, "HEAD..origin/main")
	merged = behind > 0 && gitIsAncestor(ctx, worktree, "HEAD", "origin/main")
	return behind, ahead, merged
}

// gitRevCount returns the commit count for revspec (e.g. "HEAD..origin/main"),
// or 0 on any error.
func gitRevCount(ctx context.Context, worktree, revspec string) int {
	cctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", worktree, "rev-list", "--count", revspec)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(string(out), "%d", &n); err != nil {
		return 0
	}
	return n
}

// gitIsAncestor reports whether rev is an ancestor of of. Any error (including
// the non-ancestor exit code 1) yields false.
func gitIsAncestor(ctx context.Context, worktree, rev, of string) bool {
	cctx, cancel := context.WithTimeout(ctx, subprocessTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", worktree, "merge-base", "--is-ancestor", rev, of)
	return cmd.Run() == nil
}

// shouldAlert reports whether (branch, signal) is outside its dedup window, and
// records the alert time when it is. signal encodes the signal-state so a state
// change (pending→failure) is treated as a fresh alert.
func (t *Tracker) shouldAlert(branch, signal string) bool {
	key := branch + "\x00" + signal
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.dedup[key]; ok && now.Sub(last) < dedupWindow {
		return false
	}
	t.dedup[key] = now
	return true
}

// pruneDedup drops dedup entries older than the window so the map can't grow
// unbounded across a long-lived daemon.
func (t *Tracker) pruneDedup() {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, ts := range t.dedup {
		if now.Sub(ts) >= dedupWindow {
			delete(t.dedup, k)
		}
	}
}

// agentLabel renders an agent as "name" when it has one, else its id.
func agentLabel(bs BranchStatus) string {
	if bs.Name != "" {
		return bs.Name
	}
	return bs.AgentID
}
