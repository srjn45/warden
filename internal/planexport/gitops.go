package planexport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/lifecycle"
)

// PRRequest is the input for opening or reusing a pull request.
type PRRequest struct {
	Base  string
	Head  string
	Title string
	Body  string
}

// PRInfo is the result of CreateOrReusePR.
type PRInfo struct {
	URL     string
	Created bool
}

// GitHost is the Git + GitHub seam used by Syncer. Tests inject fakes; production
// uses RunnerGitHost backed by lifecycle.Runner (and CreatePR rails).
type GitHost interface {
	WorkingTreeDirty(ctx context.Context, dir string) (bool, error)
	RepositoryIdentity(ctx context.Context, repo string) (string, error)
	CheckGitHubAuth(ctx context.Context, dir string) error
	EnsureSyncWorktree(ctx context.Context, repo, branch, baseRef string) (worktree string, cleanup func(), remoteExisted bool, err error)
	ReadFile(ctx context.Context, worktree, relPath string) (exists bool, data []byte, err error)
	WriteFile(ctx context.Context, worktree, relPath string, data []byte) error
	CommitPath(ctx context.Context, worktree, relPath, message string) (sha string, err error)
	PushBranch(ctx context.Context, worktree, branch string) error
	HeadSHA(ctx context.Context, worktree string) (string, error)
	CreateOrReusePR(ctx context.Context, worktree string, req PRRequest) (PRInfo, error)
}

// RunnerGitHost implements GitHost with lifecycle.Runner subprocesses.
type RunnerGitHost struct {
	Run lifecycle.Runner
}

// NewRunnerGitHost wraps a lifecycle.Runner. Nil run defaults to ExecRunner.
func NewRunnerGitHost(run lifecycle.Runner) *RunnerGitHost {
	if run == nil {
		run = lifecycle.ExecRunner{}
	}
	return &RunnerGitHost{Run: run}
}

func (h *RunnerGitHost) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	return h.Run.Run(ctx, dir, name, args...)
}

func (h *RunnerGitHost) WorkingTreeDirty(ctx context.Context, dir string) (bool, error) {
	out, err := h.run(ctx, dir, "git", "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out) != "", nil
}

func (h *RunnerGitHost) RepositoryIdentity(ctx context.Context, repo string) (string, error) {
	out, err := h.run(ctx, repo, "git", "remote", "get-url", "origin")
	if err == nil {
		if u := strings.TrimSpace(out); u != "" {
			return u, nil
		}
	}
	abs, aerr := filepath.Abs(repo)
	if aerr != nil {
		return repo, nil
	}
	return abs, nil
}

func (h *RunnerGitHost) CheckGitHubAuth(ctx context.Context, dir string) error {
	out, err := h.run(ctx, dir, "gh", "auth", "status")
	if err != nil {
		msg := strings.TrimSpace(out)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func (h *RunnerGitHost) EnsureSyncWorktree(ctx context.Context, repo, branch, baseRef string) (string, func(), bool, error) {
	if err := validateGitRef(branch); err != nil {
		return "", nil, false, err
	}
	if err := validateGitRef(baseRef); err != nil {
		return "", nil, false, err
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return "", nil, false, err
	}
	wtRel := filepath.Join(".worktrees", "plan-sync-"+sanitizePathSegment(branch))
	wtAbs := filepath.Join(absRepo, wtRel)
	cleanup := func() {
		_, _ = h.run(context.Background(), absRepo, "git", "worktree", "remove", "--force", wtRel)
		_, _ = h.run(context.Background(), absRepo, "git", "worktree", "prune")
	}
	// Drop a leftover worktree from a prior crashed sync.
	if st, err := os.Stat(wtAbs); err == nil && st.IsDir() {
		cleanup()
	}

	fetchOut, err := h.run(ctx, absRepo, "git", "fetch", "origin", baseRef)
	if err != nil {
		return "", nil, false, fmt.Errorf("git fetch origin %s: %w: %s", baseRef, err, strings.TrimSpace(fetchOut))
	}

	remoteExisted := false
	if out, err := h.run(ctx, absRepo, "git", "ls-remote", "--heads", "origin", branch); err == nil && strings.TrimSpace(out) != "" {
		remoteExisted = true
	}

	baseStart := "origin/" + baseRef
	// Prefer starting from the remote sync branch when it already exists so we
	// never rewrite history; divergence is then detected at push time.
	startPoint := baseStart
	if remoteExisted {
		startPoint = "origin/" + branch
		if fout, ferr := h.run(ctx, absRepo, "git", "fetch", "origin", branch); ferr != nil {
			return "", nil, false, fmt.Errorf("git fetch origin %s: %w: %s", branch, ferr, strings.TrimSpace(fout))
		}
	}

	// Remove local branch if it exists without a worktree so -B can recreate it.
	_, _ = h.run(ctx, absRepo, "git", "branch", "-D", branch)

	addOut, err := h.run(ctx, absRepo, "git", "worktree", "add", "-B", branch, wtRel, startPoint)
	if err != nil {
		cleanup()
		return "", nil, false, fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(addOut))
	}
	return wtAbs, cleanup, remoteExisted, nil
}

func (h *RunnerGitHost) ReadFile(_ context.Context, worktree, relPath string) (bool, []byte, error) {
	p := filepath.Join(worktree, filepath.FromSlash(relPath))
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil, nil
		}
		return false, nil, err
	}
	return true, data, nil
}

