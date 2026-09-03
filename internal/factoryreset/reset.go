// Package factoryreset implements warden's offline data wipe: a scoped reset of
// daemon-owned state back toward a fresh install.
package factoryreset

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/preset"
	"github.com/srjn45/warden/internal/prompttemplate"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/store"
)

// Scope selects how much of warden's on-disk state to remove.
type Scope string

const (
	// ScopeRuntime clears the live fleet and coordination scratch state but keeps
	// archived history, projects, backends, and observability ledgers.
	ScopeRuntime Scope = "runtime"
	// ScopeData wipes every daemon store under data_dir while keeping config-side
	// files (config.yaml, presets, token) unless ScopeFull is used.
	ScopeData Scope = "data"
	// ScopeFull resets data_dir and config-side bootstrap files to a new-install
	// baseline (fresh config.yaml unless KeepConfig is set).
	ScopeFull Scope = "full"
)

// Options controls a factory reset wipe.
type Options struct {
	DataDir      string
	ConfigPath   string
	Scope        Scope
	KeepConfig   bool
	KeepBackends bool
	BackupPath   string
}

// RuntimeDrainer is the daemon API surface used to drain live work before an
// offline wipe. *client.Client satisfies it.
type RuntimeDrainer interface {
	List(ctx context.Context) ([]*store.Session, error)
	History(ctx context.Context, p client.HistoryParams) ([]*store.Session, error)
	Terminate(ctx context.Context, id string) error
	Delete(ctx context.Context, id string, hard bool) error
	RemoveWorktree(ctx context.Context, id string, force, deleteAdoptedBranch bool) error
	PipelineList(ctx context.Context) ([]*pipeline.Pipeline, error)
	PipelineCancel(ctx context.Context, id string) error
	PipelineDelete(ctx context.Context, id string) error
	GetAutopilot(ctx context.Context) (client.AutopilotStatus, error)
	SetAutopilot(ctx context.Context, enabled bool, repo string) (client.AutopilotStatus, error)
	ListAutopilotRuns(ctx context.Context) ([]client.AutopilotRunStatus, error)
	ControlAutopilotRun(ctx context.Context, runID, action string) (client.AutopilotRunStatus, error)
	ScheduleList(ctx context.Context) ([]*schedule.Schedule, error)
	ScheduleDelete(ctx context.Context, id string) error
	Prune(ctx context.Context, p client.PruneParams) ([]lifecycle.PruneResult, error)
}

// Drain stops live agents, pipelines, autopilot runs, and schedules via the
// daemon. pruneWorktrees removes git worktrees for every known session repo when
// true. Errors on individual drain steps are logged to out and collected; the
// first error is returned after best-effort drain completes.
func Drain(ctx context.Context, d RuntimeDrainer, pruneWorktrees bool, out io.Writer) error {
	if d == nil {
		return errors.New("factory reset drain: nil client")
	}
	var errs []error
	logf := func(format string, args ...any) {
		if out != nil {
			fmt.Fprintf(out, format, args...)
		}
	}

	// Pipelines first so jobs stop spawning agents.
	pipes, err := d.PipelineList(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list pipelines: %w", err))
	} else {
		for _, p := range pipes {
			if p == nil {
				continue
			}
			_ = d.PipelineCancel(ctx, p.ID)
			if err := d.PipelineDelete(ctx, p.ID); err != nil {
				errs = append(errs, fmt.Errorf("delete pipeline %s: %w", p.ID, err))
			} else {
				logf("removed pipeline %s\n", p.ID)
			}
		}
	}

	// Autopilot: stop runs, then disable every enabled repo.
	if runs, err := d.ListAutopilotRuns(ctx); err != nil {
		errs = append(errs, fmt.Errorf("list autopilot runs: %w", err))
	} else {
		for _, r := range runs {
			if _, err := d.ControlAutopilotRun(ctx, r.RunID, "stop"); err != nil {
				errs = append(errs, fmt.Errorf("stop autopilot run %s: %w", r.RunID, err))
			}
			if _, err := d.ControlAutopilotRun(ctx, r.RunID, "unregister"); err != nil {
				errs = append(errs, fmt.Errorf("unregister autopilot run %s: %w", r.RunID, err))
			} else {
				logf("removed autopilot run %s\n", r.RunID)
			}
		}
	}
	if st, err := d.GetAutopilot(ctx); err != nil {
		errs = append(errs, fmt.Errorf("autopilot status: %w", err))
	} else {
		for _, repo := range st.EnabledRepos {
			if _, err := d.SetAutopilot(ctx, false, repo); err != nil {
				errs = append(errs, fmt.Errorf("disable autopilot for %s: %w", repo, err))
			} else {
				logf("disabled autopilot for %s\n", repo)
			}
		}
	}

	scheds, err := d.ScheduleList(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list schedules: %w", err))
	} else {
		for _, sc := range scheds {
			if sc == nil {
				continue
			}
			if err := d.ScheduleDelete(ctx, sc.ID); err != nil {
				errs = append(errs, fmt.Errorf("delete schedule %s: %w", sc.ID, err))
			} else {
				logf("removed schedule %s\n", sc.ID)
			}
		}
	}

	active, err := d.List(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("list agents: %w", err))
	}
	var closed []*store.Session
	if pruneWorktrees {
		closed, _ = d.History(ctx, client.HistoryParams{})
	}

	repos := collectRepos(active, closed)
	for _, s := range active {
		if s == nil {
			continue
		}
		id := s.ID
		_ = d.Terminate(ctx, id)
		if err := d.Delete(ctx, id, true); err != nil {
			errs = append(errs, fmt.Errorf("purge agent %s: %w", id, err))
			continue
		}
		logf("purged agent %s\n", id)
		if pruneWorktrees && s.Worktree != "" {
			if err := d.RemoveWorktree(ctx, id, true, true); err != nil {
				errs = append(errs, fmt.Errorf("remove worktree for %s: %w", id, err))
			}
		}
	}

	if pruneWorktrees {
		for repo := range repos {
			if _, err := d.Prune(ctx, client.PruneParams{
				Repo:            repo,
				Force:           true,
				IncludeArchived: true,
			}); err != nil {
				errs = append(errs, fmt.Errorf("prune worktrees in %s: %w", repo, err))
			} else {
				logf("pruned orphan worktrees in %s\n", repo)
			}
		}
	}

	return errors.Join(errs...)
}

