package autopilot

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeLandingHost is a LandHost+ListOpenPRs fake keyed by head branch.
type fakeLandingHost struct {
	mu      sync.Mutex
	prs     []OpenPR
	live    map[string]PRInfo // branch → live view
	gate    map[string]GateState
	merges  []int
	listErr error
}

func (h *fakeLandingHost) ListOpenPRs(context.Context, string) ([]OpenPR, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prs, h.listErr
}
func (h *fakeLandingHost) FindPR(_ context.Context, b string) (PRInfo, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.live[b]
	return p, ok, nil
}
func (h *fakeLandingHost) GateCI(_ context.Context, _, b, _ string) (GateState, string, error) {
	return h.gate[b], "detail", nil
}
func (h *fakeLandingHost) GateLocal(_ context.Context, w string) (GateState, string, error) {
	return h.gate[w], "detail", nil
}
func (h *fakeLandingHost) Merge(_ context.Context, pr int, _ string, _ bool) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.merges = append(h.merges, pr)
	for b, p := range h.live {
		if p.Number == pr {
			p.Merged = true
			h.live[b] = p
		}
	}
	return "merge-sha", nil
}

// landingRT wraps the guardian fake with the landing seam.
type landingRT struct {
	*guardianFake
	host   *fakeLandingHost
	owners map[string]LandOwner // head branch → owner (absent ⇒ not run-owned)
	mu     sync.Mutex
	final  []LandResult
	fixes  []LandFix
}

func (r *landingRT) LandingHost(string) LandingHost { return r.host }
func (r *landingRT) ResolveLandOwner(_ context.Context, _, head string) (LandOwner, bool) {
	o, ok := r.owners[head]
	return o, ok
}
func (r *landingRT) FinalizeLanding(_ context.Context, runID string, _ LandOwner, res LandResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.final = append(r.final, res)
	l := r.NewLedger(runID)
	_ = l.WriteTaskState("t1", LedgerLanded, "test")
	if !res.AlreadyLanded {
		_ = l.AppendLanding(Landing{Branch: res.Branch, SHA: res.HeadSHA, PR: res.PR})
	}
}
func (r *landingRT) DispatchFix(_ context.Context, _ string, f LandFix) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fixes = append(r.fixes, f)
}

func landingSetup(t *testing.T) (*Controller, *landingRT, string) {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	g := newGuardianFake()
	c, runID := enabledGuardianController(t, g, clock, cyclicResolver("a", "free"), testGuardian())
	rt := &landingRT{guardianFake: g, host: &fakeLandingHost{live: map[string]PRInfo{}, gate: map[string]GateState{}},
		owners: map[string]LandOwner{}}
	c.SetRuntime(rt)
	c.mu.Lock()
	r := c.runs[runID]
	r.integrationBranch, r.defaultBranch, r.state = "autopilot/x", "main", StateActive
	c.mu.Unlock()
	return c, rt, runID
}

func addPR(rt *landingRT, n int, head, sha, mergeable string, gate GateState, owned bool) {
	rt.host.prs = append(rt.host.prs, OpenPR{Number: n, HeadRef: head, HeadSHA: sha, Mergeable: mergeable})
	rt.host.live[head] = PRInfo{Number: n, BaseRef: "autopilot/x", HeadSHA: sha, Mergeable: mergeable == "MERGEABLE"}
	rt.host.gate[head] = gate
	rt.host.gate["wt-"+head] = gate
	if owned {
		rt.owners[head] = LandOwner{TaskID: "t1", Worktree: "wt-" + head}
	}
}

func TestLandingGreenLands(t *testing.T) {
	c, rt, _ := landingSetup(t)
	addPR(rt, 1, "w/a", "sha1", "MERGEABLE", GateGreen, true)
	c.landingTick(context.Background())
	require.Equal(t, []int{1}, rt.host.merges)
	require.Len(t, rt.final, 1)
	require.False(t, rt.final[0].AlreadyLanded)

	// Next tick (PR now merged/closed in reality): nothing more is merged.
	c.landingTick(context.Background())
	require.Equal(t, []int{1}, rt.host.merges)
}

