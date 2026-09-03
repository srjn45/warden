package factoryreset

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srjn45/warden/internal/auth"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/store"
	"github.com/stretchr/testify/require"
)

func TestRelPathsScopes(t *testing.T) {
	runtime := RelPaths(ScopeRuntime, false)
	require.Contains(t, runtime, "sessions-db/active")
	require.NotContains(t, runtime, "sessions-db/closed")
	require.NotContains(t, runtime, "projects")

	data := RelPaths(ScopeData, true)
	require.Contains(t, data, "sessions-db")
	require.Contains(t, data, "projects")
	require.NotContains(t, data, "backends")

	dataNoBackends := RelPaths(ScopeData, false)
	require.Contains(t, dataNoBackends, "backends")
}

// TestRelPathsDataKeepsToken guards against token.env sitting in the shared
// dataPaths() list: scope=data must keep it (per the ScopeData doc comment and
// the factory-reset CLI help), only scope=full removes it, and it does so via
// auth.DefaultTokenFile() in wipeConfigSide rather than a dataDir-relative path.
func TestRelPathsDataKeepsToken(t *testing.T) {
	require.NotContains(t, RelPaths(ScopeData, true), "token.env")
	require.NotContains(t, RelPaths(ScopeFull, true), "token.env")
}

func TestWipeDataKeepsTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token.env")
	require.NoError(t, os.WriteFile(tokPath, []byte("WARDEN_TOKEN=abc\n"), 0o600))

	require.NoError(t, Wipe(Options{DataDir: dir, Scope: ScopeData}))

	require.FileExists(t, tokPath)
}

func TestWipeFullRemovesTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tokPath := auth.DefaultTokenFile()
	require.NoError(t, os.MkdirAll(filepath.Dir(tokPath), 0o700))
	require.NoError(t, os.WriteFile(tokPath, []byte("WARDEN_TOKEN=abc\n"), 0o600))

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("addr: localhost:9999\n"), 0o644))

	require.NoError(t, Wipe(Options{
		DataDir:    dir,
		ConfigPath: cfgPath,
		Scope:      ScopeFull,
	}))

	require.NoFileExists(t, tokPath)
}

func TestWipeRuntimeKeepsClosedAndProjects(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sessions-db", "closed"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "projects", "projects"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sessions-db", "active"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sessions-db", "active", "keep"), []byte("x"), 0o600))

	require.NoError(t, Wipe(Options{DataDir: dir, Scope: ScopeRuntime}))

	_, err := os.Stat(filepath.Join(dir, "sessions-db", "closed"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "projects"))
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(dir, "sessions-db", "active", "keep"))
}

func TestWipeFullResetsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("addr: localhost:9999\n"), 0o644))
	presets := filepath.Join(dir, "presets.yaml")
	require.NoError(t, os.WriteFile(presets, []byte("x: {}\n"), 0o644))

	require.NoError(t, Wipe(Options{
		DataDir:    dir,
		ConfigPath: cfgPath,
		Scope:      ScopeFull,
	}))

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	require.NotContains(t, string(data), "localhost:9999")
	require.NoFileExists(t, presets)
}

func TestExecuteRequiresOfflineStore(t *testing.T) {
	dir := t.TempDir()
	fs, err := store.NewFileStore(dir)
	require.NoError(t, err)
	defer fs.Close(context.Background())

	err = Execute(Options{DataDir: dir, Scope: ScopeRuntime})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already owned")
}

func TestExecuteWithBackup(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "metrics"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "metrics", "marker"), []byte("1"), 0o600))
	backup := filepath.Join(t.TempDir(), "backup")

	require.NoError(t, Execute(Options{
		DataDir:    dir,
		Scope:      ScopeData,
		BackupPath: backup,
	}))

	require.NoFileExists(t, filepath.Join(dir, "metrics", "marker"))
	require.FileExists(t, filepath.Join(backup, "metrics", "marker"))
}

type fakeDrainer struct {
	pipelines []*pipeline.Pipeline
	agents    []*store.Session
	repos     []string
}

func (f *fakeDrainer) List(context.Context) ([]*store.Session, error) { return f.agents, nil }
func (f *fakeDrainer) History(context.Context, client.HistoryParams) ([]*store.Session, error) {
	return nil, nil
}
func (f *fakeDrainer) Terminate(context.Context, string) error                  { return nil }
func (f *fakeDrainer) Delete(context.Context, string, bool) error               { return nil }
func (f *fakeDrainer) RemoveWorktree(context.Context, string, bool, bool) error { return nil }
func (f *fakeDrainer) PipelineList(context.Context) ([]*pipeline.Pipeline, error) {
	return f.pipelines, nil
}
func (f *fakeDrainer) PipelineCancel(context.Context, string) error { return nil }
func (f *fakeDrainer) PipelineDelete(context.Context, string) error { return nil }
func (f *fakeDrainer) GetAutopilot(context.Context) (client.AutopilotStatus, error) {
	return client.AutopilotStatus{EnabledRepos: f.repos}, nil
}
func (f *fakeDrainer) SetAutopilot(context.Context, bool, string) (client.AutopilotStatus, error) {
	return client.AutopilotStatus{}, nil
}
func (f *fakeDrainer) ListAutopilotRuns(context.Context) ([]client.AutopilotRunStatus, error) {
	return nil, nil
}
func (f *fakeDrainer) ControlAutopilotRun(context.Context, string, string) (client.AutopilotRunStatus, error) {
	return client.AutopilotRunStatus{}, nil
}
func (f *fakeDrainer) ScheduleList(context.Context) ([]*schedule.Schedule, error) { return nil, nil }
func (f *fakeDrainer) ScheduleDelete(context.Context, string) error               { return nil }
func (f *fakeDrainer) Prune(context.Context, client.PruneParams) ([]lifecycle.PruneResult, error) {
	return nil, nil
}

func TestDrainPurgesAgentsAndPipelines(t *testing.T) {
	d := &fakeDrainer{
		pipelines: []*pipeline.Pipeline{{ID: "p1"}},
		agents:    []*store.Session{{ID: "a1"}},
	}
	require.NoError(t, Drain(context.Background(), d, false, nil))
}

func TestConfigWriteFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, config.WriteFresh(path))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "addr:")
}
