package autopilot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// fakeEnv is a scriptable Env for preflight tests. By default GitToplevel returns
// the plan file's own directory as the repo, gh auth is OK, and the
// integration branch is absent so preflight auto-creates it.
type fakeEnv struct {
	repoOf        func(dir string) (string, error) // nil ⇒ repo == dir
	defaultBranch string                           // "" ⇒ "main"
	defaultErr    error
	exists        map[string]bool // repo+"\x00"+branch ⇒ true
	existsErr     error
	createErr     error
	ghErr         error
	unknownBE     map[string]bool // backend ids BackendKnown rejects
	coversPRs     bool            // WorkflowsCoverPRs result (gate `auto` resolution)
	coversErr     error

	created []string // "repo|branch|base" audit of CreateBranch calls
}

func (f *fakeEnv) GitToplevel(_ context.Context, dir string) (string, error) {
	if f.repoOf != nil {
		return f.repoOf(dir)
	}
	return dir, nil
}

func (f *fakeEnv) DefaultBranch(_ context.Context, _ string) (string, error) {
	if f.defaultErr != nil {
		return "", f.defaultErr
	}
	if f.defaultBranch != "" {
		return f.defaultBranch, nil
	}
	return "main", nil
}

func (f *fakeEnv) BranchExists(_ context.Context, repo, branch string) (bool, error) {
	if f.existsErr != nil {
		return false, f.existsErr
	}
	return f.exists[repo+"\x00"+branch], nil
}

func (f *fakeEnv) CreateBranch(_ context.Context, repo, branch, base string) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.created = append(f.created, repo+"|"+branch+"|"+base)
	if f.exists == nil {
		f.exists = map[string]bool{}
	}
	f.exists[repo+"\x00"+branch] = true
	return nil
}

func (f *fakeEnv) GHAuthOK(_ context.Context) error { return f.ghErr }

func (f *fakeEnv) BackendKnown(backend string) error {
	if f.unknownBE[backend] {
		return errors.New("brain backend \"" + backend + "\" is not a known agent backend")
	}
	return nil
}

func (f *fakeEnv) WorkflowsCoverPRs(_ context.Context, _, _ string) (bool, error) {
	return f.coversPRs, f.coversErr
}

// writePlan creates a valid plan file under dir and returns its path.
func writePlan(t *testing.T, dir, name, goal string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("version: 1\ngoal: "+goal+"\n"), 0o644))
	return p
}

func TestRunIDStable(t *testing.T) {
	a := RunID("/repo", "/repo/plan.yaml")
	b := RunID("/repo", "/repo/plan.yaml")
	require.Equal(t, a, b, "run_id must be stable for the same repo+plan")
	require.NotEqual(t, a, RunID("/repo", "/repo/other.yaml"))
	require.NotEqual(t, a, RunID("/other", "/repo/plan.yaml"))
	require.True(t, len(a) > 3 && a[:3] == "ap-")
}

func TestEnableHappyPath(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	env := &fakeEnv{}
	c := NewController(ControllerConfig{
		Plans:             []string{plan},
		IntegrationBranch: "autopilot/integration",
		Gate:              "auto",
		BaseDir:           dir,
	}, env)

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.True(t, st.Enabled)
	require.Len(t, st.Runs, 1)
	require.Equal(t, StateActive, st.Runs[0].State)
	// gate `auto` resolves at preflight (§6.1): this fake repo has no workflows
	// covering integration PRs, so it degrades to the local-check gate.
	require.Equal(t, "local", st.Runs[0].Gate)
	require.Equal(t, plan, st.Runs[0].PlanFile)
	require.Nil(t, st.Runs[0].Brain, "no brain in the S1 inert core")
	// integration branch was auto-created off the default branch (per-plan derive)
	require.Len(t, env.created, 1)
	require.Contains(t, env.created[0], "autopilot/plan|main")

	// idempotent: re-enable yields the same run id and does not re-create the branch
	st2, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, st.Runs[0].RunID, st2.Runs[0].RunID)
	require.Len(t, env.created, 1, "branch not re-created when it already exists")
}

