package autopilot

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSanitizeBranchComponent(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain", in: "alpha", want: "alpha"},
		{name: "dots kept", in: "foo.bar", want: "foo.bar"},
		{name: "trailing lock", in: "foo.lock", want: "foo-lock"},
		{name: "trailing dot", in: "foo.", want: "foo"},
		{name: "leading dot", in: ".hidden", want: "hidden"},
		{name: "collapsed dots", in: "foo..bar", want: "foo.bar"},
		{name: "all dots", in: "..", wantErr: true},
		{name: "nested slash", in: "foo/bar", wantErr: true},
		{name: "empty", in: " ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeBranchComponent(tt.in)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestResolveIntegrationBranchPrecedence(t *testing.T) {
	t.Run("derived default from empty template", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{planName: "ship"})
		require.NoError(t, err)
		require.Equal(t, "autopilot/ship", got)
	})
	t.Run("derived default from legacy global", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{
			planName: "ship", template: DefaultIntegrationBranch,
		})
		require.NoError(t, err)
		require.Equal(t, "autopilot/ship", got)
	})
	t.Run("plan template expands", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{
			planName: "ship", template: "integration/{{plan}}",
		})
		require.NoError(t, err)
		require.Equal(t, "integration/ship", got)
	})
	t.Run("custom global override", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{
			planName: "ship", template: "ap/int",
		})
		require.NoError(t, err)
		require.Equal(t, "ap/int", got)
	})
	t.Run("stored value grandfathered", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{
			planName: "ship", template: "integration/{{plan}}", stored: DefaultIntegrationBranch,
		})
		require.NoError(t, err)
		require.Equal(t, DefaultIntegrationBranch, got)
	})
	t.Run("sanitizer collision disambiguates with run_id suffix", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{
			planName: "foo", runID: "ap-abcdef123456",
			taken: func(b string) bool { return b == "autopilot/foo" },
		})
		require.NoError(t, err)
		require.Equal(t, "autopilot/foo_abcdef", got)
	})
	t.Run("custom global is not disambiguated", func(t *testing.T) {
		got, err := resolveIntegrationBranch(branchResolveOpts{
			planName: "ship", template: "ap/int", runID: "ap-abcdef123456",
			taken: func(string) bool { return true },
		})
		require.NoError(t, err)
		require.Equal(t, "ap/int", got)
	})
}

func TestTwoPlansInOneRepoResolveDistinctBranches(t *testing.T) {
	dir := t.TempDir()
	p1 := writePlan(t, dir, "alpha.yaml", "alpha goal")
	p2 := writePlan(t, dir, "beta.yaml", "beta goal")
	runStore, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = runStore.Close() })

	c := NewController(ControllerConfig{
		Plans:             []string{p1, p2},
		IntegrationBranch: DefaultIntegrationBranch,
		BaseDir:           dir,
		RunStore:          runStore,
		Resolver:          &fakeResolver{backendID: "a", tier: "free"},
	}, &fakeEnv{})
	c.SetRuntime(newFakeRuntime())

	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, st.Runs, 2)

	branches := map[string]string{}
	for _, r := range st.Runs {
		lp, ok := c.LandParams(r.RunID)
		require.True(t, ok)
		require.NotEqual(t, DefaultIntegrationBranch, lp.IntegrationBranch)
		branches[r.RunID] = lp.IntegrationBranch

		rec, err := runStore.Get(r.RunID)
		require.NoError(t, err)
		require.Equal(t, lp.IntegrationBranch, rec.IntegrationBranch)
	}
	require.Len(t, branches, 2)
	require.NotEqual(t, branches[st.Runs[0].RunID], branches[st.Runs[1].RunID])
	require.ElementsMatch(t, []string{"autopilot/alpha", "autopilot/beta"},
		[]string{branches[st.Runs[0].RunID], branches[st.Runs[1].RunID]})
}

func TestRegisterPersistsDerivedBranchAndLandUsesStoredValue(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "ship.yaml", "ship")
	runStore, err := NewRunStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = runStore.Close() })
	c := NewController(ControllerConfig{BaseDir: repo, RunStore: runStore}, &fakeEnv{})

	st, err := c.Register(context.Background(), RegisterRequest{Name: "ship", PlanFile: plan})
	require.NoError(t, err)
	rec, err := runStore.Get(st.RunID)
	require.NoError(t, err)
	require.Equal(t, "autopilot/ship", rec.IntegrationBranch)

	_, err = c.StartRun(context.Background(), st.RunID)
	require.NoError(t, err)
	lp, ok := c.LandParams(st.RunID)
	require.True(t, ok)
	require.Equal(t, "autopilot/ship", lp.IntegrationBranch)
}

