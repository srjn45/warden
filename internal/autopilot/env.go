package autopilot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
	"gopkg.in/yaml.v3"
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
	// WorkflowsCoverPRs reports whether repo has CI workflows that trigger on pull
	// requests targeting integrationBranch — the signal that resolves a `gate:
	// auto` run to `ci` rather than the `local` fallback (autopilot.md §6.1). It
	// is only consulted when the gate is `auto`; an error fails open to false (the
	// safe local gate still runs and CI can be earned later).
	WorkflowsCoverPRs(ctx context.Context, repo, integrationBranch string) (bool, error)
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

func (execEnv) WorkflowsCoverPRs(_ context.Context, repo, integrationBranch string) (bool, error) {
	dir := filepath.Join(repo, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil // no workflows dir at all → no CI coverage
		}
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue // unreadable workflow — skip, don't fail the whole scan
		}
		if workflowCoversPRs(b, integrationBranch) {
			return true, nil
		}
	}
	return false, nil
}

// workflowCoversPRs reports whether a single workflow file triggers on pull
// requests targeting integrationBranch. A `pull_request` trigger with no branch
// filter covers all PR targets (including the integration branch); a filter
// covers it when the integration branch matches one of the listed patterns
// (exact or a simple `prefix/*` glob). Parsing failures are treated as "no
// coverage" so a malformed workflow never falsely claims CI (auto then safely
// falls back to the local gate).
func workflowCoversPRs(content []byte, integrationBranch string) bool {
	var wf struct {
		On yaml.Node `yaml:"on"`
	}
	if err := yaml.Unmarshal(content, &wf); err != nil {
		return false
	}
	branches, hasPR := pullRequestBranches(&wf.On)
	if !hasPR {
		return false
	}
	if len(branches) == 0 {
		return true // pull_request with no branch filter covers every target
	}
	for _, pat := range branches {
		if branchMatches(pat, integrationBranch) {
			return true
		}
	}
	return false
}

// pullRequestBranches inspects a workflow's `on:` node for a pull_request (or
// pull_request_target) trigger and returns its `branches` filter. hasPR reports
// whether any PR trigger is present at all; an empty branches slice with
// hasPR=true means "no filter" (covers all targets). The `on:` key can be a
// scalar (`on: pull_request`), a sequence (`on: [push, pull_request]`), or a
// mapping (`on: {pull_request: {branches: [...]}}`).
func pullRequestBranches(on *yaml.Node) (branches []string, hasPR bool) {
	node := on
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	const prKey, prTargetKey = "pull_request", "pull_request_target"
	switch node.Kind {
	case yaml.ScalarNode:
		return nil, node.Value == prKey || node.Value == prTargetKey
	case yaml.SequenceNode:
		for _, c := range node.Content {
			if c.Value == prKey || c.Value == prTargetKey {
				return nil, true
			}
		}
		return nil, false
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			if key != prKey && key != prTargetKey {
				continue
			}
			hasPR = true
			branches = append(branches, triggerBranches(node.Content[i+1])...)
		}
		return branches, hasPR
	}
	return nil, false
}

// triggerBranches extracts the `branches` list from a pull_request trigger's
// value node (a null/empty value means no filter).
func triggerBranches(val *yaml.Node) []string {
	if val == nil || val.Kind != yaml.MappingNode {
		return nil
	}
	var out []string
	for i := 0; i+1 < len(val.Content); i += 2 {
		if val.Content[i].Value != "branches" {
			continue
		}
		list := val.Content[i+1]
		if list.Kind == yaml.SequenceNode {
			for _, b := range list.Content {
				out = append(out, b.Value)
			}
		} else if list.Kind == yaml.ScalarNode {
			out = append(out, list.Value)
		}
	}
	return out
}

// branchMatches reports whether a workflow branch pattern matches branch. It
// handles the two common forms — an exact name and a `prefix/**`/`prefix/*` glob
// (e.g. `autopilot/**` covering `autopilot/integration`).
func branchMatches(pattern, branch string) bool {
	if pattern == branch || pattern == "**" || pattern == "*" {
		return true
	}
	if i := strings.IndexAny(pattern, "*"); i >= 0 {
		prefix := strings.TrimRight(pattern[:i], "/")
		return branch == prefix || strings.HasPrefix(branch, prefix+"/")
	}
	return false
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