func TestEnableGateResolvesToCIWhenWorkflowsCoverPRs(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	// The repo's workflows cover integration PRs, so `auto` resolves to `ci`.
	env := &fakeEnv{coversPRs: true}
	c := NewController(ControllerConfig{
		Plans:             []string{plan},
		IntegrationBranch: "autopilot/integration",
		Gate:              "auto",
		BaseDir:           dir,
	}, env)

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Equal(t, "ci", st.Runs[0].Gate)

	// LandParams exposes the same resolved gate to the daemon land handler.
	lp, ok := c.LandParams(st.Runs[0].RunID)
	require.True(t, ok)
	require.Equal(t, "ci", lp.Gate)
	require.True(t, lp.Active)
	require.Equal(t, "main", lp.DefaultBranch)
}

func TestEnableExplicitGateModesPassThrough(t *testing.T) {
	for _, mode := range []string{"ci", "local"} {
		dir := t.TempDir()
		plan := writePlan(t, dir, "plan.yaml", "g")
		// coversPRs=true would flip `auto` to ci, but explicit modes ignore it.
		env := &fakeEnv{coversPRs: true}
		c := NewController(ControllerConfig{Plans: []string{plan}, Gate: mode, BaseDir: dir}, env)
		st, err := c.Enable(context.Background(), "")
		require.NoError(t, err)
		require.Equal(t, mode, st.Runs[0].Gate)
	}
}

func TestLandParamsUnknownRun(t *testing.T) {
	c := NewController(ControllerConfig{}, &fakeEnv{})
	_, ok := c.LandParams("ap-nope")
	require.False(t, ok)
}

func TestEnableBranchAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "g")
	env := &fakeEnv{exists: map[string]bool{dir + "\x00autopilot/plan": true}}
	c := NewController(ControllerConfig{Plans: []string{plan}, BaseDir: dir}, env)

	_, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Empty(t, env.created, "existing integration branch is not re-created")
}

func TestEnablePreflightFailures(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, dir string) (ControllerConfig, Env)
		wantSub string
	}{
		{
			name: "missing plan file",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				return ControllerConfig{Plans: []string{filepath.Join(dir, "nope.yaml")}, BaseDir: dir}, &fakeEnv{}
			},
			wantSub: "plan file not found",
		},
		{
			name: "invalid plan (unknown field)",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				p := filepath.Join(dir, "plan.yaml")
				require.NoError(t, os.WriteFile(p, []byte("goal: g\nbogus: 1\n"), 0o644))
				return ControllerConfig{Plans: []string{p}, BaseDir: dir}, &fakeEnv{}
			},
			wantSub: "bogus",
		},
		{
			name: "gh not authenticated",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				p := writePlan(t, dir, "plan.yaml", "g")
				return ControllerConfig{Plans: []string{p}, BaseDir: dir}, &fakeEnv{ghErr: errors.New("gh is not authenticated: run gh auth login")}
			},
			wantSub: "not authenticated",
		},
		{
			name: "not a git repo",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				p := writePlan(t, dir, "plan.yaml", "g")
				return ControllerConfig{Plans: []string{p}, BaseDir: dir},
					&fakeEnv{repoOf: func(string) (string, error) { return "", errors.New("not a repo") }}
			},
			wantSub: "not inside a git repository",
		},
		{
			name: "protected integration branch name",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				p := writePlan(t, dir, "plan.yaml", "g")
				return ControllerConfig{Plans: []string{p}, IntegrationBranch: "main", BaseDir: dir}, &fakeEnv{}
			},
			wantSub: "protected name",
		},
		{
			name: "integration branch equals the repo default branch",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				p := writePlan(t, dir, "plan.yaml", "g")
				return ControllerConfig{Plans: []string{p}, IntegrationBranch: "develop", BaseDir: dir},
					&fakeEnv{defaultBranch: "develop"}
			},
			wantSub: "protected name",
		},
		{
			name: "no plans configured",
			setup: func(t *testing.T, dir string) (ControllerConfig, Env) {
				return ControllerConfig{Plans: nil, BaseDir: dir}, &fakeEnv{}
			},
			wantSub: "no plans configured",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg, env := tt.setup(t, dir)
			c := NewController(cfg, env)
			_, err := c.Enable(context.Background(), "")
			require.Error(t, err)
			var pfe *PreflightError
			require.True(t, errors.As(err, &pfe), "want a *PreflightError, got %T", err)
			require.NotEmpty(t, pfe.Failures)
			joined := pfe.Error()
			require.Contains(t, joined, tt.wantSub)
			// a failed enable changes no state
			require.False(t, c.Status().Enabled)
			require.Empty(t, c.Status().Runs)
		})
	}
}

