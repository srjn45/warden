package daemon

import (
	"context"
	"os"
	"strings"

	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/store"
)

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
