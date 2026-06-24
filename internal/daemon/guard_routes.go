package daemon

import (
	"encoding/json"
	"net/http"
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

// GuardRequest is the body for POST /hooks/guard, sent by `warden hook guard`.
type GuardRequest struct {
	Session string `json:"session"` // tmux session name == agent id
	Tool    string `json:"tool"`    // Claude tool name (Edit/Write/MultiEdit/NotebookEdit)
	Path    string `json:"path"`    // target file path from the tool input
}

// GuardResponse tells the hook whether to allow or deny the tool call. Reason is
// the redirect message fed back to Claude on a deny.
type GuardResponse struct {
	Decision string `json:"decision"`         // "allow" | "deny"
	Reason   string `json:"reason,omitempty"` // populated on deny
}

// handleGuard evaluates the isolation policy for one file-mutating tool call.
// It fails soft in every direction the guard is not certain about: an unknown
// session (already gone, or never warden-spawned) returns allow, so the backstop
// can never wedge an agent's edits.
func (s *Server) handleGuard(w http.ResponseWriter, r *http.Request) {
	var req GuardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	sess, err := s.store.Get(r.Context(), req.Session)
	if err != nil {
		writeJSON(w, http.StatusOK, GuardResponse{Decision: "allow"})
		return
	}
	if deny, reason := guardDecision(sess, req.Tool, req.Path); deny {
		writeJSON(w, http.StatusOK, GuardResponse{Decision: "deny", Reason: reason})
		return
	}
	writeJSON(w, http.StatusOK, GuardResponse{Decision: "allow"})
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
	workdir := filepath.Clean(sess.Workdir)
	clean := filepath.Clean(path)
	if repo == "" || workdir == "" {
		return false, ""
	}
	if isUnder(clean, workdir) {
		return false, "" // inside the agent's own worktree
	}
	if isUnder(clean, repo) {
		return true, "This agent is isolated in its worktree at " + sess.Workdir +
			". The target " + path + " is inside the shared repo (" + sess.Repo +
			") but outside your worktree — another agent may own it. Re-run the edit " +
			"against the matching path under " + sess.Workdir + " instead."
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