func TestPreflightReportsAllFailuresAtOnce(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "g")
	// gh dead AND a protected integration branch — both should surface in one pass.
	env := &fakeEnv{ghErr: errors.New("gh is not authenticated")}
	c := NewController(ControllerConfig{Plans: []string{plan}, IntegrationBranch: "main", BaseDir: dir}, env)

	_, err := c.Enable(context.Background(), "")
	var pfe *PreflightError
	require.True(t, errors.As(err, &pfe))
	require.GreaterOrEqual(t, len(pfe.Failures), 2, "all failures reported at once, not just the first")
	require.Contains(t, pfe.Error(), "not authenticated")
	require.Contains(t, pfe.Error(), "protected name")
}

func TestDisableKillSwitch(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "g")
	c := NewController(ControllerConfig{Plans: []string{plan}, BaseDir: dir}, &fakeEnv{})

	_, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.True(t, c.Status().Enabled)

	st := c.Disable(context.Background(), "")
	require.False(t, st.Enabled)
	require.Len(t, st.Runs, 1)
	require.Equal(t, StateStopped, st.Runs[0].State)
}

// TestEnableIsPerRepo proves the switch is per-repo: enabling one repo registers
// only its run and leaves another repo's config untouched, Status reports exactly
// which repos are on, and Disable is likewise scoped to one repo.
func TestEnableIsPerRepo(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	planA := writePlan(t, dirA, "plan.yaml", "a")
	planB := writePlan(t, dirB, "plan.yaml", "b")
	// Default fakeEnv: each plan's own dir is its repo, so the two plans are two repos.
	c := NewController(ControllerConfig{
		Plans:   []string{planA, planB},
		BaseDir: dirA,
	}, &fakeEnv{})

	// Enable only repo A.
	st, err := c.Enable(context.Background(), dirA)
	require.NoError(t, err)
	require.True(t, st.Enabled)
	require.Equal(t, []string{dirA}, st.EnabledRepos)
	require.Len(t, st.Runs, 1)
	require.Equal(t, dirA, st.Runs[0].Repo)

	// Enabling repo B adds its run without disturbing A's.
	st, err = c.Enable(context.Background(), dirB)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{dirA, dirB}, st.EnabledRepos)
	require.Len(t, st.Runs, 2)

	// Disabling A is scoped: B keeps running.
	st = c.Disable(context.Background(), dirA)
	require.Equal(t, []string{dirB}, st.EnabledRepos)
	require.Len(t, st.Runs, 2)
	for _, r := range st.Runs {
		if r.Repo == dirA {
			require.Equal(t, StateStopped, r.State)
		}
		if r.Repo == dirB {
			require.Equal(t, StateActive, r.State)
		}
	}
	require.False(t, c.enableStore.IsEnabled(dirA))
	require.True(t, c.enableStore.IsEnabled(dirB))
}

