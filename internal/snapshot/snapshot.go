// Package snapshot implements warden's checkpoint system (FUTURE_ENHANCEMENTS
// #46): a non-destructive capture of an agent's worktree state AND its session
// transcript at a known-good point, plus a guarded restore. It mirrors the
// lifecycle git-ops style — compact result structs, worktree-pinned operations,
// rails against destructive states — and reuses lifecycle.Runner as its single
// command seam so the git/tmux calls are mocked the same way in tests.
//
// Capture is reversible-safe by construction: `git stash create` builds a commit
// object recording the working tree WITHOUT touching it (no stash entry pushed,
// no index change), so a snapshot never perturbs the agent it checkpoints.
// Restore re-applies that stash commit (`git stash apply <sha>`) onto the
// recorded worktree, refusing a dirty tree (unless forced) and never running on a
// protected branch — the same guards lifecycle.Sync/RemoveWorktree enforce.
package snapshot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/lifecycle"
)

// opTimeout bounds every git/tmux subprocess a capture or restore runs. A stuck
// git or a wedged tmux pane must never block the daemon goroutine indefinitely;
// 5s is generous for the local, single-repo operations involved.
const opTimeout = 5 * time.Second

// ErrDirtyWorktree is returned by Restore when the target worktree has
// uncommitted changes and force was not set — applying a snapshot's stash over
// live edits could clobber them, so warden refuses (mirrors lifecycle's sync
// dirty-tree rail). The daemon maps it to a 409 with a "use --force" hint.
var ErrDirtyWorktree = errors.New("worktree has uncommitted changes (use --force to restore anyway)")

// Snapshot is one persisted checkpoint: the worktree's git state (HEAD + a
// non-destructive stash commit capturing any dirty changes) and a pointer to the
// captured session transcript. It is the compact record `wd snapshot list`
// returns, newest first.
type Snapshot struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id,omitempty"` // owning agent (empty for a bare-dir capture)
	Message   string    `json:"message,omitempty"`    // optional operator label
	CreatedAt time.Time `json:"created_at"`
	Workdir   string    `json:"workdir"` // absolute worktree the snapshot was taken in (restore target)
	Branch    string    `json:"branch"`  // branch HEAD was on at capture
	HeadSHA   string    `json:"head_sha"`
	// StashSHA is the `git stash create` commit object capturing the working-tree
	// changes. Empty when the tree was clean at capture (nothing to stash) — restore
	// then only reports the recorded HEAD, applying no patch.
	StashSHA        string   `json:"stash_sha,omitempty"`
	DirtyFiles      []string `json:"dirty_files,omitempty"`     // paths with uncommitted changes at capture
	TranscriptPath  string   `json:"transcript_path,omitempty"` // saved transcript blob (empty if none captured)
	TranscriptLines int      `json:"transcript_lines,omitempty"`
}

// RestoreResult is the compact struct `wd snapshot restore` returns. A clean
// re-apply sets Applied=true with no Conflicts; a stash that applies with
// conflicts leaves them in the tree for the operator (and Claude) to resolve, the
// same deterministic-detect handoff `wd sync` uses.
type RestoreResult struct {
	SnapshotID     string   `json:"snapshot_id"`
	SessionID      string   `json:"session_id,omitempty"`
	Workdir        string   `json:"workdir"`
	Branch         string   `json:"branch"`
	SnapshotHead   string   `json:"snapshot_head"`
	CurrentHead    string   `json:"current_head"`
	HeadMatch      bool     `json:"head_match"` // current HEAD equals the recorded HEAD
	Applied        bool     `json:"applied"`    // the stash patch was re-applied cleanly
	Conflicts      []string `json:"conflicts,omitempty"`
	TranscriptPath string   `json:"transcript_path,omitempty"`
	Message        string   `json:"message,omitempty"`
}

// Manager captures, lists, and restores snapshots. It pairs the command seam
// (run) with the persistence layer (store); the daemon constructs one with a
// real ExecRunner + an on-disk Store, tests with a FakeRunner + a temp Store.
type Manager struct {
	run   lifecycle.Runner
	store *Store
}

// New builds a Manager over a command runner and a snapshot store.
func New(run lifecycle.Runner, store *Store) *Manager { return &Manager{run: run, store: store} }

// CaptureRequest is the resolved input to Capture: the worktree to checkpoint,
// the owning agent (for list filtering + transcript capture), and an optional
// label. The daemon fills it from the pinned session + workdir.
type CaptureRequest struct {
	SessionID   string
	Workdir     string
	TmuxSession string // tmux target for transcript capture ("" = skip transcript)
	Message     string
}

// Capture records the worktree's state and session transcript without mutating
// either. It reads HEAD + branch, builds a non-destructive stash commit of any
// dirty changes (`git stash create`), lists the dirty paths, grabs the tmux
// scrollback as the transcript, and persists the metadata + transcript blob.
func (m *Manager) Capture(ctx context.Context, req CaptureRequest) (*Snapshot, error) {
	if strings.TrimSpace(req.Workdir) == "" {
		return nil, fmt.Errorf("snapshot capture: no worktree to snapshot")
	}
	branch := m.gitOut(ctx, req.Workdir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return nil, fmt.Errorf("not a git repository: %s", req.Workdir)
	}
	head := m.gitOut(ctx, req.Workdir, "rev-parse", "HEAD")
	// git stash create builds (but does not store or push) a commit object for the
	// current working tree + index. It is the load-bearing non-destructive primitive:
	// the agent's tree is untouched, yet we hold a SHA that fully reconstructs it.
	stash, err := m.git(ctx, req.Workdir, "stash", "create", "warden snapshot")
	if err != nil {
		return nil, fmt.Errorf("git stash create: %w: %s", err, strings.TrimSpace(stash))
	}
	status, err := m.git(ctx, req.Workdir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(status))
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{
		ID:         id,
		SessionID:  req.SessionID,
		Message:    req.Message,
		CreatedAt:  time.Now().UTC(),
		Workdir:    req.Workdir,
		Branch:     branch,
		HeadSHA:    strings.TrimSpace(head),
		StashSHA:   strings.TrimSpace(stash),
		DirtyFiles: parsePorcelainPaths(status),
	}
	// Transcript is best-effort: a missing/closed tmux pane must not fail the
	// snapshot (the git state is the load-bearing half). Captured with -S - to grab
	// the full scrollback, not just the visible screen.
	transcript := ""
	if req.TmuxSession != "" {
		if out, err := m.tmux(ctx, "capture-pane", "-p", "-S", "-", "-t", req.TmuxSession); err == nil {
			transcript = out
		}
	}
	if err := m.store.Put(snap, transcript); err != nil {
		return nil, err
	}
	return snap, nil
}

