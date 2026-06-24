package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// GitRequest is the shared body for the wd commit/push/sync endpoints. Session
// is the calling agent's id (optional): when it resolves, warden uses the
// agent's own Workdir as the authoritative target — an agent cannot commit into
// a sibling worktree by passing a different dir — and records the action against
// the agent. Dir is the fallback for a human running wd commit outside an agent.
type GitRequest struct {
	Session string `json:"session,omitempty"`
	Dir     string `json:"dir,omitempty"`
	Message string `json:"message,omitempty"` // commit only
	Base    string `json:"base,omitempty"`    // sync only (defaults to main)
}

// resolveGitTarget decodes the body and returns the authoritative working dir
// plus the resolved session (nil when none/unknown). It writes the HTTP error
// and returns ok=false when the target cannot be determined.
func (s *Server) resolveGitTarget(w http.ResponseWriter, r *http.Request) (dir string, sess *store.Session, req GitRequest, ok bool) {
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return "", nil, req, false
	}
	dir = req.Dir
	if req.Session != "" {
		got, err := s.store.Get(r.Context(), req.Session)
		switch {
		case err == nil:
			sess = got
			if sess.Workdir != "" {
				dir = sess.Workdir // authoritative: pin the action to the agent's own worktree
			}
		case errors.Is(err, store.ErrNotFound):
			// Unknown session: fall back to the provided dir (a human may pass a
			// stale id); only fail if there is nothing to act on.
		default:
			writeErr(w, http.StatusInternalServerError, err.Error())
			return "", nil, req, false
		}
	}
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "no working directory: provide dir or a known session")
		return "", nil, req, false
	}
	return dir, sess, req, true
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	dir, sess, req, ok := s.resolveGitTarget(w, r)
	if !ok {
		return
	}
	res, err := s.life.Commit(r.Context(), dir, req.Message)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if res.Committed && sess != nil {
		s.recordGitEvent(sess.ID, "commit", res.SHA+" on "+res.Branch)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitPush(w http.ResponseWriter, r *http.Request) {
	dir, sess, _, ok := s.resolveGitTarget(w, r)
	if !ok {
		return
	}
	res, err := s.life.Push(r.Context(), dir)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if sess != nil {
		s.recordGitEvent(sess.ID, "push", res.Branch+" -> "+res.Remote)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGitSync(w http.ResponseWriter, r *http.Request) {
	dir, sess, req, ok := s.resolveGitTarget(w, r)
	if !ok {
		return
	}
	res, err := s.life.Sync(r.Context(), dir, req.Base)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if sess != nil && res.Updated {
		s.recordGitEvent(sess.ID, "sync", "rebased onto "+res.Base)
	}
	writeJSON(w, http.StatusOK, res)
}

// recordGitEvent appends a best-effort bookkeeping event linking a git action to
// the agent record. Failures are ignored — the git work already succeeded.
func (s *Server) recordGitEvent(id, kind, detail string) {
	_ = s.store.AppendEvent(context.Background(), id, store.Event{
		TS:     time.Now(),
		Type:   kind,
		Detail: detail,
	})
}
