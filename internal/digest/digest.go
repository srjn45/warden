// Package digest builds an on-demand completion digest for an agent: a
// deterministic set of facts parsed from the Claude Code transcript, merged
// with git change stats, and enriched with a best-effort LLM narrative.
package digest

import "context"

// Facts are the deterministic signals parsed from a transcript. Pure output of
// ParseTranscript — no filesystem, no subprocess.
type Facts struct {
	EditedFiles []string // unique Write/Edit/MultiEdit/NotebookEdit targets, first-seen order
	Turns       int      // count of assistant records
	Task        string   // first real user prompt
	LastMessage string   // last assistant text (deterministic summary fallback)
}

// FileChange is one entry in a digest's file list.
type FileChange struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`   // git --numstat added lines; 0 when cwd isn't a repo
	Removed int    `json:"removed"` // git --numstat removed lines
	Edited  bool   `json:"edited"`  // appeared as an edit-tool target in the transcript
}

// Digest is the wire payload returned by GET /sessions/{id}/digest and consumed
// by the CLI, web, and TUI.
type Digest struct {
	Summary string       `json:"summary"` // LLM narrative, or LastMessage on fallback
	Files   []FileChange `json:"files"`
	Branch  string       `json:"branch"` // "" when cwd isn't a git repo
	Turns   int          `json:"turns"`
	Status  string       `json:"status"` // current warden status, set by the daemon
	Task    string       `json:"task"`
}

// LineDelta is the +/- pair for one file from git --numstat.
type LineDelta struct {
	Added, Removed int
}

// Narrator turns deterministic facts into a short natural-language summary. The
// real impl shells `claude -p`; the daemon degrades to Facts.LastMessage on error.
type Narrator interface {
	Summarize(ctx context.Context, f Facts) (string, error)
}
