package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
)

// Env is the small host surface the preflight touches: git topology on the plan
// file's repo and `gh` auth. It is an interface so tests drive preflight with a
// fake instead of a real repo + GitHub login. The real implementation is
// execEnv; NewController defaults to it.
type Env interface {
	// GitToplevel returns the absolute repo root containing dir (git
	// rev-parse --show-toplevel). A non-repo dir is an error.
	GitToplevel(ctx context.Context, dir string) (string, error)
	// DefaultBranch returns the repo's default branch name (the branch HEAD of
	// origin, e.g. "main"). Falls back to a best guess when no remote HEAD is set.
	DefaultBranch(ctx context.Context, repo string) (string, error)
	// BranchExists reports whether branch exists locally in repo.
	BranchExists(ctx context.Context, repo, branch string) (bool, error)
	// CreateBranch creates branch off base in repo without checking it out
	// (git branch <branch> <base>).
	CreateBranch(ctx context.Context, repo, branch, base string) error
	// GHAuthOK returns nil when `gh` is installed and authenticated and can reach
	// the remote; otherwise an actionable error.
	GHAuthOK(ctx context.Context) error
	// BackendKnown returns nil when backend is an agent backend warden can launch,
	// else an actionable error. It is the mechanical half of the §5.1 backend-trust
	// preflight — a typo'd or uninstalled ladder backend surfaces at enable, not
	// mid-rotation at 3am. Deeper per-repo trust/auth detection (a first-run trust
	// prompt is a one-time operator step, never a runtime blocker) lands with the
	// tier failover work (S5); autopilot never auto-clears a trust prompt.
	BackendKnown(backend string) error
}

// execEnv is the production Env backed by the git and gh CLIs.
type execEnv struct{}

// NewExecEnv returns the real host-backed Env (git + gh CLIs).
func NewExecEnv() Env { return execEnv{} }

func (execEnv) GitToplevel(ctx context.Context, dir string) (string, error) {
	out, err := runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", dir)
	}
	return strings.TrimSpace(out), nil
}

func (execEnv) DefaultBranch(ctx context.Context, repo string) (string, error) {
	// origin/HEAD symbolic ref → the remote's default branch (e.g. main).
	if out, err := runGit(ctx, repo, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		ref := strings.TrimSpace(out)
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			ref = ref[i+1:]
		}
		if ref != "" {
			return ref, nil
		}
	}
	// No remote HEAD configured — fall back to whichever conventional name exists.
	for _, name := range []string{"main", "master"} {
		if ok, _ := (execEnv{}).BranchExists(ctx, repo, name); ok {
			return name, nil
		}
	}
	return "", fmt.Errorf("cannot determine default branch for %s (no origin/HEAD and no main/master)", repo)
}

func (execEnv) BranchExists(ctx context.Context, repo, branch string) (bool, error) {
	err := runGitQuiet(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	// show-ref exits 1 when the ref is absent — that is "does not exist", not a
	// hard error. Any other failure is surfaced.
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

func (execEnv) CreateBranch(ctx context.Context, repo, branch, base string) error {
	if _, err := runGit(ctx, repo, "branch", branch, base); err != nil {
		return fmt.Errorf("create branch %s off %s: %w", branch, base, err)
	}
	return nil
}

func (execEnv) BackendKnown(backend string) error {
	if _, err := agentbackend.Get(backend); err != nil {
		return fmt.Errorf("brain backend %q is not a known agent backend: %v", backend, err)
	}
	return nil
}

func (execEnv) GHAuthOK(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "gh", "auth", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("gh CLI not installed (needed for PR gating)")
		}
		return fmt.Errorf("gh is not authenticated: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// --- small exec helpers ---

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return string(out), err
}

func runGitQuiet(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	return cmd.Run()
}

func isExitCode(err error, code int) bool {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() == code
	}
	return false
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "executable file not found")
}
