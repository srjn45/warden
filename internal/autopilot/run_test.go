package autopilot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBackendLadderHelpers(t *testing.T) {
	ladder := BackendLadder{
		Free:         []string{"", "antigravity", "codex"},
		Subscription: []string{"claude"},
		PayPerUse:    []string{"gpt"},
	}
	require.Equal(t, "antigravity", ladder.firstFree(), "first non-blank free backend")
	require.Equal(t, []string{"antigravity", "codex", "claude", "gpt"}, ladder.all(), "union in tier order, blanks skipped")
}

func TestReloadPlanIfChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.yaml")
	require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: first\n"), 0o644))

	base := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, base, base))
	info, _ := os.Stat(path)

	r := &run{runID: "ap-run", absPlanFile: path, planModTime: info.ModTime()}
	first, err := LoadPlan(path)
	require.NoError(t, err)
	r.plan = first

	// No mtime change ⇒ no reload.
	changed, notify := r.reloadPlanIfChanged()
	require.False(t, changed)
	require.NoError(t, notify)

	// A valid newer edit is adopted.
	require.NoError(t, os.WriteFile(path, []byte("version: 1\ngoal: second\n"), 0o644))
	newer := base.Add(10 * time.Minute)
	require.NoError(t, os.Chtimes(path, newer, newer))
	changed, notify = r.reloadPlanIfChanged()
	require.True(t, changed)
	require.NoError(t, notify)
	require.Equal(t, "second", r.plan.Goal)

	// A corrupt newer edit keeps the last-good plan and reports the error (§3):
	// a steering typo must never wedge the run.
	require.NoError(t, os.WriteFile(path, []byte("goal: third\nbogus: 1\n"), 0o644))
	corrupt := newer.Add(10 * time.Minute)
	require.NoError(t, os.Chtimes(path, corrupt, corrupt))
	changed, notify = r.reloadPlanIfChanged()
	require.False(t, changed)
	require.Error(t, notify)
	require.Equal(t, "second", r.plan.Goal, "run keeps the last-good plan")

	// The corrupt mtime is recorded, so it is reported once, not every tick.
	changed, notify = r.reloadPlanIfChanged()
	require.False(t, changed)
	require.NoError(t, notify)
}

// fakeRuntime is a scriptable autopilot.Runtime for controller brain-lifecycle
// tests: it records spawns/terminations and serves an in-memory ledger + digest
// sources.
type fakeRuntime struct {
	store    *fakeStore
	sources  fakeSources
	spawned  []BrainSpec
	killed   []string
	notified []string
	spawnErr error
	nextID   int
	installs int // times InstallDefaultAutoApprovePolicy was called
}

func newFakeRuntime() *fakeRuntime { return &fakeRuntime{store: newFakeStore()} }

func (r *fakeRuntime) SpawnBrain(_ context.Context, spec BrainSpec) (BrainHandle, error) {
	if r.spawnErr != nil {
		return BrainHandle{}, r.spawnErr
	}
	r.spawned = append(r.spawned, spec)
	r.nextID++
	return BrainHandle{AgentID: fmt.Sprintf("brain-%d", r.nextID), Backend: spec.Backend}, nil
}

func (r *fakeRuntime) TerminateBrain(_ context.Context, agentID string) error {
	r.killed = append(r.killed, agentID)
	return nil
}

func (r *fakeRuntime) NewLedger(runID string) *Ledger   { return NewLedger(r.store, runID) }
func (r *fakeRuntime) DigestSources() DigestSources     { return r.sources }
func (r *fakeRuntime) NotifyOwner(_ string, msg string) { r.notified = append(r.notified, msg) }
func (r *fakeRuntime) InstallDefaultAutoApprovePolicy() { r.installs++ }

func TestControllerSpawnsAndTearsDownBrain(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		Plans:             []string{plan},
		IntegrationBranch: "autopilot/integration",
		BaseDir:           dir,
		Resolver:          &fakeResolver{backendID: "antigravity", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	require.Equal(t, StateActive, st.Runs[0].State)

	// A real brain was spawned: on the first free backend, headless, tagged
	// autopilot + run:<run_id>, with the digest as its opening brief.
	require.Len(t, rt.spawned, 1)
	spec := rt.spawned[0]
	require.Equal(t, "antigravity", spec.Backend)
	require.True(t, spec.Headless)
	require.Contains(t, spec.Tags, autopilotTag)
	require.Contains(t, spec.Tags, runTag(st.Runs[0].RunID))
	require.Contains(t, spec.Prompt, "ship it", "opening brief is the recovery digest")

	// Status reflects the brain.
	require.NotNil(t, st.Runs[0].Brain)
	require.Equal(t, "brain-1", st.Runs[0].Brain.AgentID)
	require.Equal(t, "antigravity", st.Runs[0].Brain.Backend)

	// Idempotent re-enable does not kill/respawn the healthy brain.
	_, err = c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, rt.spawned, 1, "healthy brain not respawned on re-enable")
	require.Empty(t, rt.killed)

	// Disable is the kill switch: the brain is terminated.
	dst := c.Disable(context.Background(), "")
	require.False(t, dst.Enabled)
	require.Len(t, dst.Runs, 1)
	require.Equal(t, StateStopped, dst.Runs[0].State)
	require.Equal(t, []string{"brain-1"}, rt.killed)
}

// TestControllerInstallsDefaultPolicyOnEnable proves enabling autopilot installs
// the generous default auto-approve policy (§10) through the runtime seam, so
// day-one workers don't stall. The seam owns the "owner has no rules" check; the
// Controller just invokes it once per Enable.
func TestControllerInstallsDefaultPolicyOnEnable(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		Plans:    []string{plan},
		BaseDir:  dir,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)

	_, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, 1, rt.installs, "enabling autopilot installs the default auto-approve policy")

	c.Disable(context.Background(), "")
}

// TestActiveBrainForRun proves the approval router's ownership lookup: the run's
// brain is resolvable while enabled and not after the kill switch (§8).
func TestActiveBrainForRun(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	c := NewController(ControllerConfig{
		Plans:    []string{plan},
		BaseDir:  dir,
		Resolver: &fakeResolver{backendID: "antigravity", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(rt)

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	runID := st.Runs[0].RunID

	brainID, ok := c.ActiveBrainForRun(runID)
	require.True(t, ok)
	require.Equal(t, "brain-1", brainID)

	_, ok = c.ActiveBrainForRun("ap-nonexistent")
	require.False(t, ok, "an unknown run has no brain")

	c.Disable(context.Background(), "")
	_, ok = c.ActiveBrainForRun(runID)
	require.False(t, ok, "a disabled autopilot resolves no brain")
}

func TestControllerBrainSpawnFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	rt := newFakeRuntime()
	rt.spawnErr = fmt.Errorf("backend unavailable")
	c := NewController(ControllerConfig{Plans: []string{plan}, BaseDir: dir}, &fakeEnv{})
	c.SetRuntime(rt)

	// A spawn failure does not fail the whole switch — the run is degraded and the
	// guardian (S5) heals it.
	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	require.Equal(t, StateDegraded, st.Runs[0].State)
	require.Nil(t, st.Runs[0].Brain)

	c.Disable(context.Background(), "")
}