func collectRepos(active, closed []*store.Session) map[string]struct{} {
	out := map[string]struct{}{}
	for _, list := range [][]*store.Session{active, closed} {
		for _, s := range list {
			if s == nil {
				continue
			}
			repo := strings.TrimSpace(s.Repo)
			if repo == "" {
				continue
			}
			out[repo] = struct{}{}
		}
	}
	return out
}

// RelPaths returns data_dir-relative paths removed for scope. keepBackends only
// applies to data/full scopes. Paths are sorted for stable tests.
func RelPaths(scope Scope, keepBackends bool) []string {
	var paths []string
	switch scope {
	case ScopeRuntime:
		paths = []string{
			"sessions-db/active",
			"pipelines-db",
			"pipelines",
			".pipelines-filedb-imported",
			"context",
			"inbox",
			"prompts",
			"hints",
			"settings",
			"exits",
		}
	case ScopeData:
		paths = dataPaths(keepBackends)
	case ScopeFull:
		paths = dataPaths(keepBackends)
	default:
		return nil
	}
	sort.Strings(paths)
	return paths
}

func dataPaths(keepBackends bool) []string {
	paths := []string{
		"sessions-db",
		"sessions",
		"closed",
		".sessions-filedb-imported",
		".sessions-store.lock",
		"pipelines-db",
		"pipelines",
		".pipelines-filedb-imported",
		"projects",
		"context",
		"inbox",
		"autopilot",
		"schedules-db",
		".schedules-filedb-imported",
		"snapshots-db",
		"snapshots",
		".snapshots-filedb-imported",
		"prompts",
		"hints",
		"settings",
		"exits",
		"metrics",
		"savings",
		"spend",
		"audit.jsonl",
		"ratelimit-captures",
		"repair-backups",
		".provenance-migrated",
		".worktrees",
		"addr",
		"token.env",
		"tutorial-complete",
		"orch_history",
	}
	if !keepBackends {
		paths = append(paths, "backends")
	}
	return paths
}

// Wipe removes on-disk state for opts.Scope. The session store must be offline
// (daemon stopped) — callers should take the store lock via store.WithOfflineSessionStore.
func Wipe(opts Options) error {
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		return errors.New("factory reset: empty data_dir")
	}
	scope := opts.Scope
	if scope != ScopeRuntime && scope != ScopeData && scope != ScopeFull {
		return fmt.Errorf("factory reset: unknown scope %q", scope)
	}

	for _, rel := range RelPaths(scope, opts.KeepBackends) {
		target := filepath.Join(dataDir, rel)
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}

	if scope == ScopeFull {
		if err := wipeConfigSide(opts); err != nil {
			return err
		}
	}
	return nil
}

func wipeConfigSide(opts Options) error {
	if !opts.KeepConfig {
		cfgPath := opts.ConfigPath
		if cfgPath == "" {
			cfgPath = config.DefaultPath()
		}
		if err := config.WriteFresh(cfgPath); err != nil {
			return fmt.Errorf("reset config: %w", err)
		}
	}
	cfgDir := filepath.Dir(opts.ConfigPath)
	if cfgDir == "" || cfgDir == "." {
		cfgDir = filepath.Dir(config.DefaultPath())
	}
	for _, name := range []string{
		filepath.Base(preset.DefaultPath()),
		filepath.Base(prompttemplate.DefaultPath()),
	} {
		_ = os.Remove(filepath.Join(cfgDir, name))
	}
	return nil
}

// Execute runs backup (optional), offline wipe, and returns. The caller must
// drain live state and stop the daemon before calling Execute when a running
// hub holds the session-store lock.
func Execute(opts Options) error {
	dataDir := strings.TrimSpace(opts.DataDir)
	if dataDir == "" {
		return errors.New("factory reset: empty data_dir")
	}
	if opts.BackupPath != "" {
		if err := backupTree(dataDir, opts.BackupPath); err != nil {
			return err
		}
	}
	return store.WithOfflineSessionStore(dataDir, func() error {
		return Wipe(opts)
	})
}
