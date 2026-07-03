// Package curate is warden's gated auto-curation of the backend-neutral project
// memory (#53 PR-2, design §4.2). On the EXISTING completion-digest hook it runs a
// debounced, extraction-not-dump pass that PROPOSES durable, reusable facts into the
// repo's committed .warden/memory.md as UNVERIFIED, timestamped, provenance-tagged
// entries.
//
// It is deliberately conservative and never authoritative:
//   - proposals start `unverified` — a hint, promoted to `trusted` only by
//     corroboration (a second agent's sighting) or a human approving the diff;
//   - a contradicting proposal SUPERSEDES an older entry (struck with a tombstone);
//     un-recorroborated entries age out past a TTL; a fact whose named path vanished
//     is flagged stale by a deterministic check;
//   - the pass writes the WORKING TREE only. It NEVER commits and NEVER pushes — the
//     committed diff is the human gate that stops one agent's wrong belief from
//     poisoning the whole fleet.
//
// Every external dependency (repo-root resolution, the summarization LLM, file
// writes, path-existence, the debounce clock) is an injectable seam, so tests run
// hermetically with no real repo, LLM, or wall-clock — the same discipline PR-0/PR-1
// used to stub git.
package curate

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/memory"
)

// Defaults for the debounce window and the age-out TTL. The debounce coalesces a
// burst of near-simultaneous completions into ONE pass; the TTL is generous — memory
// is a background nicety and un-recorroborated hints should linger a while before
// aging out, not churn.
const (
	DefaultDebounce = 30 * time.Second
	DefaultTTL      = 45 * 24 * time.Hour
)

// Signal is the completion-time evidence one finished agent contributes to a curation
// batch — the neutral projection of a digest + its session, so this package needs no
// digest/daemon import. Provenance (Agent/Commit) is what makes a proposed entry a
// claim-with-attribution rather than anonymous authority.
type Signal struct {
	Task    string   // the agent's first real prompt
	Summary string   // the digest's 1–2 sentence narrative
	Files   []string // files it touched
	Branch  string   // branch it worked on
	Agent   string   // provenance: the agent/session id
	Commit  string   // provenance: the commit sha it produced ("" if none)
}

// ProposeInput is everything the extraction pass reads: the repo root, the current
// memory (so it can avoid re-proposing known facts and phrase supersessions), and the
// batch of completion signals coalesced by the debounce.
type ProposeInput struct {
	RepoRoot string
	Current  *memory.Memory
	Signals  []Signal
}

// Proposer extracts durable, reusable candidate facts from a completion batch. The
// production impl (LLMProposer) reuses warden's $0-preferring offload — local LLM
// first, headless claude -p only as the configured fallback — so curation adds no
// cloud spend on a critical path. Tests inject a canned proposer and stay offline.
type Proposer interface {
	Propose(ctx context.Context, in ProposeInput) ([]memory.Entry, error)
}

// stopper is the subset of *time.Timer the debouncer needs, so the clock is
// injectable in tests (no wall-clock waits).
type stopper interface{ Stop() bool }

// Curator debounces completion signals per repo and runs the curation pass. Zero
// value is NOT ready — use New. All fields after Proposer are optional seams that
// default to production behavior.
type Curator struct {
	store    *memory.Store
	proposer Proposer

	// Injectable seams (nil ⇒ production default).
	Now      func() time.Time                        // wall clock
	Write    func(path string, data []byte) error    // working-tree write
	Exists   func(abs string) bool                   // path-existence (staleness)
	After    func(d time.Duration, f func()) stopper // debounce timer
	Debounce time.Duration
	TTL      time.Duration

	// onPass, when set, is invoked after each completed pass with its repo memory
	// path and result — a test sync/observation hook (unused in production).
	onPass func(path string, r Result)

	mu      sync.Mutex
	pending map[string]*batch // keyed by the resolved .warden/memory.md path
	wg      sync.WaitGroup    // tracks in-flight passes (test sync)
}

type batch struct {
	workdir string
	signals []Signal
	timer   stopper
}

