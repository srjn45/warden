package daemon

import (
	"context"
	"fmt"

	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/pressure"
	"github.com/srjn45/warden/internal/store"
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
// the type so unknown types collapse to "other" (no worktree). In free-form mode
// (no Type — prompted or interactive) the Type is left empty so the doc stays
// "classifying" and lifecycle.Spawn launches in the caller's cwd.
func (a *lifecycleAdapter) Spawn(ctx context.Context, req SpawnRequest) (*store.Session, error) {
	lr := lifecycle.SpawnRequest{
		Ticket:         req.Ticket,
		Name:           req.Name,
		Repo:           req.Repo,
		Branch:         req.Branch,
		PR:             req.PR,
		Worktree:       req.Worktree,
		InRepo:         req.InRepo,
		Prompt:         req.Prompt,
		Cwd:            req.Cwd,
		PermissionMode: req.PermissionMode,
		AutoRestart:    req.AutoRestart,
		Model:          req.Model,
		Backend:        req.Backend,
		Tags:           req.Tags,
		ParentID:       req.ParentID,
	}
	// Normalize only a typed spawn. Free-form (Type empty) is keyed on cwd, not the
	// prompt: leaving Type empty keeps lifecycle.Spawn on the cwd-launch path and the
	// doc "classifying" — for both prompted and empty-prompt (interactive) spawns.
	// (Must NOT normalize when Type=="", or NormalizeType collapses "" → "other",
	// flipping an interactive spawn onto the typed/managed-worktree path.)
	if req.Type != "" {
		lr.Type = store.NormalizeType(req.Type)
	}
	// Fork (codex fork superpower, #52): the adapter owns the store, so it resolves
	// the source agent here ONCE and hands lifecycle the read-back values — lifecycle
	// stays store-free (see lifecycle.SpawnRequest fork fields). It reads the source's
	// PINNED backend session id (the codex rollout UUID the fork branches from; empty
	// ⇒ ErrForkSourceNotPinned, §5) and its branch (the fork worktree's base, §7), and
	// pins the fork to the source's repo so the worktree is a sibling off that branch.
	if req.ForkFrom != "" {
		src, err := a.store.Get(ctx, req.ForkFrom)
		if err != nil {
			return nil, err
		}
		if src.ClaudeSessionID == "" {
			return nil, lifecycle.ErrForkSourceNotPinned
		}
		if src.Branch == "" {
			return nil, fmt.Errorf("fork source %s has no branch to base the fork on", req.ForkFrom)
		}
		lr.ForkFrom = req.ForkFrom
		lr.ForkSourceSessionID = src.ClaudeSessionID
		lr.ForkSourceBranch = src.Branch
		lr.ForkSourceWorkdir = src.Workdir // read-side of the PR-2 dirty-tree carry (§7)
		lr.Repo = src.Repo                 // base + worktree live in the source agent's repo
		// A fork MUST run the source's backend — the SessionForker that minted the
		// session is the only one that can branch it (forking a codex session with the
		// claude backend would hit the clean "cannot fork"). Pin it from the source so
		// the wrappers don't have to restate --backend (and a mismatched one can't win).
		lr.Backend = src.Backend
	}
	return a.lc.Spawn(ctx, lr)
}

func (a *lifecycleAdapter) Classify(ctx context.Context, prompt string) (store.Type, error) {
	return a.lc.Classify(ctx, prompt)
}

func (a *lifecycleAdapter) GenerateName(ctx context.Context, prompt string) string {
	return a.lc.GenerateName(ctx, prompt)
}

func (a *lifecycleAdapter) Terminate(ctx context.Context, tmuxSession string) error {
	return a.lc.Terminate(ctx, tmuxSession)
}

func (a *lifecycleAdapter) RemoveWorktree(ctx context.Context, sess *store.Session, force, deleteAdoptedBranch bool) error {
	return a.lc.RemoveWorktree(ctx, lifecycle.CleanupTarget{
		ID: sess.ID, Repo: sess.Repo, Worktree: sess.Worktree,
		Branch: sess.Branch, BranchCreated: sess.BranchCreated,
		TmuxSession: sess.TmuxSession,
	}, force, deleteAdoptedBranch)
}

func (a *lifecycleAdapter) ListWorktrees(ctx context.Context, repo string, active, archived []*store.Session) ([]lifecycle.WorktreeListing, error) {
	return a.lc.ListWorktrees(ctx, repo, active, archived)
}

func (a *lifecycleAdapter) PruneWorktrees(ctx context.Context, repo string, opts lifecycle.PruneOpts) ([]lifecycle.PruneResult, error) {
	return a.lc.PruneWorktrees(ctx, repo, opts)
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
		Branch: sess.Branch, BranchCreated: sess.BranchCreated,
		TmuxSession: sess.TmuxSession,
	}, true, false)
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

func (a *lifecycleAdapter) SpawnJob(ctx context.Context, req lifecycle.JobSpawnRequest) (*store.Session, error) {
	return a.lc.SpawnJob(ctx, req)
}

func (a *lifecycleAdapter) TranscriptPath(sess *store.Session) string {
	return a.lc.TranscriptPath(sess)
}

func (a *lifecycleAdapter) GitBranch(ctx context.Context, dir string) string {
	return a.lc.GitBranch(ctx, dir)
}

func (a *lifecycleAdapter) GitNumstat(ctx context.Context, dir string) string {
	return a.lc.GitNumstat(ctx, dir)
}

func (a *lifecycleAdapter) CommitWorktree(ctx context.Context, dir, message string) (bool, error) {
	return a.lc.CommitWorktree(ctx, dir, message)
}

func (a *lifecycleAdapter) Commit(ctx context.Context, dir, message string) (lifecycle.CommitResult, error) {
	return a.lc.Commit(ctx, dir, message)
}

func (a *lifecycleAdapter) Push(ctx context.Context, dir string) (lifecycle.PushResult, error) {
	return a.lc.Push(ctx, dir)
}

func (a *lifecycleAdapter) Sync(ctx context.Context, dir, base string) (lifecycle.SyncResult, error) {
	return a.lc.Sync(ctx, dir, base)
}

func (a *lifecycleAdapter) CreatePR(ctx context.Context, dir, title, body, base string) (lifecycle.PRResult, error) {
	return a.lc.CreatePR(ctx, dir, title, body, base)
}

func (a *lifecycleAdapter) Check(ctx context.Context, dir, name string) (lifecycle.CheckResult, error) {
	return a.lc.Check(ctx, dir, name)
}

func (a *lifecycleAdapter) MemoryPressure(ctx context.Context) (pressure.Level, error) {
	return a.lc.MemoryPressure(ctx)
}
