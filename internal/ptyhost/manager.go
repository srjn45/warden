package ptyhost

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/tmuxproc"
)

// Config builds a Manager.
type Config struct {
	// Mode is the initial `runtime.pty` mode (SetMode changes it live; a mode
	// applies to sessions created after the change).
	Mode Mode
	// Dir is the runtime's data directory (<data_dir>/pty). Direct sessions
	// journal under Dir/direct/<name>, shadow mirrors under Dir/shadow/<name>.
	Dir string
	// Run executes tmux (shadow pipe-pane / capture / probes).
	Run tmuxproc.Runner
	// Tmux is the legacy pane host (default tmuxproc.New(Run)).
	Tmux      tmuxproc.Host
	Retention ptyruntime.Retention
	Options   ptyruntime.Options
	// Shell overrides the direct-terminal shell (tests); default $SHELL.
	Shell []string
	// ReconcileEvery is how often dead shadow panes are reaped (default 2s).
	ReconcileEvery time.Duration
}

// Observer receives every journaled event of every runtime-backed session,
// in seq order per session (overlap after a resubscribe is already dropped).
// name is the tmux/session name. It runs on the session's consumer goroutine
// and must not block.
type Observer func(name string, ev ptyevent.Event)

// Manager owns the PTY runtime for the daemon.
type Manager struct {
	mode   atomic.Value // Mode
	dir    string
	run    tmuxproc.Runner
	tmux   tmuxproc.Host
	direct *ptyruntime.Host
	shadow *Shadow
	every  time.Duration
	acks   acks

	omu       sync.RWMutex
	observers []Observer

	wmu      sync.Mutex
	watching map[string]*watch
	wg       sync.WaitGroup
	closed   bool
}

type watch struct {
	sub  *ptyruntime.Subscription
	stop chan struct{}
}

// Stream is a journaled session the §6 transport can serve.
type Stream interface {
	ID() string
	Epoch() uint64
	Journal() *ptyruntime.Journal
	Subscribe(ptyevent.Cursor) *ptyruntime.Subscription
}

// Target is what Lookup resolves a session name to.
type Target struct {
	Name         string
	Presentation ptyevent.Presentation
	Stream       Stream
	// Direct is the warden-owned PTY session (nil for a shadow-mirrored tmux
	// pane, which is observe-only).
	Direct *ptyruntime.Session
}

// New builds a Manager. It starts nothing until sessions are created.
func New(cfg Config) *Manager {
	if cfg.Tmux == nil {
		cfg.Tmux = tmuxproc.New(cfg.Run)
	}
	if cfg.ReconcileEvery <= 0 {
		cfg.ReconcileEvery = 2 * time.Second
	}
	m := &Manager{dir: cfg.Dir, run: cfg.Run, tmux: cfg.Tmux, every: cfg.ReconcileEvery, watching: map[string]*watch{},
		direct: &ptyruntime.Host{JournalDir: filepath.Join(cfg.Dir, "direct"), Shell: cfg.Shell,
			Retention: cfg.Retention, Options: cfg.Options},
		shadow: newShadow(cfg.Run, filepath.Join(cfg.Dir, "shadow"), cfg.Retention, cfg.Options)}
	m.SetMode(cfg.Mode)
	return m
}

// Mode is the current mode.
func (m *Manager) Mode() Mode { return m.mode.Load().(Mode) }

// SetMode switches the mode for sessions created from now on. Turning it off
// also stops every shadow mirror (the panes keep running in tmux); direct
// terminals keep running on their PTY until they end.
func (m *Manager) SetMode(mode Mode) {
	mode = ParseMode(string(mode))
	prev, _ := m.mode.Swap(mode).(Mode)
	if prev != "" && prev != mode {
		slog.Info("ptyhost: runtime.pty mode changed (applies to new sessions)", "from", prev, "to", mode)
	}
	if mode == ModeOff && prev.mirrors() {
		for _, name := range m.shadow.Names() {
			m.unwatch(name)
			m.shadow.Stop(context.Background(), name, nil)
		}
	}
}