func TestLandingPendingWaits(t *testing.T) {
	c, rt, _ := landingSetup(t)
	addPR(rt, 1, "w/a", "sha1", "MERGEABLE", GatePending, true)
	addPR(rt, 2, "w/b", "sha2", "MERGEABLE", GateMissing, true)
	c.landingTick(context.Background())
	require.Empty(t, rt.host.merges)
	require.Empty(t, rt.fixes)
}

func TestLandingRedAndConflictedGoToFixHook(t *testing.T) {
	c, rt, _ := landingSetup(t)
	addPR(rt, 1, "w/red", "sha1", "MERGEABLE", GateRed, true)
	addPR(rt, 2, "w/conf", "sha2", "CONFLICTING", GateGreen, true)
	addPR(rt, 3, "w/unk", "sha3", "UNKNOWN", GateGreen, true)
	c.landingTick(context.Background())
	require.Empty(t, rt.host.merges, "red/conflicted never merge")
	require.Len(t, rt.fixes, 2)
	kinds := map[int]FixKind{}
	for _, f := range rt.fixes {
		kinds[f.PR.Number] = f.Kind
	}
	require.Equal(t, FixRed, kinds[1])
	require.Equal(t, FixConflict, kinds[2])
}

func TestLandingIgnoresWrongBaseAndUnowned(t *testing.T) {
	c, rt, _ := landingSetup(t)
	addPR(rt, 1, "foreign", "sha1", "MERGEABLE", GateGreen, false)
	addPR(rt, 2, "w/wrongbase", "sha2", "MERGEABLE", GateGreen, true)
	p := rt.host.live["w/wrongbase"]
	p.BaseRef = "main"
	rt.host.live["w/wrongbase"] = p
	addPR(rt, 3, "w/draft", "sha3", "MERGEABLE", GateGreen, true)
	rt.host.prs[2].Draft = true
	c.landingTick(context.Background())
	require.Empty(t, rt.host.merges)
	require.Empty(t, rt.fixes)
}

func TestLandingStaleHeadDropped(t *testing.T) {
	c, rt, _ := landingSetup(t)
	addPR(rt, 1, "w/a", "sha1", "MERGEABLE", GateGreen, true)
	p := rt.host.live["w/a"]
	p.HeadSHA = "sha-new" // pushed after listing
	rt.host.live["w/a"] = p
	c.landingTick(context.Background())
	require.Empty(t, rt.host.merges)
	require.Empty(t, rt.final)
}

func TestLandingIdempotentAcrossRestart(t *testing.T) {
	c, rt, runID := landingSetup(t)
	addPR(rt, 1, "w/a", "sha1", "MERGEABLE", GateGreen, true)
	// Ledger already records this head SHA (merged, then daemon died pre-finalize).
	require.NoError(t, rt.NewLedger(runID).AppendLanding(Landing{Branch: "w/a", SHA: "sha1", PR: 1}))
	c.landingTick(context.Background())
	require.Empty(t, rt.host.merges, "no second merge")
	require.Len(t, rt.final, 1)
	require.True(t, rt.final[0].AlreadyLanded, "finalize still runs to heal bookkeeping")
}

func TestLandingManagerAbsentStillLands(t *testing.T) {
	c, rt, runID := landingSetup(t)
	rt.missing = map[string]bool{"brain-1": true}
	c.mu.Lock()
	c.runs[runID].brain = nil
	c.mu.Unlock()
	addPR(rt, 1, "w/a", "sha1", "MERGEABLE", GateGreen, true)
	c.landingTick(context.Background())
	require.Equal(t, []int{1}, rt.host.merges)
}

func TestLandingHonoursKillSwitch(t *testing.T) {
	for _, st := range []RunState{StatePaused, StateStopped} {
		c, rt, runID := landingSetup(t)
		c.mu.Lock()
		c.runs[runID].state = st
		c.mu.Unlock()
		addPR(rt, 1, "w/a", "sha1", "MERGEABLE", GateGreen, true)
		c.landingTick(context.Background())
		require.Empty(t, rt.host.merges, string(st))
	}
}
