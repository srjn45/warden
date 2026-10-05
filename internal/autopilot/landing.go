package autopilot

import (
	"context"
	"errors"
	"strconv"
	"sync"
)

// Daemon-owned landing loop (run-to-final-pr spec §A). On the guardian's ticker
// every landable run's open integration-branch PRs are gated and, on green +
// mergeable, merged through the same idempotent Land the agent-facing route
// uses — so strategy, branch deletion, the landings record and idempotency are
// unchanged. Landing never depends on an agent remembering to poll.

// OpenPR is one open PR into a run's integration branch, as listed by the host.
type OpenPR struct {
	Number    int
	HeadRef   string
	HeadSHA   string
	Draft     bool
	Mergeable string // MERGEABLE | CONFLICTING | UNKNOWN
	URL       string
}

// LandOwner is the run-owned task/worker a PR head maps to (spec §A.2).
type LandOwner struct {
	TaskID   string
	WorkerID string // owning session id; "" when none survives
	Worktree string // worker worktree (local gate / CI query dir); may be ""
}

// FixKind says why a PR is handed to the fix hook instead of being merged.
type FixKind string

const (
	FixRed      FixKind = "red"
	FixConflict FixKind = "conflict"
)

// LandFix is the hand-off to the fix loop (spec §B; implemented separately).
type LandFix struct {
	PR     OpenPR
	TaskID string
	Kind   FixKind
	Detail string
}

// LandingRuntime is the daemon seam the landing pass drives. Its host is a
// LandHost extended with PR listing, so the pass shares Land's gate code.
type LandingRuntime interface {
	// LandingHost returns the gh/git host rooted at repo.
	LandingHost(repo string) LandingHost
	// ResolveLandOwner maps a PR head branch to its run-owned task; ok=false when
	// the PR is not run-owned (it is then ignored and never merged).
	ResolveLandOwner(ctx context.Context, runID, headRef string) (LandOwner, bool)
	// FinalizeLanding runs the post-merge bookkeeping (ledger landing + state,
	// plan task done, worker teardown, audit). Idempotent.
	FinalizeLanding(ctx context.Context, runID string, owner LandOwner, res LandResult)
	// DispatchFix receives a red or conflicted PR; it must never merge.
	DispatchFix(ctx context.Context, runID string, fix LandFix)
}

// LandingHost is LandHost plus listing a run's open PRs.
type LandingHost interface {
	LandHost
	// ListOpenPRs lists open PRs whose base is integration.
	ListOpenPRs(ctx context.Context, integration string) ([]OpenPR, error)
}

// errStaleHead aborts a land attempt whose PR head moved since it was listed.
var errStaleHead = errors.New("autopilot landing: PR head changed since listing")

// pinnedHost fails FindPR when the live head differs from the listed one, so a
// result computed for a stale SHA is dropped rather than acted on.
type pinnedHost struct {
	LandHost
	sha string
}

func (p pinnedHost) FindPR(ctx context.Context, branch string) (PRInfo, bool, error) {
	pr, ok, err := p.LandHost.FindPR(ctx, branch)
	if err == nil && ok && !pr.Merged && pr.HeadSHA != p.sha {
		return PRInfo{}, false, errStaleHead
	}
	return pr, ok, err
}

// landSnapshot is the in-memory slice of a run read under c.mu.
type landSnapshot struct {
	runID, repo, integration, defaultBranch, gate, strategy string
	deleteBranch                                            bool
}

const landConcurrency = 4

