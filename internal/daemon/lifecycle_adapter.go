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
// the type so unknown types collapse to "other" (no worktree). In prompt mode
// (Prompt set, Type empty) the Type is left empty so the doc stays "classifying".
func (a *lifecycleAdapter) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	lr := lifecycle.SpawnRequest{
		Ticket:     req.Ticket,
		Repo:       req.Repo,
		Branch:     req.Branch,
		PR:         req.PR,
		Worktree:   req.Worktree,
		Prompt:     req.Prompt,
		Cwd:        req.Cwd,
		Supervised: req.Supervised,
	}
	// Leave Type empty in prompt mode so it stays "classifying"; otherwise normalize.
	if !(req.Prompt != "" && req.Type == "") {
		lr.Type = store.NormalizeType(req.Type)
	}
	return a.lc.Spawn(ctx, lr)
}

func (a *lifecycleAdapter) Classify(ctx context.Context, prompt string) (store.Type, error) {
	return a.lc.Classify(ctx, prompt)
}

func (a *lifecycleAdapter) Terminate(ctx context.Context, tmuxSession string) error {
	return a.lc.Terminate(ctx, tmuxSession)
}

func (a *lifecycleAdapter) RemoveWorktree(ctx context.Context, sess *store.Session, force bool) error {
	return a.lc.RemoveWorktree(ctx, lifecycle.CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, TmuxSession: sess.TmuxSession,
	}, force)
}

// Teardown force-cleans the resources Spawn created (spawn rollback): kill tmux,
// then force-remove the worktree if there is one.
func (a *lifecycleAdapter) Teardown(ctx context.Context, sess *store.Session) error {
	_ = a.lc.Terminate(ctx, sess.TmuxSession)
	if sess.Worktree == "" {
		return nil
	}
	return a.lc.RemoveWorktree(ctx, lifecycle.CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, TmuxSession: sess.TmuxSession,
	}, true)
}

func (a *lifecycleAdapter) Restore(ctx context.Context, sess *store.Session) error {
	return a.lc.Restore(ctx, sess)
}

func (a *lifecycleAdapter) NewestClaudeSession(_ context.Context, cwd string) (string, error) {
	return a.lc.NewestClaudeSession(cwd)
}

func (a *lifecycleAdapter) Adopt(ctx context.Context, req AdoptParams) (*store.Session, error) {
	return a.lc.Adopt(ctx, lifecycle.AdoptRequest{
		ID:              req.ID,
		Cwd:             req.Cwd,
		ClaudeSessionID: req.ClaudeSessionID,
		TmuxSession:     req.TmuxSession,
	})
}

func (a *lifecycleAdapter) Input(ctx context.Context, tmuxSession, text string) error {
	return a.lc.Input(ctx, tmuxSession, text)
}

func (a *lifecycleAdapter) Output(ctx context.Context, tmuxSession string, lines int) (string, error) {
	return a.lc.Output(ctx, tmuxSession, lines)
}

func (a *lifecycleAdapter) SendKeys(ctx context.Context, tmuxSession, key string) error {
	return a.lc.SendKeys(ctx, tmuxSession, key)
}
