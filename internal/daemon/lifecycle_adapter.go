package daemon

import (
	"context"

	"github.com/srajanpathak/agentctl/internal/lifecycle"
	"github.com/srajanpathak/agentctl/internal/store"
)

// lifecycleAdapter combines subprocess lifecycle ops with the store so the
// daemon Lifecycle interface is satisfied by one object.
type lifecycleAdapter struct {
	lc    *lifecycle.Lifecycle
	store store.Store
}

func NewLifecycleAdapter(lc *lifecycle.Lifecycle, st store.Store) Lifecycle {
	return &lifecycleAdapter{lc: lc, store: st}
}

// Spawn translates the daemon's wire DTO into the lifecycle request, normalizing
// the type so unknown types collapse to "other" (no worktree).
func (a *lifecycleAdapter) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	return a.lc.Spawn(ctx, lifecycle.SpawnRequest{
		Type:     store.NormalizeType(req.Type),
		Ticket:   req.Ticket,
		Repo:     req.Repo,
		Branch:   req.Branch,
		PR:       req.PR,
		Worktree: req.Worktree,
	})
}

func (a *lifecycleAdapter) Cleanup(ctx context.Context, id string, force, hard bool) error {
	sess, err := a.store.Get(ctx, id)
	if err != nil {
		return err
	}
	return a.lc.Cleanup(ctx, lifecycle.CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, TmuxSession: sess.TmuxSession,
	}, force)
}

func (a *lifecycleAdapter) Input(ctx context.Context, tmuxSession, text string) error {
	return a.lc.Input(ctx, tmuxSession, text)
}

func (a *lifecycleAdapter) Output(ctx context.Context, tmuxSession string, lines int) (string, error) {
	return a.lc.Output(ctx, tmuxSession, lines)
}