func (h *RunnerGitHost) WriteFile(_ context.Context, worktree, relPath string, data []byte) error {
	p := filepath.Join(worktree, filepath.FromSlash(relPath))
	if err := mkdirAllParent(p); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func (h *RunnerGitHost) CommitPath(ctx context.Context, worktree, relPath, message string) (string, error) {
	branchOut, err := h.run(ctx, worktree, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w: %s", err, strings.TrimSpace(branchOut))
	}
	branch := strings.TrimSpace(branchOut)
	if lifecycle.IsProtectedBranch(branch) {
		return "", fmt.Errorf("refusing to commit on protected branch %q", branch)
	}
	addOut, err := h.run(ctx, worktree, "git", "add", "--", relPath)
	if err != nil {
		return "", fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(addOut))
	}
	commitOut, err := h.run(ctx, worktree, "git", "commit", "-m", message)
	if err != nil {
		_, _ = h.run(ctx, worktree, "git", "reset", "HEAD", "--", relPath)
		return "", fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(commitOut))
	}
	shaOut, err := h.run(ctx, worktree, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, strings.TrimSpace(shaOut))
	}
	return strings.TrimSpace(shaOut), nil
}

func (h *RunnerGitHost) PushBranch(ctx context.Context, worktree, branch string) error {
	if lifecycle.IsProtectedBranch(branch) {
		return fmt.Errorf("refusing to push protected branch %q", branch)
	}
	// Never force-push. Deleted remotes are recreated by a normal push -u.
	out, err := h.run(ctx, worktree, "git", "push", "-u", "origin", branch)
	if err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

func (h *RunnerGitHost) HeadSHA(ctx context.Context, worktree string) (string, error) {
	out, err := h.run(ctx, worktree, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w: %s", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

func (h *RunnerGitHost) CreateOrReusePR(ctx context.Context, worktree string, req PRRequest) (PRInfo, error) {
	if lifecycle.IsProtectedBranch(req.Head) {
		return PRInfo{}, fmt.Errorf("refusing to open a PR from protected branch %q", req.Head)
	}
	base := req.Base
	if base == "" {
		base = "main"
	}
	out, err := h.run(ctx, worktree, "gh", "pr", "create",
		"--base", base, "--head", req.Head, "--title", req.Title, "--body", req.Body)
	out = strings.TrimSpace(out)
	url := firstHTTPURL(out)
	if err != nil {
		if strings.Contains(out, "already exists") && url != "" {
			return PRInfo{URL: url, Created: false}, nil
		}
		return PRInfo{}, fmt.Errorf("gh pr create: %w: %s", err, out)
	}
	return PRInfo{URL: url, Created: true}, nil
}

func firstHTTPURL(s string) string {
	for _, f := range strings.Fields(s) {
		if strings.HasPrefix(f, "http://") || strings.HasPrefix(f, "https://") {
			return f
		}
	}
	return ""
}

func sanitizePathSegment(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, " ", "-")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}
