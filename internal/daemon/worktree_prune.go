package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// worktreeSweepInterval is the slow cadence for the unattended orphan sweep
// (worktree_auto_prune). Reclaiming a clean, record-less checkout is cheap and
// non-urgent, so an hourly pass plus the startup pass is plenty.
const worktreeSweepInterval = time.Hour

// runWorktreePruneSweep runs the unattended worktree GC loop (enabled by
// worktree_auto_prune). It sweeps once at startup, then on a slow ticker, until
// ctx is cancelled. Each pass reconciles every repo warden has a record for and
// reclaims clean, record-less orphans only.
//
// INVARIANT: this unattended path NEVER reclaims an archived-owned worktree —
// it always passes IncludeArchived=false and Force=false, so archived owners are
// kept and dirty/unpushed orphans are skipped. Reclaiming an archived worktree
// (the gitignored-files blind spot) is a manual, interactive
// `warden prune --include-archived` only.
func (s *Server) runWorktreePruneSweep(ctx context.Context) {
	s.sweepWorktreesOnce(ctx) // startup pass
	t := time.NewTicker(worktreeSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepWorktreesOnce(ctx)
		}
	}
}

// sweepWorktreesOnce runs one guarded prune pass across every repo warden tracks
// a worktree for. Per-repo failures are logged and never abort the sweep.
func (s *Server) sweepWorktreesOnce(ctx context.Context) {
	active, err := s.store.List(ctx)
	if err != nil {
		slog.Warn("worktree auto-prune: list active failed", "err", err)
		return
	}
	archived, err := s.store.ListClosed(ctx)
	if err != nil {
		slog.Warn("worktree auto-prune: list archived failed", "err", err)
		return
	}
	for _, repo := range worktreeRepos(active, archived) {
		results, err := s.life.PruneWorktrees(ctx, repo, lifecycle.PruneOpts{
			DryRun:          false,
			Force:           false, // dirty/unpushed always require manual --force
			IncludeArchived: false, // INVARIANT: never sweep archived-owned worktrees unattended
			Active:          active,
			Archived:        archived,
		})
		if err != nil {
			slog.Warn("worktree auto-prune: prune failed", "repo", repo, "err", err)
			continue
		}
		removed := 0
		for _, r := range results {
			if r.Action == lifecycle.PruneRemove {
				removed++
			}
		}
		if removed > 0 {
			slog.Info("worktree auto-prune: reclaimed orphan worktrees", "repo", repo, "count", removed)
		}
	}
}

// worktreeRepos returns the distinct, non-empty repo paths of every session that
// owns a worktree (active + archived). These are the repos whose .worktrees the
// sweep reconciles; archived repos are included so a record-less orphan in a repo
// with only archived sessions is still reached (archived owners stay protected by
// IncludeArchived=false).
func worktreeRepos(active, archived []*store.Session) []string {
	seen := map[string]bool{}
	var out []string
	add := func(sessions []*store.Session) {
		for _, sess := range sessions {
			if sess.Repo == "" || sess.Worktree == "" || seen[sess.Repo] {
				continue
			}
			seen[sess.Repo] = true
			out = append(out, sess.Repo)
		}
	}
	add(active)
	add(archived)
	return out
}