// List returns snapshots newest-first. A non-empty sessionID filters to one
// agent's snapshots; "" returns all.
func (m *Manager) List(_ context.Context, sessionID string) ([]*Snapshot, error) {
	return m.store.List(sessionID)
}

// Restore re-applies a snapshot's captured worktree state onto its recorded
// worktree. It refuses a protected branch and (unless force) a dirty tree, then
// applies the stash commit — reversible-safe: stash apply neither resets HEAD nor
// drops the stash, so a restore is purely additive and the snapshot stays usable.
// The transcript path is surfaced for the operator regardless.
func (m *Manager) Restore(ctx context.Context, id string, force bool) (*RestoreResult, error) {
	snap, err := m.store.Get(id)
	if err != nil {
		return nil, err
	}
	dir := snap.Workdir
	branch := m.gitOut(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return nil, fmt.Errorf("not a git repository: %s", dir)
	}
	// Never reconstruct working state directly onto an integration branch — an
	// agent restores onto its own branch, mirroring the commit/push/sync rails.
	if lifecycle.IsProtectedBranch(branch) {
		return nil, fmt.Errorf("refusing to restore onto protected branch %q — restore onto an agent branch", branch)
	}
	status, err := m.git(ctx, dir, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(status))
	}
	if strings.TrimSpace(status) != "" && !force {
		return nil, ErrDirtyWorktree
	}
	cur := strings.TrimSpace(m.gitOut(ctx, dir, "rev-parse", "HEAD"))
	res := &RestoreResult{
		SnapshotID:     snap.ID,
		SessionID:      snap.SessionID,
		Workdir:        dir,
		Branch:         branch,
		SnapshotHead:   snap.HeadSHA,
		CurrentHead:    cur,
		HeadMatch:      cur == snap.HeadSHA,
		TranscriptPath: snap.TranscriptPath,
		Message:        snap.Message,
	}
	if snap.StashSHA == "" {
		// Clean capture: there was no working-tree patch to re-apply. The HEAD report
		// is the whole restore (Applied stays false).
		return res, nil
	}
	out, err := m.git(ctx, dir, "stash", "apply", snap.StashSHA)
	if err != nil {
		conflicts := m.unmergedPaths(ctx, dir)
		if len(conflicts) == 0 {
			return nil, fmt.Errorf("git stash apply %s: %w: %s", snap.StashSHA, err, strings.TrimSpace(out))
		}
		// Conflicts are left in the tree for resolution (Claude resolves the hunks),
		// the same deterministic-detect handoff wd sync uses — not a hard error.
		res.Conflicts = conflicts
		return res, nil
	}
	res.Applied = true
	return res, nil
}

// git runs a git subprocess in dir under opTimeout.
func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return m.run.Run(cctx, dir, "git", args...)
}

// gitOut runs a git subprocess and returns its trimmed stdout, or "" on error —
// for the best-effort reads (branch, HEAD) where an error degrades gracefully.
func (m *Manager) gitOut(ctx context.Context, dir string, args ...string) string {
	out, err := m.git(ctx, dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// tmux runs a tmux subprocess under opTimeout (no working dir).
func (m *Manager) tmux(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return m.run.Run(cctx, "", "tmux", args...)
}

// unmergedPaths lists files left with merge conflicts after a stash apply
// (best-effort, nil on error) — the deterministic conflict set handed back.
func (m *Manager) unmergedPaths(ctx context.Context, dir string) []string {
	out, err := m.git(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	return nonEmptyLines(out)
}

// newID returns a sortable-enough, collision-resistant snapshot id of the form
// "snap-<8hex>". Ordering is by CreatedAt (see Store.List), so the id only needs
// to be unique, not monotonic.
func newID() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate snapshot id: %w", err)
	}
	return "snap-" + hex.EncodeToString(b), nil
}

// parsePorcelainPaths extracts changed paths from `git status --porcelain`. Each
// line is "XY <path>" (or "XY <orig> -> <new>" for a rename — the post-rename path
// is the one that landed). Mirrors lifecycle's parser; kept local so the snapshot
// package stays independent and the parser is unit-tested here.
func parsePorcelainPaths(porcelain string) []string {
	var paths []string
	for _, line := range nonEmptyLines(porcelain) {
		if len(line) < 4 {
			continue
		}
		p := strings.TrimSpace(line[3:])
		if idx := strings.Index(p, " -> "); idx >= 0 {
			p = p[idx+len(" -> "):]
		}
		paths = append(paths, p)
	}
	return paths
}

// nonEmptyLines splits s on newlines and drops blank lines.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// countLines returns the number of newline-separated lines in s (0 for empty).
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\n"), "\n") + 1
}
