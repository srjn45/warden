package daemon

import (
	"path/filepath"
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// guardTools are the file-mutating Claude tools the isolation guard evaluates.
// The PreToolUse matcher in the generated settings already narrows to these; the
// map is a defensive re-check so a stray tool name can never be denied.
var guardTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// guardDecision is the pure isolation policy. An isolated agent (one that owns a
// worktree) must not Edit/Write a file that lives inside the shared repo but
// outside its own worktree — that is escaping into the shared tree or a sibling
// agent's worktree. Everything else passes:
//   - in-repo / free-form agents (no worktree) are unconstrained by policy;
//   - edits within the agent's own worktree are fine;
//   - edits entirely outside the repo (e.g. /tmp scratch) are not our concern;
//   - a non-absolute path can't be resolved server-side, so it fails open.
//
// It returns (deny, reason) where reason is the redirect message for Claude.
func guardDecision(sess *store.Session, tool, path string) (bool, string) {
	if !guardTools[tool] {
		return false, ""
	}
	if sess == nil || sess.Worktree == "" {
		return false, "" // not isolated by policy — nothing to enforce
	}
	if !filepath.IsAbs(path) {
		return false, "" // unresolvable here; fail open
	}
	repo := filepath.Clean(sess.Repo)
	if repo == "" {
		return false, ""
	}
	// Resolve the isolation boundary from the same field the gate keys on
	// (Worktree), rather than the separately-stored Workdir. Worktree is recorded
	// relative to the repo, so join it onto repo (tolerating an already-absolute
	// value) and Clean. This keeps the gate and the containment check derived from
	// one field instead of relying on spawn keeping Workdir == repo/worktree.
	worktree := sess.Worktree
	if !filepath.IsAbs(worktree) {
		worktree = filepath.Join(repo, worktree)
	}
	worktree = filepath.Clean(worktree)
	clean := filepath.Clean(path)
	if isUnder(clean, worktree) {
		return false, "" // inside the agent's own worktree
	}
	if isUnder(clean, repo) {
		return true, "This agent is isolated in its worktree at " + worktree +
			". The target " + path + " is inside the shared repo (" + sess.Repo +
			") but outside your worktree — another agent may own it. Re-run the edit " +
			"against the matching path under " + worktree + " instead."
	}
	return false, "" // outside the repo entirely
}

// isUnder reports whether path equals root or is nested beneath it. The check is
// lexical (paths are pre-Cleaned by the caller); symlinks are not resolved, which
// is acceptable for a best-effort isolation backstop.
func isUnder(path, root string) bool {
	if root == "" {
		return false
	}
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}
