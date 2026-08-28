package projectstore

import "time"

// Status is a project's lifecycle state. A project is never deleted on close
// (IDE-like hibernation, docs/specs/2026-08-28-project-centric-ui.md §4): closing
// only flips it to StatusClosed and hides it from the active surfaces; reopening
// flips it back. The zero value ("") is treated as StatusOpen by NormalizeStatus
// so a record that predates the field reads as open.
type Status string

const (
	// StatusOpen is an active project shown in the cockpit/TUI.
	StatusOpen Status = "open"
	// StatusClosed is a hibernated project: kept in the DB, hidden from active
	// surfaces, its agents terminated-but-restorable until it is reopened.
	StatusClosed Status = "closed"
)

// Valid reports whether s is one of the known statuses.
func (s Status) Valid() bool { return s == StatusOpen || s == StatusClosed }

// NormalizeStatus maps any input to a known Status, defaulting the empty/unknown
// value to StatusOpen so pre-field records and blank inputs read as open.
func NormalizeStatus(s Status) Status {
	if s == StatusClosed {
		return StatusClosed
	}
	return StatusOpen
}

// Project is one first-class project entity (docs/specs/2026-08-28-project-centric-ui.md
// Phase 1), keyed by its stable ID in ScrivaDB. It is the parent that agents and
// pipelines group under via their ProjectID back-ref.
//
// A worktree is NOT a project of its own: an agent running in a git worktree links
// to this parent project's ID and carries its own worktree path on the session
// record (store.Session.Worktree) — projects are repo-level, one per checkout root
// or remote, never one per worktree.
type Project struct {
	// ID is the canonical, stable project key: the local absolute path of the main
	// checkout, or the remote URL. Every agent/pipeline in any worktree of this repo
	// shares this one ID.
	ID string `json:"id"`
	// Name is the human-friendly display name.
	Name string `json:"name"`
	// Path is the local absolute path of the project's main checkout in the
	// workspace (may be empty for a remote not yet cloned — filled in by Phase 2).
	Path string `json:"path"`
	// Status is Open or Closed (hibernated).
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