// TestReconfigureSwapsTemplateInPlace proves a config hot-reload swaps the global
// template live: the resolved gate changes on the persisted-enabled repo WITHOUT
// churning its run (same run id — a healthy brain is not respawned).
func TestReconfigureSwapsTemplateInPlace(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	c := NewController(ControllerConfig{Plans: []string{plan}, Gate: "ci", BaseDir: dir}, &fakeEnv{})

	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	require.Equal(t, "ci", st.Runs[0].Gate)
	runID := st.Runs[0].RunID

	// Reload with the gate flipped to local: the run re-resolves in place.
	c.Reconfigure(context.Background(), ControllerConfig{Plans: []string{plan}, Gate: "local", BaseDir: dir})
	st = c.Status()
	require.Len(t, st.Runs, 1)
	require.Equal(t, runID, st.Runs[0].RunID, "a healthy run is not respawned on reconfigure")
	require.Equal(t, "local", st.Runs[0].Gate, "the new gate template applied live")
	require.Equal(t, []string{dir}, st.EnabledRepos, "the persisted enable set is preserved")
}

// TestReconfigureRemovedPlanStopsRun proves deleting an autopilot.plans[] entry on
// reload tears down its run while retaining the stopped durable record.
func TestReconfigureRemovedPlanStopsRun(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	planA := writePlan(t, dirA, "plan.yaml", "a")
	planB := writePlan(t, dirB, "plan.yaml", "b")
	c := NewController(ControllerConfig{Plans: []string{planA, planB}, BaseDir: dirA}, &fakeEnv{})

	_, err := c.Enable(context.Background(), dirA)
	require.NoError(t, err)
	_, err = c.Enable(context.Background(), dirB)
	require.NoError(t, err)
	require.Len(t, c.Status().Runs, 2)

	// Reload with plan B removed from config: B stops, A survives.
	c.Reconfigure(context.Background(), ControllerConfig{Plans: []string{planA}, BaseDir: dirA})
	st := c.Status()
	require.Len(t, st.Runs, 2)
	for _, r := range st.Runs {
		if r.Repo == dirA {
			require.Equal(t, StateActive, r.State)
		}
		if r.Repo == dirB {
			require.Equal(t, StateStopped, r.State)
		}
	}
	// The enable set is not forgotten — re-adding plan B and reloading brings it back.
	c.Reconfigure(context.Background(), ControllerConfig{Plans: []string{planA, planB}, BaseDir: dirA})
	require.Len(t, c.Status().Runs, 2, "re-adding the plan re-registers the still-enabled repo's run")
}

// TestEnableNoPlanForRepo proves enabling a repo with no plan targeting it is a
// clean, actionable failure (not a silent no-op) and changes no state.
func TestEnableNoPlanForRepo(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	planA := writePlan(t, dirA, "plan.yaml", "a")
	c := NewController(ControllerConfig{Plans: []string{planA}, BaseDir: dirA}, &fakeEnv{})

	_, err := c.Enable(context.Background(), dirB)
	var pfe *PreflightError
	require.ErrorAs(t, err, &pfe)
	require.Contains(t, pfe.Error(), "no autopilot plan resolves to "+dirB)
	require.False(t, c.Status().Enabled)
	require.False(t, c.enableStore.IsEnabled(dirB))
}

// TestBootReEnablePersistsAcrossRestart proves a repo enabled with a data-dir
// store comes back up on a fresh controller (the daemon's boot re-enable): the
// persisted set survives, and Enable over it re-registers the run.
func TestBootReEnablePersistsAcrossRestart(t *testing.T) {
	dataDir := t.TempDir()
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	cfg := ControllerConfig{Plans: []string{plan}, BaseDir: dir, DataDir: dataDir}

	c1 := NewController(cfg, &fakeEnv{})
	_, err := c1.Enable(context.Background(), dir)
	require.NoError(t, err)

	// Simulate a daemon restart: a brand-new controller over the same data dir.
	c2 := NewController(cfg, &fakeEnv{})
	require.Equal(t, []string{dir}, c2.PersistedEnabled(), "the enabled set is persisted")
	require.Empty(t, c2.Status().Runs, "runs are not live until boot re-enable runs")

	// Boot re-enable brings the run back up.
	for _, repo := range c2.PersistedEnabled() {
		_, err := c2.Enable(context.Background(), repo)
		require.NoError(t, err)
	}
	st := c2.Status()
	require.True(t, st.Enabled)
	require.Len(t, st.Runs, 1)
	require.Equal(t, dir, st.Runs[0].Repo)
}