func TestGrandfatherPreservesAutopilotIntegration(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "legacy.yaml", "legacy")
	data := t.TempDir()
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}

	s1, err := NewRunStore(data)
	require.NoError(t, err)
	now := time.Now().UTC()
	id := RunID(repo, plan)
	require.NoError(t, s1.Create(RunRecord{
		RunID:             id,
		Name:              "legacy",
		Repo:              repo,
		PlanFile:          plan,
		State:             StateRegistered,
		IntegrationBranch: DefaultIntegrationBranch,
		CreatedAt:         now,
		UpdatedAt:         now,
	}))
	require.NoError(t, s1.Close())

	c := NewController(ControllerConfig{DataDir: data, BaseDir: repo, IntegrationBranch: DefaultIntegrationBranch}, env)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	require.Equal(t, DefaultIntegrationBranch, c.runs[id].integrationBranch)
	lp, ok := c.LandParams(id)
	require.True(t, ok)
	require.Equal(t, DefaultIntegrationBranch, lp.IntegrationBranch)

	_, err = c.StartRun(context.Background(), id)
	require.NoError(t, err)
	rec, err := c.store.Get(id)
	require.NoError(t, err)
	require.Equal(t, DefaultIntegrationBranch, rec.IntegrationBranch, "restart/start must not re-derive grandfathered records")
}

func TestRegisterSanitizerNames(t *testing.T) {
	repo := t.TempDir()
	lockPlan := writePlan(t, repo, "lock.yaml", "lock")
	dotPlan := writePlan(t, repo, "dot.yaml", "dot")
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, &fakeEnv{})
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	st, err := c.Register(context.Background(), RegisterRequest{Name: "foo.lock", PlanFile: lockPlan})
	require.NoError(t, err)
	require.Equal(t, "autopilot/foo-lock", c.runs[st.RunID].integrationBranch)

	st, err = c.Register(context.Background(), RegisterRequest{Name: "foo.", PlanFile: dotPlan})
	require.NoError(t, err)
	require.Equal(t, "autopilot/foo", c.runs[st.RunID].integrationBranch)
}

func TestRegisterRejectsNestedPlanName(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "nested.yaml", "nested")
	c := NewController(ControllerConfig{BaseDir: repo}, &fakeEnv{})
	_, err := c.Register(context.Background(), RegisterRequest{Name: "foo/bar", PlanFile: plan})
	require.Error(t, err)
	require.ErrorContains(t, err, "nested integration branches")
}

func TestCustomGlobalCollisionWarns(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	dir := t.TempDir()
	p1 := writePlan(t, dir, "alpha.yaml", "alpha")
	p2 := writePlan(t, dir, "beta.yaml", "beta")
	c := NewController(ControllerConfig{
		Plans:             []string{p1, p2},
		IntegrationBranch: "ap/shared",
		BaseDir:           dir,
		RunStore:          func() *RunStore { s, err := NewRunStore(t.TempDir()); require.NoError(t, err); return s }(),
	}, &fakeEnv{})

	st, err := c.Enable(context.Background(), dir)
	require.NoError(t, err)
	require.Len(t, st.Runs, 2)
	lp1, _ := c.LandParams(st.Runs[0].RunID)
	lp2, _ := c.LandParams(st.Runs[1].RunID)
	require.Equal(t, "ap/shared", lp1.IntegrationBranch)
	require.Equal(t, "ap/shared", lp2.IntegrationBranch)
	require.Contains(t, buf.String(), "same integration branch")
}

func TestPlanTemplateOnRegister(t *testing.T) {
	repo := t.TempDir()
	plan := writePlan(t, repo, "release.yaml", "rel")
	c := NewController(ControllerConfig{
		BaseDir: repo, IntegrationBranch: "integration/{{plan}}",
	}, &fakeEnv{})
	st, err := c.Register(context.Background(), RegisterRequest{Name: "release", PlanFile: plan})
	require.NoError(t, err)
	require.Equal(t, "integration/release", c.runs[st.RunID].integrationBranch)
}