// New builds a Curator over a memory store and proposer, filling production defaults
// for every unset seam.
func New(store *memory.Store, p Proposer) *Curator {
	return &Curator{
		store:    store,
		proposer: p,
		Now:      time.Now,
		Write:    func(path string, data []byte) error { return os.WriteFile(path, data, 0o644) },
		Exists:   func(abs string) bool { _, err := os.Stat(abs); return err == nil },
		After:    func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		Debounce: DefaultDebounce,
		TTL:      DefaultTTL,
		pending:  map[string]*batch{},
	}
}

// Enqueue records one completed agent's signal and (re)arms the per-repo debounce
// timer, so a BURST of completions in the same repo coalesces into a single pass.
// Cheap and non-blocking: it resolves the repo's memory path, buffers the signal, and
// returns — the pass runs later off the timer. A repo-root resolution failure (the
// workdir isn't a git repo) is logged and dropped; curation is best-effort.
func (c *Curator) Enqueue(ctx context.Context, workdir string, sig Signal) {
	path, err := c.store.Locate(ctx, workdir)
	if err != nil {
		slog.Debug("curate: resolve memory path failed; dropping signal", "workdir", workdir, "err", err)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.pending[path]
	if b == nil {
		b = &batch{workdir: workdir}
		c.pending[path] = b
	}
	b.signals = append(b.signals, sig)
	if b.timer != nil {
		b.timer.Stop() // coalesce: restart the window on each new completion
	}
	d := c.Debounce
	if d <= 0 {
		d = DefaultDebounce
	}
	b.timer = c.After(d, func() { c.fire(path) })
}

// fire drains a repo's coalesced batch and runs one pass. Guarded so a spurious
// timer for an already-drained key is a no-op.
func (c *Curator) fire(path string) {
	c.mu.Lock()
	b := c.pending[path]
	delete(c.pending, path)
	c.mu.Unlock()
	if b == nil || len(b.signals) == 0 {
		return
	}
	c.wg.Add(1)
	defer c.wg.Done()
	c.runPass(context.Background(), b.workdir, b.signals)
}

// Wait blocks until all in-flight passes finish. Test-only sync helper.
func (c *Curator) Wait() { c.wg.Wait() }

// runPass is the curation pass itself (§4.2): load the current memory, extract
// candidate facts from the batch, MERGE them (dedup/promote, supersede-on-contradiction),
// age out un-recorroborated hints, flag paths that vanished, and — only if something
// changed — write the WORKING TREE. It never commits or pushes.
func (c *Curator) runPass(ctx context.Context, workdir string, signals []Signal) {
	m, path, _, err := c.store.Load(ctx, workdir)
	if err != nil {
		slog.Warn("curate: load memory failed; skipping pass", "workdir", workdir, "err", err)
		return
	}
	root := repoRootFromMemPath(path)

	cands, err := c.proposer.Propose(ctx, ProposeInput{RepoRoot: root, Current: m, Signals: signals})
	if err != nil {
		slog.Warn("curate: proposer failed; skipping pass", "root", root, "err", err)
		return
	}

	now := c.Now()
	r := Merge(m, cands, now)
	ttl := c.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	r.AgeOut(m, ttl, now)
	r.Stale = CheckStale(m, root, c.Exists, now)

	if !r.Changed() {
		return
	}
	if err := c.Write(path, []byte(m.Serialize())); err != nil {
		slog.Warn("curate: write memory failed", "path", path, "err", err)
		return
	}
	slog.Info("curate: proposed memory updates (uncommitted — review the diff)",
		"path", path, "added", r.Added, "superseded", r.Superseded,
		"promoted", r.Promoted, "aged_out", r.AgedOut, "stale", r.Stale)
	if c.onPass != nil {
		c.onPass(path, r)
	}
}

// repoRootFromMemPath recovers the repo root from a resolved .warden/memory.md path.
// The store builds it as filepath.Join(root, ".warden", "memory.md"), so two
// filepath.Dir hops (…/.warden/memory.md → …/.warden → …) return the root — used for
// the deterministic staleness check's path joins.
func repoRootFromMemPath(memPath string) string {
	return filepath.Dir(filepath.Dir(memPath))
}