// Observe registers an observer for every runtime-backed session.
func (m *Manager) Observe(o Observer) {
	m.omu.Lock()
	m.observers = append(m.observers, o)
	m.omu.Unlock()
}

// Host is the routed tmuxproc.Host for every pane: creation goes to tmux
// (mirrored in shadow/on), every other call goes to whichever host owns the
// name.
func (m *Manager) Host() tmuxproc.Host { return routed{m: m} }

// TerminalHost is Host, except that in mode on a new plain-shell terminal is
// created on a direct warden-owned PTY.
func (m *Manager) TerminalHost() tmuxproc.Host { return routed{m: m, terminal: true} }

// Lookup resolves a session name to its journaled stream.
func (m *Manager) Lookup(name string) (Target, bool) {
	if s := m.direct.Session(name); s != nil {
		return Target{Name: name, Presentation: ptyevent.PresentationDirect, Stream: s, Direct: s}, true
	}
	if mr := m.shadow.Mirror(name); mr != nil {
		return Target{Name: name, Presentation: ptyevent.PresentationTmux, Stream: mr}, true
	}
	return Target{}, false
}

// Direct returns the direct PTY session for name (nil if it is not hosted
// directly).
func (m *Manager) Direct(name string) *ptyruntime.Session { return m.direct.Session(name) }

// HasDirectJournal reports whether name was ever hosted on a direct PTY: its
// journal is what a restart recovers into (the next epoch).
func (m *Manager) HasDirectJournal(name string) bool {
	fi, err := os.Stat(filepath.Join(m.dir, "direct", name))
	return err == nil && fi.IsDir()
}

// RecoverShadow re-adopts the live tmux panes among names that were mirrored
// before (daemon restart): each opens a new epoch with lifecycle{recovered},
// marker{reset} and a snapshot seeded from the live screen. A no-op when the
// mode does not mirror.
func (m *Manager) RecoverShadow(ctx context.Context, names []string) int {
	if !m.Mode().mirrors() {
		return 0
	}
	n := 0
	for _, name := range names {
		if !m.shadow.HasJournal(name) || m.shadow.Mirror(name) != nil || !m.tmux.HasSession(ctx, name) {
			continue
		}
		mr, err := m.shadow.Start(ctx, name, true)
		if err != nil {
			slog.Warn("ptyhost: re-adopt shadow pane failed", "session", name, "err", err)
			continue
		}
		m.watchStream(name, mr)
		n++
	}
	return n
}

// Run reaps shadow mirrors whose tmux pane is gone until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reconcile(ctx)
		}
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	for _, name := range m.shadow.Names() {
		if !m.tmux.HasSession(ctx, name) {
			m.endShadow(ctx, name, "tmux session ended")
		}
	}
}

func (m *Manager) endShadow(ctx context.Context, name, detail string) {
	m.shadow.Stop(ctx, name, &ptyevent.Lifecycle{Phase: ptyevent.PhaseExited, Detail: detail})
	m.unwatch(name)
}

// Close ends every direct session (journaling its exit) and stops every
// shadow mirror (leaving the tmux panes and their epochs open for
// re-adoption), then waits for every consumer.
func (m *Manager) Close() {
	m.wmu.Lock()
	m.closed = true
	m.wmu.Unlock()
	m.shadow.Close(context.Background())
	m.direct.Close()
	m.wmu.Lock()
	for name, w := range m.watching {
		close(w.stop)
		delete(m.watching, name)
	}
	m.wmu.Unlock()
	m.wg.Wait()
}

// --- observers ---------------------------------------------------------------

// watchStream runs one consumer per session: it tails the journal from the
// current head and hands every event to the observers, resubscribing from its
// cursor when it falls behind (ErrResync). It ends when the stream closes.
func (m *Manager) watchStream(name string, st Stream) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if m.closed {
		return
	}
	if old, ok := m.watching[name]; ok {
		close(old.stop)
	}
	w := &watch{stop: make(chan struct{})}
	m.watching[name] = w
	j := st.Journal()
	cur := ptyevent.Cursor{SessionID: j.SessionID(), Epoch: j.Last().Epoch, Seq: j.Last().Seq}
	m.wg.Add(1)
	go m.consume(name, st, cur, w.stop)
}

