// Package collab provides inter-agent file-conflict detection: a daemon-side
// monitor tracks which files each active agent has touched and warns when two
// or more agents overlap on the same path.
//
// Detection is fsnotify-first: edits record dirty paths in memory and conflict
// checks compare those sets without spawning git. A slower git-diff reconcile
// loop refreshes state after commits/reverts and when events are missed. The
// watcher degrades cleanly to pure git polling when fsnotify is unavailable or
// the inotify budget is exhausted.
//
// Design: docs/specs/2026-09-03-collab-fsnotify-first-detection.md
package collab

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	// daemonSender is the reserved provenance id stamped on monitor warnings.
	daemonSender = "daemon"
	// watchDebounce coalesces a burst of filesystem events into a single rescan.
	watchDebounce = 300 * time.Millisecond
)

// Lister is the slice of the session store the monitor needs.
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
	diff  func(ctx context.Context, worktree string) []string
	watch *watcher

	dirtyMu sync.RWMutex
	dirty   map[string]map[string]struct{}

	cacheMu    sync.RWMutex
	cached     []Conflict
	cacheValid bool

	mu    sync.Mutex
	dedup map[string]time.Time
}

// NewMonitor returns a Monitor backed by the session store and mailbox.
func NewMonitor(st Lister, mbox *mailbox.Store) *Monitor {
	return &Monitor{
		store: st,
		mbox:  mbox,
		diff:  gitDiffFiles,
		dedup: map[string]time.Time{},
		dirty: map[string]map[string]struct{}{},
	}
}

// Run scans until ctx is cancelled. interval is the watch-reconcile and
// in-memory scan cadence; gitReconcile controls git-diff refresh when fsnotify
// is active (default 2m when zero or negative).
func (m *Monitor) Run(ctx context.Context, interval, gitReconcile time.Duration) {
	if interval <= 0 {
		return
	}
	if gitReconcile <= 0 {
		gitReconcile = defaultGitReconcileInterval
	}

	fsnotifyOn := false
	if w, err := newWatcher(); err != nil {
		slog.Warn("collab: real-time watcher unavailable, polling only", "err", err)
	} else {
		m.watch = w
		fsnotifyOn = true
		defer w.Close()
		m.reconcileWatches(ctx)
		go m.watchLoop(ctx)
	}

	m.gitReconcile(ctx)
	m.refreshAndWarn(ctx)

	pollTicker := time.NewTicker(interval)
	defer pollTicker.Stop()

	var gitC <-chan time.Time
	if fsnotifyOn {
		gitTicker := time.NewTicker(gitReconcile)
		defer gitTicker.Stop()
		gitC = gitTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-pollTicker.C:
			m.reconcileWatches(ctx)
			if !fsnotifyOn {
				m.gitReconcile(ctx)
			}
			m.refreshAndWarn(ctx)
		case <-gitC:
			m.gitReconcile(ctx)
			m.refreshAndWarn(ctx)
		}
	}
}

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
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) != 0 {
				m.noteFileChange(ev.Name)
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
			m.refreshAndWarn(ctx)
		}
	}
}

func (m *Monitor) noteFileChange(absPath string) {
	if m.watch == nil || absPath == "" {
		return
	}
	info, err := os.Stat(absPath)
	if err == nil && info.IsDir() {
		return
	}
	root := m.watch.worktreeFor(absPath)
	if root == "" {
		return
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil || strings.HasPrefix(rel, "..") || rel == "." {
		return
	}
	rel = filepath.ToSlash(rel)
	if rel == ".git" || strings.HasPrefix(rel, ".git/") {
		return
	}
	m.addDirty(root, rel)
}

func (m *Monitor) addDirty(worktree, rel string) {
	m.dirtyMu.Lock()
	defer m.dirtyMu.Unlock()
	set, ok := m.dirty[worktree]
	if !ok {
		set = map[string]struct{}{}
		m.dirty[worktree] = set
	}
	set[rel] = struct{}{}
}

func (m *Monitor) setDirty(worktree string, files []string) {
	m.dirtyMu.Lock()
	defer m.dirtyMu.Unlock()
	if len(files) == 0 {
		delete(m.dirty, worktree)
		return
	}
	set := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f != "" {
			set[f] = struct{}{}
		}
	}
	m.dirty[worktree] = set
}

func (m *Monitor) dirtyFiles(worktree string) []string {
	m.dirtyMu.RLock()
	defer m.dirtyMu.RUnlock()
	set := m.dirty[worktree]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func (m *Monitor) gitReconcile(ctx context.Context) {
	sessions, err := m.store.List(ctx)
	if err != nil {
		slog.Warn("collab: git reconcile: list sessions failed", "err", err)
		return
	}
	active := map[string]struct{}{}
	for _, s := range sessions {
		if !tracked(s) {
			continue
		}
		active[s.Worktree] = struct{}{}
		m.setDirty(s.Worktree, m.diff(ctx, s.Worktree))
	}
	m.dirtyMu.Lock()
	for wt := range m.dirty {
		if _, ok := active[wt]; !ok {
			delete(m.dirty, wt)
		}
	}
	m.dirtyMu.Unlock()
}

func (m *Monitor) refreshAndWarn(ctx context.Context) {
	if err := m.refreshConflicts(ctx); err != nil {
		slog.Warn("collab: conflict scan failed", "err", err)
		return
	}
	m.pruneDedup()
	for _, c := range m.cachedSnapshot() {
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

// Conflicts returns the cached conflict snapshot so API polls do not spawn git.
func (m *Monitor) Conflicts(ctx context.Context) ([]Conflict, error) {
	if snap, ok := m.cachedSnapshotIfValid(); ok {
		return snap, nil
	}
	if err := m.refreshConflicts(ctx); err != nil {
		return nil, err
	}
	return m.cachedSnapshot(), nil
}

func (m *Monitor) refreshConflicts(ctx context.Context) error {
	sessions, err := m.store.List(ctx)
	if err != nil {
		return err
	}
	fileAgents := map[string][]AgentInfo{}
	for _, s := range sessions {
		if !tracked(s) {
			continue
		}
		for _, f := range m.filesForWorktree(ctx, s.Worktree) {
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

	m.cacheMu.Lock()
	m.cached = conflicts
	m.cacheValid = true
	m.cacheMu.Unlock()
	return nil
}

func (m *Monitor) filesForWorktree(ctx context.Context, worktree string) []string {
	if files := m.dirtyFiles(worktree); len(files) > 0 {
		return files
	}
	if m.watch == nil {
		return m.diff(ctx, worktree)
	}
	return nil
}

func (m *Monitor) cachedSnapshotIfValid() ([]Conflict, bool) {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	if !m.cacheValid {
		return nil, false
	}
	return copyConflicts(m.cached), true
}

func (m *Monitor) cachedSnapshot() []Conflict {
	m.cacheMu.RLock()
	defer m.cacheMu.RUnlock()
	return copyConflicts(m.cached)
}

func copyConflicts(in []Conflict) []Conflict {
	if len(in) == 0 {
		return nil
	}
	out := make([]Conflict, len(in))
	copy(out, in)
	return out
}

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
