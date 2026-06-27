package daemon

import (
	"context"
	"os"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
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
//
// The transcript is read through the agent's backend (design §5): a backend whose
// transcript is not structured (Caps.StructuredTranscript=false) skips parsing and
// degrades to a pane-scrape summary; a structured non-Claude backend (e.g. Aider's
// markdown) is parsed via its own ParseTranscript and bridged into the neutral
// digest Facts. Claude keeps its existing JSONL path verbatim.
func (s *Server) buildDigest(ctx context.Context, sess *store.Session) digest.Digest {
	d := digest.Digest{Status: string(sess.Status)}

	b, err := agentbackend.Get(sess.Backend)
	if err != nil {
		b = agentbackend.Default()
	}

	// Tier B: no parseable transcript ⇒ degrade to a pane-scrape summary rather
	// than a structured digest (savings/token counting are likewise skipped — they
	// need real token deltas this backend can't supply).
	if b == nil || !b.Capabilities().StructuredTranscript {
		d.Summary = s.paneScrapeSummary(ctx, sess)
		return d
	}

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

	facts := s.transcriptFacts(b, f)
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

// transcriptFacts parses an open transcript into digest Facts. Claude keeps its
// existing JSONL parser (byte-identical behavior); any other structured backend
// goes through the neutral Backend.ParseTranscript and is bridged into Facts —
// the path that proves the adapter interface end-to-end (Phase 1).
func (s *Server) transcriptFacts(b agentbackend.Backend, f *os.File) digest.Facts {
	if b.ID() == agentbackend.DefaultID {
		facts, _ := digest.ParseTranscript(f) // I/O error ignored — malformed lines skipped internally
		return facts
	}
	turns, _ := b.ParseTranscript(f)
	return factsFromTurns(turns)
}

// factsFromTurns bridges a backend's neutral []Turn into the digest Facts the
// narrator and completion summary read.
func factsFromTurns(turns []agentbackend.Turn) digest.Facts {
	var f digest.Facts
	for _, t := range turns {
		switch t.Role {
		case "assistant":
			f.Turns++
			if msg := strings.TrimSpace(t.Text); msg != "" {
				f.LastMessage = msg
			}
			for _, fp := range t.Files {
				f.EditedFiles = appendUniqueStr(f.EditedFiles, fp)
			}
		case "user":
			if f.Task == "" {
				f.Task = strings.TrimSpace(t.Text)
			}
		}
	}
	return f
}

// paneScrapeSummary is the Tier-B digest fallback: it reads the agent's tmux pane
// and returns its last meaningful line as a low-fidelity summary, since there is
// no structured transcript to parse.
func (s *Server) paneScrapeSummary(ctx context.Context, sess *store.Session) string {
	pane, err := s.life.Output(ctx, sess.TmuxSession, 40)
	if err != nil || strings.TrimSpace(pane) == "" {
		return "no transcript available"
	}
	lines := strings.Split(strings.TrimRight(pane, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return "no transcript available"
}

// appendUniqueStr appends s to dst unless already present (first-seen order).
func appendUniqueStr(dst []string, s string) []string {
	for _, e := range dst {
		if e == s {
			return dst
		}
	}
	return append(dst, s)
}
