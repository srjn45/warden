package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/srajanpathak/warden/internal/digest"
	"github.com/srajanpathak/warden/internal/store"
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
	// Pipeline jobs capture a digest snapshot at reap time; serve it directly so the
	// transcript/git rebuild is skipped for reaped agents. A nil snapshot (job not yet
	// reaped) falls through to the live rebuild below.
	if s.exec != nil && sess.PipelineID != "" && sess.JobID != "" {
		if snap := s.exec.JobDigest(sess.PipelineID, sess.JobID); snap != nil {
			writeJSON(w, http.StatusOK, *snap)
			return
		}
	}
	d := s.buildDigest(r.Context(), sess)
	writeJSON(w, http.StatusOK, d)
}

// BuildDigest is the exported entry point for wiring buildDigest into the executor.
func (s *Server) BuildDigest(ctx context.Context, sess *store.Session) digest.Digest {
	return s.buildDigest(ctx, sess)
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
	facts, _ := digest.ParseTranscript(f) // scanner I/O error ignored — malformed lines are skipped internally; partial facts still beat a 500
	stats := digest.ParseNumstat(s.life.GitNumstat(ctx, sess.Workdir))
	d.Files = digest.MergeFiles(facts.EditedFiles, stats)
	d.Branch = s.life.GitBranch(ctx, sess.Workdir)
	d.Turns = facts.Turns
	d.Task = facts.Task
	// Deterministic fallback first; the narrator only enriches.
	d.Summary = facts.LastMessage
	if s.narrator != nil {
		if out, err := s.narrator.Summarize(ctx, facts); err == nil && strings.TrimSpace(out) != "" {
			d.Summary = out
		}
	}
	return d
}
