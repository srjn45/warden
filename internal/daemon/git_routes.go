package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/plugin"
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
	dir, sess, status, msg := s.pinnedWorkdir(r.Context(), req.Session, req.Dir)
	if status != 0 {
		writeErr(w, status, msg)
		return "", nil, req, false
	}
	return dir, sess, req, true
}

// pinnedWorkdir resolves the authoritative working dir for an agent action.
// When session resolves to a known agent, that agent's own Workdir wins over any
// client-supplied dir — the security boundary that stops an agent acting on a
// sibling worktree by passing a different path. An unknown session falls back to
// the supplied dir (a human may pass a stale id). status==0 means success;
// otherwise (status, msg) is the HTTP error the caller should write.
func (s *Server) pinnedWorkdir(ctx context.Context, session, dir string) (resolved string, sess *store.Session, status int, msg string) {
	resolved = dir
	if session != "" {
		got, err := s.store.Get(ctx, session)
		switch {
		case err == nil:
			sess = got
			if sess.Workdir != "" {
				resolved = sess.Workdir // authoritative: pin to the agent's own worktree
			}
		case errors.Is(err, store.ErrNotFound):
			// Unknown session: fall back to the provided dir.
		default:
			return "", nil, http.StatusInternalServerError, err.Error()
		}
	}
	if resolved == "" {
		return "", nil, http.StatusBadRequest, "no working directory: provide dir or a known session"
	}
	return resolved, sess, 0, ""
}

func (s *Server) handleGitCommit(w http.ResponseWriter, r *http.Request) {
	dir, sess, req, ok := s.resolveGitTarget(w, r)
	if !ok {
		return
	}
	meta := plugin.MetaFromSession(sess)
	meta.Workdir = dir
	// pre-commit hook (#47): advisory, fail-open.
	s.plugins.Dispatch(r.Context(), plugin.EventPreCommit, meta, map[string]string{"message": req.Message})
	res, err := s.life.Commit(r.Context(), dir, req.Message)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if res.Committed && sess != nil {
		s.recordGitEvent(sess.ID, "commit", res.SHA+" on "+res.Branch)
	}
	// post-commit hook (#47): advisory, fail-open. Payload carries the resulting
	// SHA/branch and whether anything was actually committed.
	s.plugins.Dispatch(r.Context(), plugin.EventPostCommit, meta, map[string]string{
		"sha": res.SHA, "branch": res.Branch, "committed": strconv.FormatBool(res.Committed),
	})
	// Record the git tool-spam this compact struct kept out of context (before
	// serialization, since RawBytes is json:"-"). Fail-open.
	s.recordGitSavings(sess, res.RawBytes, res.RawSample, res)
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
	s.recordGitSavings(sess, res.RawBytes, res.RawSample, res)
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
	s.recordGitSavings(sess, res.RawBytes, res.RawSample, res)
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
