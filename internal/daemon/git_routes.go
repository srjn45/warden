package daemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/store"
)

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

// recordGitEvent appends a best-effort bookkeeping event linking a git action to
// the agent record. Failures are ignored — the git work already succeeded.
func (s *Server) recordGitEvent(id, kind, detail string) {
	_ = s.store.AppendEvent(context.Background(), id, store.Event{
		TS:     time.Now(),
		Type:   kind,
		Detail: detail,
	})
}