// landingTick runs one landing pass over every landable run. c.mu is held only
// to snapshot; all gh/git I/O runs unlocked, one pass per run (TryLock) and one
// in-flight land per PR.
func (c *Controller) landingTick(ctx context.Context) {
	c.mu.Lock()
	lr, ok := c.runtime.(LandingRuntime)
	if !ok {
		c.mu.Unlock()
		return
	}
	var snaps []landSnapshot
	for _, r := range c.runs {
		if !c.landingEligibleLocked(r) || r.integrationBranch == "" {
			continue
		}
		snaps = append(snaps, landSnapshot{
			runID: r.runID, repo: r.repo, integration: r.integrationBranch,
			defaultBranch: r.defaultBranch, gate: c.runGate(r),
			strategy: c.strategy, deleteBranch: c.deleteBranch,
		})
	}
	c.mu.Unlock()

	sem := make(chan struct{}, landConcurrency)
	var wg sync.WaitGroup
	for _, s := range snaps {
		mu := c.landLock("run:" + s.runID)
		if !mu.TryLock() {
			continue // previous pass still running
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(s landSnapshot, mu *sync.Mutex) {
			defer wg.Done()
			defer func() { <-sem }()
			defer mu.Unlock()
			c.landRun(ctx, lr, s)
		}(s, mu)
	}
	wg.Wait()
}

// landingEligibleLocked: kill switch — paused/stopped/complete/registered runs
// land nothing.
func (c *Controller) landingEligibleLocked(r *run) bool {
	switch r.state {
	case StateActive, StateHealing, StateDegraded:
		return true
	}
	return false
}

// landingEligible re-checks eligibility under the lock (apply-time re-check).
func (c *Controller) landingEligible(runID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.runs[runID]
	return ok && c.landingEligibleLocked(r)
}

// landLock returns the process-wide mutex for key (per run or per PR).
func (c *Controller) landLock(key string) *sync.Mutex {
	m, _ := c.landLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (c *Controller) landRun(ctx context.Context, lr LandingRuntime, s landSnapshot) {
	host := lr.LandingHost(s.repo)
	if host == nil {
		return
	}
	prs, err := host.ListOpenPRs(ctx, s.integration)
	if err != nil {
		return
	}
	var ledger *Ledger
	if rt, ok := c.runtime.(interface{ NewLedger(string) *Ledger }); ok {
		ledger = rt.NewLedger(s.runID)
	}
	for _, pr := range prs {
		if ctx.Err() != nil {
			return
		}
		if pr.Draft {
			continue
		}
		owner, owned := lr.ResolveLandOwner(ctx, s.runID, pr.HeadRef)
		if !owned {
			continue // foreign or unmatched head: never touched
		}
		c.landPR(ctx, lr, host, ledger, s, pr, owner)
	}
}

// landPR lands one PR under its per-PR lock. Land re-checks every precondition,
// so a race between listing and merging can never merge red/conflicting code.
func (c *Controller) landPR(ctx context.Context, lr LandingRuntime, host LandingHost, ledger *Ledger, s landSnapshot, pr OpenPR, owner LandOwner) {
	mu := c.landLock("pr:" + s.runID + "/" + strconv.Itoa(pr.Number))
	if !mu.TryLock() {
		return // an agent `land` or a previous pass holds this PR
	}
	defer mu.Unlock()
	if !c.landingEligible(s.runID) {
		return
	}
	res, err := Land(ctx, LandRequest{
		RunActive: true, Owned: true,
		Branch: pr.HeadRef, Worktree: owner.Worktree,
		IntegrationBranch: s.integration, DefaultBranch: s.defaultBranch,
		Gate: s.gate, Strategy: s.strategy, DeleteBranch: s.deleteBranch,
	}, pinnedHost{LandHost: host, sha: pr.HeadSHA}, ledger)
	if err != nil {
		var le *LandError
		switch {
		case errors.Is(err, errStaleHead):
		case errors.As(err, &le):
			switch le.Kind {
			case ErrGateRed:
				lr.DispatchFix(ctx, s.runID, LandFix{PR: pr, TaskID: owner.TaskID, Kind: FixRed, Detail: le.Detail})
			case ErrNotMergeable:
				if pr.Mergeable == "CONFLICTING" {
					lr.DispatchFix(ctx, s.runID, LandFix{PR: pr, TaskID: owner.TaskID, Kind: FixConflict, Detail: le.Detail})
				}
				// UNKNOWN: GitHub still computing — re-evaluate next tick.
			}
			// pending / ci_missing / wrong_base / disabled: wait.
		}
		return
	}
	// Even if the run was paused mid-merge the merge is real: always record it.
	lr.FinalizeLanding(ctx, s.runID, owner, res)
}
