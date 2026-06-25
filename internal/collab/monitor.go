// Package collab provides inter-agent file-conflict detection: a daemon-side
// monitor scans each active agent's worktree with `git diff` and warns agents
// that are editing the same file as another agent.
//
// Detection runs on two cadences: an fsnotify watcher over each worktree gives
// subsecond reaction to edits, and a slower poll loop reconciles the watch set
// against the active-session view and acts as a safety net when events are
// missed or watches can't be added. The watcher degrades cleanly to pure
// polling when fsnotify is unavailable or the inotify budget is exhausted.
//
// Design: docs/superpowers/specs/2026-06-14-intelligent-inter-agent-collaboration-design.md
// (Hardened MVP + deferred FSNotify real-time detection). Warnings are
// informational, delivered through the existing mailbox, and never block an
// agent. State is in-memory and ephemeral.
package collab

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

const (
	// dedupWindow suppresses re-warning the same (agent, file) pair within this
	// span. The mailbox bounds storage growth, but not re-warn spam: without
	// this an open conflict would re-warn every tick.
	dedupWindow = 5 * time.Minute
	// gitDiffTimeout bounds each `git diff` subprocess so one wedged worktree
	// can't stall the scan.
	gitDiffTimeout = 5 * time.Second
	// daemonSender is the reserved provenance id stamped on monitor warnings.
	// Agents are blocked from forging it by daemon.sanitizeSender; daemon-internal
	// writes (like this one) are trusted by construction and call Append directly.
	daemonSender = "daemon"
	// watchDebounce coalesces a burst of filesystem events into a single rescan,
	// so saving several files at once triggers at most one conflict scan.
	watchDebounce = 300 * time.Millisecond
)

// Lister is the slice of the session store the monitor needs. store.Store
// satisfies it; tests supply a fake.
type Lister interface {
	List(ctx context.Context) ([]*store.Session, error)
}

// AgentInfo identifies an agent editing a file.
type AgentInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Conflict is one file being edited by two or more agents.
type Conflict struct {
	File   string      `json:"file"`
	Agents []AgentInfo `json:"agents"`
}

// Monitor scans active agent worktrees for file-level edit conflicts.
type Monitor struct {
	store Lister
	mbox  *mailbox.Store
	// diff returns the modified files in a worktree; overridable in tests.
	diff func(ctx context.Context, worktree string) []string

	// watch is the real-time fsnotify layer; nil when fsnotify is unavailable,
	// in which case detection runs on the poll loop alone.
	watch *watcher

	mu    sync.Mutex
	dedup map[string]time.Time // "agentID\x00file" -> last warned
}

// NewMonitor returns a Monitor backed by the session store and mailbox.
func NewMonitor(st Lister, mbox *mailbox.Store) *Monitor {
	return &Monitor{store: st, mbox: mbox, diff: gitDiffFiles, dedup: map[string]time.Time{}}
}

// Run scans until ctx is cancelled. A non-positive interval disables the
// monitor (returns immediately). When fsnotify is available, a watcher reacts
// to edits in subseconds; the interval poll reconciles the watch set against
// the active-session view and serves as a safety net regardless.
func (m *Monitor) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	// Real-time layer: best-effort. If fsnotify can't initialize (e.g. inotify
	// instances exhausted), detection degrades to the poll loop below.
	if w, err := newWatcher(); err != nil {
		slog.Warn("collab: real-time watcher unavailable, polling only", "err", err)
	} else {
		m.watch = w
		defer w.Close()
		m.reconcileWatches(ctx)
		go m.watchLoop(ctx)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileWatches(ctx)
			m.tick(ctx)
		}
	}
}

// reconcileWatches points the watcher at the current tracked-worktree set,
// adding watches for new agents and dropping them for departed ones. It is a
// no-op when the real-time layer is disabled.
func (m *Monitor) reconcileWatches(ctx context.Context) {
	if m.watch == nil {
		return
	}
	sessions, err := m.store.List(ctx)
	if err != nil {
		slog.Warn("collab: reconcile watches: list sessions failed", "err", err)
		return
	}
	var roots []string
	for _, s := range sessions {
		if tracked(s) {
			roots = append(roots, s.Worktree)
		}
	}
	m.watch.reconcile(roots)
}

