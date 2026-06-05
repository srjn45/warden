package daemon

import (
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

	d := digest.Digest{Status: string(sess.Status)}

	path := s.life.TranscriptPath(sess)
	if path == "" {
		d.Summary = "no transcript available"
		writeJSON(w, http.StatusOK, d)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		d.Summary = "no transcript available"
		writeJSON(w, http.StatusOK, d)
		return
	}
	defer f.Close()

	facts, _ := digest.ParseTranscript(f) // malformed lines tolerated inside
	stats := digest.ParseNumstat(s.life.GitNumstat(r.Context(), sess.Workdir))

	d.Files = digest.MergeFiles(facts.EditedFiles, stats)
	d.Branch = s.life.GitBranch(r.Context(), sess.Workdir)
	d.Turns = facts.Turns
	d.Task = facts.Task

	// Deterministic fallback first; the narrator only enriches.
	d.Summary = facts.LastMessage
	if s.narrator != nil {
		if out, err := s.narrator.Summarize(r.Context(), facts); err == nil && strings.TrimSpace(out) != "" {
			d.Summary = out
		}
	}
	writeJSON(w, http.StatusOK, d)
}
