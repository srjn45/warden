package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/store"
)

// handleCreatePR opens a GitHub pull request for one agent's branch. It pins to
// the agent's own worktree, pushes the branch (gh needs it on the remote), builds
// the completion digest for the PR title+body, and runs `gh pr create`. An
// already-existing PR comes back as a non-error result so `done --create-pr` is
// idempotent. The work is done before the CLI terminates the agent, so any
// failure leaves the agent untouched for a retry.
func (s *Server) handleCreatePR(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req struct {
		Base string `json:"base,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body is optional (base defaults to main)

	dir := sess.Workdir
	if dir == "" {
		writeErr(w, http.StatusConflict, "agent has no working directory — cannot open a PR")
		return
	}
	// gh pr create needs the branch on the remote; push first (also enforces the
	// protected-branch rail before we build the digest).
	if _, err := s.life.Push(r.Context(), dir); err != nil {
		writeErr(w, http.StatusConflict, "push failed: "+err.Error())
		return
	}
	d := s.buildDigest(r.Context(), sess)
	res, err := s.life.CreatePR(r.Context(), dir, prTitle(sess, d), digest.Markdown(&d), req.Base)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.recordGitEvent(sess.ID, "pr", res.URL)
	writeJSON(w, http.StatusOK, res)
}

// prTitle picks a human PR title for an agent: its live one-line subject, else
// the digest's parsed task (the first user prompt, truncated), else the branch.
func prTitle(sess *store.Session, d digest.Digest) string {
	for _, c := range []string{sess.Subject, d.Task} {
		if t := truncateTitle(strings.TrimSpace(c)); t != "" {
			return t
		}
	}
	if d.Branch != "" {
		return d.Branch
	}
	return sess.ID
}

// truncateTitle collapses a candidate to its first line and caps it at 72 chars
// (with an ellipsis) so a long prompt does not become an unwieldy PR title.
func truncateTitle(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 72
	if len(s) > max {
		return strings.TrimSpace(s[:max-1]) + "…"
	}
	return s
}
