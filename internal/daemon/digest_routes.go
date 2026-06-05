package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/agentctl/internal/digest"
	"github.com/srajanpathak/agentctl/internal/store"
)

// handleDigest builds an on-demand completion digest for one agent: deterministic
// transcript facts ∪ git change stats, enriched with a best-effort LLM summary
// that degrades to the last assistant message on any narrator failure.
func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
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
	d := s.buildDigest(r.Context(), sess)
	writeJSON(w, http.StatusOK, d)
}

// buildDigest assembles a completion digest from a session's on-disk transcript
// and git stats, enriched by a best-effort narrator that degrades to the last
// assistant message. Side-effect-free; reused by the pipeline executor to snapshot
// a job's digest at completion.
func (s *Server) buildDigest(ctx context.Context, sess *store.Session) digest.Digest {
	d := digest.Digest{Status: string(sess.Status)}
	path := s.life.TranscriptPath(sess)
	if path == "" {
		d.Summary = "no transcript available"
		return d
	}
	f, err := os.Open(path)
	if err != nil {
		d.Summary = "no transcript available"
		return d
	}
	defer f.Close()
	facts, _ := digest.ParseTranscript(f)
	stats := digest.ParseNumstat(s.life.GitNumstat(ctx, sess.Workdir))
	d.Files = digest.MergeFiles(facts.EditedFiles, stats)
	d.Branch = s.life.GitBranch(ctx, sess.Workdir)
	d.Turns = facts.Turns
	d.Task = facts.Task
	d.Summary = facts.LastMessage
	if s.narrator != nil {
		if out, err := s.narrator.Summarize(ctx, facts); err == nil && strings.TrimSpace(out) != "" {
			d.Summary = out
		}
	}
	return d
}
