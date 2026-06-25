package store

import "time"

// ExportVersion is the schema version stamped into an Export envelope. Bump it
// when the envelope shape changes in a way `warden import` must detect to read
// an older dump safely.
const ExportVersion = 1

// Export is the on-the-wire form of a set of Session records, produced by
// `warden export` and consumed by `warden import`. It is metadata only: the
// referenced worktrees, branches, and tmux sessions are NOT serialized as files
// and are NOT recreated on import. The Worktree/Branch/TmuxSession fields ride
// along as plain strings so an imported record still remembers where its (now
// absent) worktree used to live, but import never touches the working tree.
type Export struct {
	Version    int        `json:"version"`
	ExportedAt time.Time  `json:"exported_at"`
	Sessions   []*Session `json:"sessions"`
}

// ImportResult summarizes one `warden import` run. Each field lists the session
// ids that landed in that bucket, so the outcome is auditable rather than a bare
// count.
type ImportResult struct {
	Imported []string `json:"imported,omitempty"` // ids newly inserted
	Skipped  []string `json:"skipped,omitempty"`  // ids that already existed (no --merge)
	Merged   []string `json:"merged,omitempty"`   // ids overwritten under --merge
	Renamed  []string `json:"renamed,omitempty"`  // ids imported after dropping a colliding name
}