func (m *Manager) consume(name string, st Stream, cur ptyevent.Cursor, stop <-chan struct{}) {
	defer m.wg.Done()
	for {
		sub := st.Subscribe(cur)
		for open := true; open; {
			select {
			case ev, ok := <-sub.C:
				if !ok {
					open = false
					break
				}
				if ev.Seq <= cur.Seq {
					continue
				}
				cur = ptyevent.Cursor{SessionID: ev.SessionID, Epoch: ev.Epoch, Seq: ev.Seq}
				m.dispatch(name, ev)
			case <-stop:
				sub.Close()
				return
			}
		}
		if !errors.Is(sub.Err(), ptyruntime.ErrResync) {
			return
		}
	}
}

func (m *Manager) dispatch(name string, ev ptyevent.Event) {
	m.omu.RLock()
	obs := m.observers
	m.omu.RUnlock()
	for _, o := range obs {
		o(name, ev)
	}
}

func (m *Manager) unwatch(name string) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if w, ok := m.watching[name]; ok {
		close(w.stop)
		delete(m.watching, name)
	}
}

// --- routed tmuxproc.Host ------------------------------------------------------

type routed struct {
	m        *Manager
	terminal bool
}

var _ tmuxproc.Host = routed{}

// directEnv is added to a direct terminal: the daemon (launchd/systemd) often
// has no TERM, and the rendering end is always an xterm-compatible client.
var directEnv = []string{"TERM=xterm-256color"}

func (r routed) NewSession(ctx context.Context, name, cwd string, env ...string) error {
	m := r.m
	mode := m.Mode()
	if r.terminal && mode == ModeOn {
		if err := m.direct.NewSession(ctx, name, cwd, append(append([]string{}, directEnv...), env...)...); err != nil {
			return err
		}
		if s := m.direct.Session(name); s != nil {
			m.watchStream(name, s)
		}
		return nil
	}
	if m.direct.Session(name) != nil {
		// A dead direct session under this name (the mode changed since it ran):
		// release it so routing falls through to the new tmux pane.
		m.unwatch(name)
		_ = m.direct.KillSession(ctx, name)
	}
	if err := m.tmux.NewSession(ctx, name, cwd, env...); err != nil {
		return err
	}
	if mode.mirrors() {
		// Best-effort: shadowing must never block or fail a launch.
		if mr, err := m.shadow.Start(ctx, name, false); err != nil {
			slog.Warn("ptyhost: shadow mirror failed; pane runs unmirrored", "session", name, "err", err)
		} else {
			m.watchStream(name, mr)
		}
	}
	return nil
}

func (r routed) KillSession(ctx context.Context, name string) error {
	m := r.m
	if m.direct.Session(name) != nil {
		m.unwatch(name)
		return m.direct.KillSession(ctx, name)
	}
	err := m.tmux.KillSession(ctx, name)
	if m.shadow.Mirror(name) != nil {
		m.endShadow(ctx, name, "killed")
	}
	return err
}

func (r routed) HasSession(ctx context.Context, name string) bool {
	if r.m.direct.HasSession(ctx, name) {
		return true
	}
	if r.m.direct.Session(name) != nil {
		return false // hosted directly, process gone: never fall back to a same-named tmux session
	}
	return r.m.tmux.HasSession(ctx, name)
}

func (r routed) CapturePane(ctx context.Context, name string) (string, error) {
	if r.m.direct.Session(name) != nil {
		return r.m.direct.CapturePane(ctx, name)
	}
	return r.m.tmux.CapturePane(ctx, name)
}

func (r routed) SendKeys(ctx context.Context, name string, keys ...string) error {
	if r.m.direct.Session(name) != nil {
		return r.m.direct.SendKeys(ctx, name, keys...)
	}
	return r.m.tmux.SendKeys(ctx, name, keys...)
}