// watchLoop debounces filesystem events into conflict rescans, so a burst of
// edits triggers at most one scan. Newly created directories are added to the
// watch set as they appear (inotify is not recursive). It returns when ctx is
// cancelled or the watcher's channels close.
func (m *Monitor) watchLoop(ctx context.Context) {
	timer := time.NewTimer(watchDebounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	pending := false
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-m.watch.fsw.Events():
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create == fsnotify.Create {
				m.watch.noteCreate(ev.Name)
			}
			if !pending {
				pending = true
				timer.Reset(watchDebounce)
			}
		case err, ok := <-m.watch.fsw.Errors():
			if !ok {
				return
			}
			slog.Warn("collab: watcher error", "err", err)
		case <-timer.C:
			pending = false
			m.tick(ctx)
		}
	}
}

// tick recomputes conflicts and warns each participant (subject to dedup).
func (m *Monitor) tick(ctx context.Context) {
	conflicts, err := m.Conflicts(ctx)
	if err != nil {
		slog.Warn("collab: conflict scan failed", "err", err)
		return
	}
	m.pruneDedup()
	for _, c := range conflicts {
		for _, a := range c.Agents {
			if !m.shouldWarn(a.ID, c.File) {
				continue
			}
			body := formatWarning(c.File, a.ID, c.Agents)
			if _, err := m.mbox.Append(mailbox.Message{To: a.ID, From: daemonSender, Body: body}); err != nil {
				slog.Warn("collab: deliver conflict warning failed", "agent", a.ID, "err", err)
			}
		}
	}
}

// Conflicts lists files edited by two or more tracked agents right now. It is
// recomputed on demand (cheap at warden's scale; no shared cache to invalidate).
func (m *Monitor) Conflicts(ctx context.Context) ([]Conflict, error) {
	sessions, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	fileAgents := map[string][]AgentInfo{}
	for _, s := range sessions {
		if !tracked(s) {
			continue
		}
		for _, f := range m.diff(ctx, s.Worktree) {
			fileAgents[f] = append(fileAgents[f], AgentInfo{ID: s.ID, Name: s.Name})
		}
	}
	var conflicts []Conflict
	for f, agents := range fileAgents {
		if len(agents) > 1 {
			conflicts = append(conflicts, Conflict{File: f, Agents: agents})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].File < conflicts[j].File })
	return conflicts, nil
}

// tracked reports whether a session should be scanned: it has a worktree and is
// not in a terminal state. Crucially this includes idle/waiting_for_input/
// rate_limited agents — a paused agent still holds uncommitted edits.
func tracked(s *store.Session) bool {
	if s.Worktree == "" {
		return false
	}
	switch s.Status {
	case store.StatusDone, store.StatusErrored, store.StatusOrphaned:
		return false
	}
	return true
}

// gitDiffFiles returns the repo-relative paths modified in worktree (vs HEAD).
// Any error — missing/GC'd worktree, timeout, not-a-repo — yields no files, so
// that worktree is simply skipped this tick.
func gitDiffFiles(ctx context.Context, worktree string) []string {
	cctx, cancel := context.WithTimeout(ctx, gitDiffTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "git", "-C", worktree, "diff", "--name-only", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

// shouldWarn reports whether (agentID, file) is outside its dedup window, and
// records the warning time when it is.
func (m *Monitor) shouldWarn(agentID, file string) bool {
	key := agentID + "\x00" + file
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.dedup[key]; ok && now.Sub(last) < dedupWindow {
		return false
	}
	m.dedup[key] = now
	return true
}

// pruneDedup drops dedup entries older than the window so the map can't grow
// unbounded across a long-lived daemon.
func (m *Monitor) pruneDedup() {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, t := range m.dedup {
		if now.Sub(t) >= dedupWindow {
			delete(m.dedup, k)
		}
	}
}

// formatWarning builds the inbox message sent to selfID about a shared file.
func formatWarning(file, selfID string, agents []AgentInfo) string {
	var others []string
	for _, a := range agents {
		if a.ID == selfID {
			continue
		}
		if a.Name != "" {
			others = append(others, fmt.Sprintf("%s (%s)", a.ID, a.Name))
		} else {
			others = append(others, a.ID)
		}
	}
	return fmt.Sprintf("⚠️  File conflict: %s\nAlso being edited by: %s\nCoordinate before committing to avoid a merge conflict.",
		file, strings.Join(others, ", "))
}
