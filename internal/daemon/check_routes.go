package daemon

import (
	"encoding/json"
	"net/http"
)

// CheckRequest is the body for POST /check, sent by `wd check` / mcp__warden__check.
// Session pins the run to the calling agent's own worktree (the same boundary as
// the git routes); Dir is the fallback for a human run. Name selects a configured
// check ("" runs them all).
type CheckRequest struct {
	Session string `json:"session,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Name    string `json:"name,omitempty"`
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req CheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	dir, sess, status, msg := s.pinnedWorkdir(r.Context(), req.Session, req.Dir)
	if status != 0 {
		writeErr(w, status, msg)
		return
	}
	res, err := s.life.Check(r.Context(), dir, req.Name)
	if err != nil {
		// No config / unknown name are operator-facing 422s, not server faults:
		// the message tells the agent what to do (add config, or which names exist).
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if sess != nil {
		detail := "passed"
		if !res.Passed {
			detail = "failed"
		}
		s.recordGitEvent(sess.ID, "check", detail)
	}
	writeJSON(w, http.StatusOK, res)
}
