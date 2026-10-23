package ptyhost

import (
	"context"
	"errors"
	"fmt"
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
	// Launcher, when set, hosts every direct session in an independent PTY holder
	// process (internal/ptyholder) so the shell survives a daemon restart,
	// update or crash and is re-adopted. nil keeps the shell a child of the
	// daemon (plan 01 behavior).
	Launcher ptyruntime.Launcher
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
	mode        atomic.Value // Mode
	dir         string
	run         tmuxproc.Runner
	tmux        tmuxproc.Host
	direct      *ptyruntime.Host
	agentDirect atomic.Bool
	shadow      *Shadow
	every       time.Duration
	acks        acks

	omu       sync.RWMutex
	observers []observer

	// kind records which sessions are plain shells (kmu guards both fields).
	// Shell/AI is decided by how a session was created or by its record — never
	// by its name or screen — and AI-only observers (the state classifier and
	// everything behind it) are never shown a shell's events.
	kmu      sync.Mutex
	kinds    map[string]bool // name -> isTerminal (cached lookups + explicit marks)
	kindFunc func(name string) bool

	rmu            sync.RWMutex
	recordResolver RecordResolver

	// RollbackRecord records a runtime mode rollback event (global or cohort-scoped).
	// Rollback affects future launches only; live sessions keep their runtime and control.
	pmu       sync.RWMutex
	pins      map[string]Mode
	cohorts   map[string]Mode
	cohortOf  map[string]string
	rollbacks []RollbackRecord

	wmu      sync.Mutex
	watching map[string]*watch
	wg       sync.WaitGroup
	closed   bool
}

