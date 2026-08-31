package autopilot

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyPlansCopiesRegistersAndIsIdempotent(t *testing.T) {
	repo := t.TempDir()
	legacy := filepath.Join(repo, "autopilot.plan.yaml")
	require.NoError(t, os.WriteFile(legacy, []byte("version: 1\ngoal: migrate me\n"), 0o644))
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}
	c := NewController(ControllerConfig{DataDir: t.TempDir(), BaseDir: repo}, env)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	var out bytes.Buffer
	plans, err := MigrateLegacyPlans(context.Background(), env, c, nil, repo, &out)
	require.NoError(t, err)
	dst := filepath.Join(repo, "plans", "default.yaml")
	require.FileExists(t, dst)
	require.FileExists(t, legacy) // compatibility source is retained
	require.Contains(t, out.String(), "deprecated autopilot plan")
	require.Len(t, c.Status().Runs, 1)
	require.Equal(t, "default", c.Status().Runs[0].Name)
	require.Equal(t, []string{dst}, plans)

	out.Reset()
	plans, err = MigrateLegacyPlans(context.Background(), env, c, []string{"autopilot.plan.yaml"}, repo, &out)
	require.NoError(t, err)
	require.Equal(t, []string{dst}, plans)
	require.Len(t, c.Status().Runs, 1)
}

func TestMigrateLegacyPlansDoesNotOverwriteDifferentDestination(t *testing.T) {
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, "autopilot.plan.yaml"), []byte("version: 1\ngoal: old\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "plans"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "plans", "default.yaml"), []byte("version: 1\ngoal: new\n"), 0o644))
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}
	c := NewController(ControllerConfig{}, env)
	_, err := MigrateLegacyPlans(context.Background(), env, c, []string{"autopilot.plan.yaml"}, repo, &bytes.Buffer{})
	require.ErrorContains(t, err, "different contents")
}

func TestMigrateLegacyPlansRelocatesStoredLegacyRun(t *testing.T) {
	repo, dataDir := t.TempDir(), t.TempDir()
	legacy := filepath.Join(repo, "autopilot.plan.yaml")
	require.NoError(t, os.WriteFile(legacy, []byte("version: 1\ngoal: resume me\n"), 0o644))
	env := &fakeEnv{repoOf: func(string) (string, error) { return repo, nil }}
	first := NewController(ControllerConfig{DataDir: dataDir}, env)
	_, err := first.Register(context.Background(), RegisterRequest{Name: "default", Repo: repo, PlanFile: legacy})
	require.NoError(t, err)
	require.NoError(t, first.Close())

	second := NewController(ControllerConfig{DataDir: dataDir}, env)
	t.Cleanup(func() { require.NoError(t, second.Close()) })
	plans, err := MigrateLegacyPlans(context.Background(), env, second, []string{legacy}, repo, &bytes.Buffer{})
	require.NoError(t, err)
	require.Equal(t, []string{filepath.Join(repo, "plans", "default.yaml")}, plans)
	require.Len(t, second.Status().Runs, 1)
	require.Equal(t, RunID(repo, plans[0]), second.Status().Runs[0].RunID)
}