// writeCompletePlan creates a plan file already carrying the completion marker.
func writeCompletePlan(t *testing.T, dir, name, goal string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "version: 1\ngoal: " + goal + "\nstatus: complete\ncompleted_at: 2026-07-21T10:00:00Z\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	return p
}

func TestCompleteRunMarksPlanAndSkipsOnReenable(t *testing.T) {
	dir := t.TempDir()
	plan := writePlan(t, dir, "plan.yaml", "ship it")
	c := NewController(ControllerConfig{Plans: []string{plan}, BaseDir: dir}, &fakeEnv{})

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	runID := st.Runs[0].RunID
	require.Equal(t, StateActive, st.Runs[0].State)

	// Complete the run: the plan file gains the in-place marker and the run
	// transitions to complete (kept in status until the next enable reconciles it).
	st2, err := c.CompleteRun(context.Background(), runID)
	require.NoError(t, err)
	require.Len(t, st2.Runs, 1)
	require.Equal(t, StateComplete, st2.Runs[0].State)

	// The plan file now carries a durable, re-parseable completion marker.
	p, err := LoadPlan(plan)
	require.NoError(t, err)
	require.True(t, p.IsComplete())
	require.NotEmpty(t, p.CompletedAt)

	// Idempotent: completing again is a no-op success.
	_, err = c.CompleteRun(context.Background(), runID)
	require.NoError(t, err)

	// Re-enabling skips execution while retaining the terminal durable record.
	st3, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.True(t, st3.Enabled)
	require.Len(t, st3.Runs, 1)
	require.Equal(t, StateComplete, st3.Runs[0].State)
}

func TestCompleteRunUnknownRun(t *testing.T) {
	c := NewController(ControllerConfig{}, &fakeEnv{})
	_, err := c.CompleteRun(context.Background(), "ap-nope")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown run")
}

func TestPreflightSkipsCompletedPlan(t *testing.T) {
	dir := t.TempDir()
	done := writeCompletePlan(t, dir, "done.yaml", "already shipped")
	c := NewController(ControllerConfig{Plans: []string{done}, BaseDir: dir}, &fakeEnv{})

	// A lone completed plan: enable succeeds, registers no run, and is not a failure.
	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.True(t, st.Enabled)
	require.Empty(t, st.Runs)
}

func TestCompletedPlanDoesNotClaimRepoForActiveSibling(t *testing.T) {
	dir := t.TempDir()
	// Two plans in the same dir ⇒ same repo. One is complete, one active. The
	// completed one must NOT trip the one-run-per-repo guard (it is skipped before
	// the repo-conflict check), so the active plan enables cleanly.
	done := writeCompletePlan(t, dir, "done.yaml", "already shipped")
	active := writePlan(t, dir, "active.yaml", "ship the next thing")
	c := NewController(ControllerConfig{Plans: []string{done, active}, BaseDir: dir}, &fakeEnv{})

	st, err := c.Enable(context.Background(), "")
	require.NoError(t, err)
	require.Len(t, st.Runs, 1)
	require.Equal(t, active, st.Runs[0].PlanFile)
}

func TestStatusUnconfiguredDefaults(t *testing.T) {
	c := NewController(ControllerConfig{}, &fakeEnv{})
	st := c.Status()
	require.False(t, st.Enabled)
	require.NotNil(t, st.Runs)
	require.Empty(t, st.Runs)

	// enabling with no plans is a preflight failure, not a panic
	_, err := c.Enable(context.Background(), "")
	require.Error(t, err)
}