// RollbackRecord records a runtime mode rollback event (global or cohort-scoped).
// Rollback affects future launches only; live sessions keep their runtime and control.
type RollbackRecord struct {
	Cohort string    `json:"cohort,omitempty"`
	From   Mode      `json:"from"`
	To     Mode      `json:"to"`
	At     time.Time `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

type watch struct {
	sub  *ptyruntime.Subscription
	stop chan struct{}
}

// Stream is a journaled session the §6 transport and classifiers can serve.
type Stream interface {
	ID() string
	Epoch() uint64
	Journal() *ptyruntime.Journal
	Subscribe(ptyevent.Cursor) *ptyruntime.Subscription
	Screen() string
	Emit(ptyevent.Event) (uint64, error)
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
		pins: make(map[string]Mode), cohorts: make(map[string]Mode), cohortOf: make(map[string]string),
		direct: &ptyruntime.Host{JournalDir: filepath.Join(cfg.Dir, "direct"), Shell: cfg.Shell,
			Retention: cfg.Retention, Options: cfg.Options, Launcher: cfg.Launcher},
		shadow: newShadow(cfg.Run, filepath.Join(cfg.Dir, "shadow"), cfg.Retention, cfg.Options)}
	m.SetMode(cfg.Mode)
	return m
}

// Mode is the current mode.
// Dir is the runtime's data directory (journals and learned-prompt state live under it).
func (m *Manager) Dir() string { return m.dir }

func (m *Manager) Mode() Mode { return m.mode.Load().(Mode) }

// SetMode switches the mode for sessions created from now on. Turning it off
// also stops every shadow mirror (the panes keep running in tmux); direct
// terminals keep running on their PTY until they end.
func (m *Manager) SetMode(mode Mode) {
	mode = ParseMode(string(mode))
	prev, _ := m.mode.Swap(mode).(Mode)
	if prev != "" && prev != mode {
		slog.Info("ptyhost: runtime.pty mode changed (applies to new sessions)", "from", prev, "to", mode)
		if isRollback(prev, mode) {
			m.RecordRollback("", prev, mode, "global runtime.pty mode change")
		}
	}
	if mode == ModeOff && prev.mirrors() {
		for _, name := range m.shadow.Names() {
			m.unwatch(name)
			m.shadow.Stop(context.Background(), name, nil)
		}
	}
}

type observer struct {
	fn         Observer
	agentsOnly bool
}

// Observe registers an observer for every runtime-backed session, shells
// included. Use it only for lifecycle bookkeeping that is kind-neutral.
func (m *Manager) Observe(o Observer) {
	m.omu.Lock()
	m.observers = append(m.observers, observer{fn: o})
	m.omu.Unlock()
}

// ObserveAgents registers an observer that never sees plain-shell terminals.
// Anything that classifies output, detects prompts or may type on a session's
// behalf must register here: a shell is human-controlled and its output must
// never reach an AI permission/strategic path.
func (m *Manager) ObserveAgents(o Observer) {
	m.omu.Lock()
	m.observers = append(m.observers, observer{fn: o, agentsOnly: true})
	m.omu.Unlock()
}

// SetTerminalResolver tells the manager how to recognise an existing plain
// terminal that was not created in this process (daemon restart): the durable
// terminal record. Set before sessions are recovered.
func (m *Manager) SetTerminalResolver(fn func(name string) bool) {
	m.kmu.Lock()
	m.kindFunc = fn
	m.kmu.Unlock()
}

// MarkTerminal records name as a plain shell. Called when a terminal is created
// through TerminalHost, before any event of the session can be dispatched.
func (m *Manager) MarkTerminal(name string) {
	m.kmu.Lock()
	if m.kinds == nil {
		m.kinds = map[string]bool{}
	}
	m.kinds[name] = true
	m.kmu.Unlock()
}

// IsTerminal reports whether name is a plain-shell terminal session.
func (m *Manager) IsTerminal(name string) bool {
	m.kmu.Lock()
	defer m.kmu.Unlock()
	if v, ok := m.kinds[name]; ok {
		return v
	}
	if m.kindFunc == nil {
		return false
	}
	// Resolve once and cache both answers: a name only becomes a terminal
	// through MarkTerminal, which overwrites a cached negative.
	v := m.kindFunc(name)
	if m.kinds == nil {
		m.kinds = map[string]bool{}
	}
	m.kinds[name] = v
	return v
}

func (m *Manager) forgetKind(name string) {
	m.kmu.Lock()
	delete(m.kinds, name)
	m.kmu.Unlock()
}

// SetSessionRuntimePin pins the runtime mode for a specific session name.
// Pinning takes precedence over the global mode or cohort defaults.
func (m *Manager) SetSessionRuntimePin(name string, mode Mode) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.pins == nil {
		m.pins = make(map[string]Mode)
	}
	m.pins[name] = ParseMode(string(mode))
}

// PinnedRuntime returns the pinned runtime mode for name, if any.
func (m *Manager) PinnedRuntime(name string) (Mode, bool) {
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	mode, ok := m.pins[name]
	return mode, ok
}

// SetSessionCohort associates a session name with a named rollout cohort.
func (m *Manager) SetSessionCohort(name, cohort string) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.cohortOf == nil {
		m.cohortOf = make(map[string]string)
	}
	m.cohortOf[name] = cohort
}

// SessionCohort returns the cohort assigned to name, or empty string.
func (m *Manager) SessionCohort(name string) string {
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	return m.cohortOf[name]
}

// SetCohortMode configures the runtime mode for a cohort. When rolling back
// a cohort to a lower mode (e.g. on -> off), existing sessions in that cohort
// retain their pinned runtime; only future launches in the cohort adopt the new mode.
func (m *Manager) SetCohortMode(cohort string, mode Mode) {
	mode = ParseMode(string(mode))
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.cohorts == nil {
		m.cohorts = make(map[string]Mode)
	}
	prev := m.cohorts[cohort]
	if prev == "" {
		prev = m.Mode()
	}
	m.cohorts[cohort] = mode
	if isRollback(prev, mode) {
		m.rollbacks = append(m.rollbacks, RollbackRecord{
			Cohort: cohort,
			From:   prev,
			To:     mode,
			At:     time.Now().UTC(),
			Reason: "cohort rollback",
		})
		slog.Warn("ptyhost: cohort rolled back (live sessions retain runtime)", "cohort", cohort, "from", prev, "to", mode)
	}
}

// CohortMode returns the mode configured for cohort, if any.
func (m *Manager) CohortMode(cohort string) (Mode, bool) {
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	mode, ok := m.cohorts[cohort]
	return mode, ok
}

// Rollbacks returns a copy of the rollback records ledger.
func (m *Manager) Rollbacks() []RollbackRecord {
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	return append([]RollbackRecord(nil), m.rollbacks...)
}

// RecordRollback explicitly records a rollback event in the ledger.
func (m *Manager) RecordRollback(cohort string, from, to Mode, reason string) {
	m.pmu.Lock()
	defer m.pmu.Unlock()
	m.rollbacks = append(m.rollbacks, RollbackRecord{
		Cohort: cohort,
		From:   from,
		To:     to,
		At:     time.Now().UTC(),
		Reason: reason,
	})
}

func isRollback(from, to Mode) bool {
	rank := func(m Mode) int {
		switch m {
		case ModeOn:
			return 2
		case ModeShadow:
			return 1
		default:
			return 0
		}
	}
	return rank(to) < rank(from)
}

// EffectiveMode returns the runtime mode for name, honoring per-session pins
// and cohorts before falling back to the global mode.
func (m *Manager) EffectiveMode(name string) Mode {
	m.pmu.RLock()
	defer m.pmu.RUnlock()
	if pin, ok := m.pins[name]; ok {
		return pin
	}
	if cohort := m.cohortOf[name]; cohort != "" {
		if cm, ok := m.cohorts[cohort]; ok {
			return cm
		}
	}
	return m.Mode()
}

func (m *Manager) forgetSession(name string) {
	m.forgetKind(name)
	m.pmu.Lock()
	delete(m.pins, name)
	delete(m.cohortOf, name)
	m.pmu.Unlock()
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

// IsAuthoritative reports whether the runtime owns authoritative status and control
// for name. Direct sessions (plain terminals hosted on the direct PTY) are authoritative
// because warden owns their PTY master and process lifecycle. This holds even if the
// global mode or cohort was subsequently flipped or rolled back (per-session runtime pin:
// cohort rollback affects future launches only). Shadow sessions (agents or plain terminals
// mirrored from tmux) are observe-only mirrors and are never authoritative (no impossible
// dual ownership of one PTY master).
func (m *Manager) IsAuthoritative(name string) bool {
	if m == nil {
		return false
	}
	if m.direct != nil && m.direct.Session(name) != nil {
		return true
	}
	return false
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

// HolderHosted reports whether name is a direct session owned by a PTY holder
// (its process survives this daemon).
func (m *Manager) HolderHosted(name string) bool { return m.direct.HolderHosted(name) }

// HolderMeta returns the verified holder identity for name.  Recovery uses it
// to fence a durable direct-runtime record from a recycled PID or a different
// holder that happens to reuse the same session name.
func (m *Manager) HolderMeta(name string) (int, uint64, int, bool) {
	if m == nil || m.direct == nil || m.direct.Launcher == nil {
		return 0, 0, 0, false
	}
	if hv, ok := m.direct.Launcher.(interface {
		VerifyHolder(string) (int, uint64, int, bool)
	}); ok {
		return hv.VerifyHolder(name)
	}
	return 0, 0, 0, false
}

// AdoptDirect re-attaches to name's live holder-hosted process after a daemon
// restart: the same process on its journal (epoch+1, lifecycle{recovered}), no
// relaunch. The caller reports a failure with its cause; nothing is started.
func (m *Manager) AdoptDirect(ctx context.Context, name string) error {
	m.SetSessionRuntimePin(name, ModeOn)
	if err := m.direct.Adopt(ctx, name); err != nil {
		return err
	}
	if s := m.direct.Session(name); s != nil {
		m.watchStream(name, s)
	}
	return nil
}

// AdoptDirectTerminal is AdoptDirect with the durable shell classification
// restored for callers that know they are recovering a plain terminal. Agents
// must use AdoptDirect, otherwise AI-only observers would silently lose them.
func (m *Manager) AdoptDirectTerminal(ctx context.Context, name string) error {
	m.MarkTerminal(name)
	return m.AdoptDirect(ctx, name)
}

// MarkDirectLost journals that name's holder-hosted process could not be
// adopted (detail names the cause and the next step).
func (m *Manager) MarkDirectLost(name, detail string) error { return m.direct.MarkLost(name, detail) }

// RecoverShadow re-adopts the live tmux panes among names that were mirrored
// before (daemon restart): each opens a new epoch with lifecycle{recovered},
// marker{reset} and a snapshot seeded from the live screen. A no-op when the
// mode does not mirror.
func (m *Manager) RecoverShadow(ctx context.Context, names []string) int {
	n := 0
	for _, name := range names {
		if !m.EffectiveMode(name).mirrors() {
			continue
		}
		if !m.shadow.HasJournal(name) || m.shadow.Mirror(name) != nil || !m.tmux.HasSession(ctx, name) {
			continue
		}
		if err := m.shadow.VerifyPane(ctx, name); err != nil {
			// Not the process we mirrored: leave the pane alone and say why in the
			// journal instead of silently claiming (or relaunching) anything.
			slog.Error("ptyhost: refusing to re-adopt pane", "session", name, "err", err)
			_ = ptyruntime.MarkLost(m.shadow.journalDir(name), name, m.shadow.ret, ptyevent.PresentationTmux, err.Error())
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

// Close detaches from every holder-hosted direct session (the process keeps
// running for the next daemon), ends every other direct session (journaling its
// exit) and stops every
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
	var term, known bool
	for _, o := range obs {
		if o.agentsOnly {
			if !known {
				term, known = m.IsTerminal(name), true
			}
			if term {
				continue
			}
		}
		o.fn(name, ev)
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

// agentDirectReady is the internal readiness guard for direct AI-agent
// launches. It defaults DISABLED: until the authority work (02b-04) lands, an
// agent hosted on a direct PTY could be driven by legacy-only control or a
// competing poller, so only an explicit SetAgentDirectReady(true) (never
// config-derived yet) lets mode `on` launch an agent directly.
// SetAgentDirectReady flips the internal agent direct-launch guard.
func (m *Manager) SetAgentDirectReady(v bool) { m.agentDirect.Store(v) }

// AgentDirectReady reports the internal agent direct-launch guard.
func (m *Manager) AgentDirectReady() bool { return m.agentDirect.Load() }

// PlanAgentDirect reports whether a NEW agent named name would launch on a
// direct warden-owned PTY: guard enabled and the effective mode is `on`.
// Everything else (off, shadow, guard disabled) keeps the tmux path unchanged.
func (r routed) PlanAgentDirect(name string) bool {
	return !r.terminal && r.m.agentDirect.Load() && r.m.EffectiveMode(name) == ModeOn
}

// LaunchCommand starts argv as the agent's own process on a direct PTY under
// the independent holder. It is idempotent: if a holder already owns name (a
// crash between PTY start and the store write) the live process is adopted
// instead of launching a duplicate child. Callers must have checked
// PlanAgentDirect; a false plan returns ErrAgentDirectNotPlanned.
func (r routed) LaunchCommand(ctx context.Context, name, cwd string, argv []string, env ...string) error {
	m := r.m
	if !r.PlanAgentDirect(name) {
		return ErrAgentDirectNotPlanned
	}
	if s := m.direct.Session(name); s != nil && s.Alive() {
		return fmt.Errorf("ptyhost: agent %s already running directly", name)
	} else if s != nil {
		m.unwatch(name)
		_ = m.direct.KillSession(ctx, name)
		m.forgetSession(name)
	}
	m.SetSessionRuntimePin(name, ModeOn)
	m.forgetKind(name) // an agent never inherits a stale shell mark
	if m.direct.Launcher != nil && m.direct.Launcher.Has(name) {
		if err := m.direct.Adopt(ctx, name); err == nil {
			if s := m.direct.Session(name); s != nil {
				m.watchStream(name, s)
			}
			return nil
		}
	}
	if err := m.direct.NewCommand(ctx, name, cwd, argv, append(append([]string{}, directEnv...), env...)...); err != nil {
		m.forgetSession(name)
		return err
	}
	if s := m.direct.Session(name); s != nil {
		m.watchStream(name, s)
	}
	return nil
}

// ErrAgentDirectNotPlanned: a direct agent launch was requested while the
// guard is disabled or the session's mode is not `on`.
var ErrAgentDirectNotPlanned = errors.New("ptyhost: direct agent launch not enabled for this session")

var _ tmuxproc.Host = routed{}

// HostedDirectly reports whether name is a shell whose process IS the session
// (direct PTY). The launcher must not type a nested `${SHELL}` line into it:
// the session's shell is already the user's shell, and nesting would make
// `exit` return to an outer shell instead of ending the session with its status.
func (r routed) HostedDirectly(name string) bool { return r.m.direct.Session(name) != nil }

// PinnedMode returns the pinned runtime mode for name.
func (r routed) PinnedMode(name string) string {
	if mode, ok := r.m.PinnedRuntime(name); ok {
		return string(mode)
	}
	return string(r.m.EffectiveMode(name))
}

// HolderMeta returns holder process identity if name is holder-hosted.
func (r routed) HolderMeta(name string) (int, uint64, int, bool) {
	return r.m.HolderMeta(name)
}

// directEnv is added to a direct terminal: the daemon (launchd/systemd) often
// has no TERM, and the rendering end is always an xterm-compatible client.
var directEnv = []string{"TERM=xterm-256color"}

func (r routed) NewSession(ctx context.Context, name, cwd string, env ...string) error {
	m := r.m
	// Same-name reuse of a record id starts a new pin only after the previous
	// runtime is verified gone (§6.5).
	if s := m.direct.Session(name); s != nil && !s.Alive() {
		m.unwatch(name)
		_ = m.direct.KillSession(ctx, name)
		m.forgetSession(name)
	} else if mr := m.shadow.Mirror(name); mr != nil && !m.tmux.HasSession(ctx, name) {
		m.unwatch(name)
		m.shadow.Stop(ctx, name, nil)
		m.forgetSession(name)
	}
	mode := m.EffectiveMode(name)
	m.SetSessionRuntimePin(name, mode)
	if r.terminal {
		m.MarkTerminal(name)
	} else {
		m.forgetKind(name) // an agent reusing a name never inherits a stale shell mark
	}
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
		err := m.direct.KillSession(ctx, name)
		m.forgetSession(name)
		return err
	}
	err := m.tmux.KillSession(ctx, name)
	if m.shadow.Mirror(name) != nil {
		m.endShadow(ctx, name, "killed")
	}
	m.forgetSession(name)
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
