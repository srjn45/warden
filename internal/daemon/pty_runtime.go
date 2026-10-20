package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/ptyhost"
	"github.com/srjn45/warden/internal/ptyobs"
	"github.com/srjn45/warden/internal/ptyruntime"
	"github.com/srjn45/warden/internal/stateclassifier"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

// SetPTYRuntime wires the warden-owned PTY runtime (runtime.pty). Must be set
// before ListenAndServe: Run re-adopts mirrored panes and recovers direct
// terminals before the watchers start. nil keeps the daemon on tmux only.
func (s *Server) SetPTYRuntime(m *ptyhost.Manager) {
	s.pty = m
	if m != nil {
		if s.poller != nil {
			s.poller.Authoritative = m.IsAuthoritative
		}
		backendResolver := func(ag *agentstore.Agent) agentbackend.Backend {
			if s.poller != nil && s.poller.Backend != nil {
				return s.poller.Backend(ag)
			}
			if ag != nil {
				if b, err := agentbackend.Get(ag.AiCli); err == nil && b != nil {
					return b
				}
			}
			return agentbackend.Default()
		}
		s.classifier = stateclassifier.New(
			stateclassifier.DefaultConfig(),
			m,
			s.store,
			backendResolver,
			stateclassifier.NewOSProcessInspector(),
			nil,
		)
		s.classifier.OnTransition = func(name string, ch *ptyevent.StateChange) {
			s.notify()
		}
		s.ptyMetrics = ptyobs.NewMetrics()
		s.ptyMetrics.GaugeFunc(s.classifier.StateCounts)
		s.classifier.Metrics = s.ptyMetrics
		s.classifier.OnDisagreement = func(string, ptyevent.State, store.Status) {
			s.ptyMetrics.Inc(ptyobs.MFallback, "unknown", "shadow_disagreement")
		}
		// Kind comes from the durable record, not from names or screens, so a
		// shell is recognised across daemon restarts and agent-name collisions.
		m.SetTerminalResolver(func(name string) bool {
			if s.terminals == nil {
				return false
			}
			t, err := s.terminals.Get(context.Background(), name)
			return err == nil && t != nil
		})
		m.SetRecordResolver(func(ctx context.Context, name string) (*ptyhost.SessionRecord, bool) {
			if s.terminals != nil {
				if t, err := s.terminals.Get(ctx, name); err == nil && t != nil {
					return &ptyhost.SessionRecord{
						ID:          t.ID,
						TmuxSession: t.TmuxSession,
						Runtime:     t.Runtime,
					}, true
				}
			}
			if s.store != nil {
				if a, err := s.store.Get(ctx, name); err == nil && a != nil {
					return &ptyhost.SessionRecord{
						ID:          a.ID,
						TmuxSession: a.TmuxSession,
						Runtime:     a.Runtime,
					}, true
				}
			}
			return nil, false
		})
		m.Observe(s.onPTYEvent)
	}
}

// Classifier returns the event-driven activity/quiescence state classifier, if wired.
func (s *Server) Classifier() *stateclassifier.Classifier { return s.classifier }

// recoverPTY runs once at startup, before the pollers' first tick:
//
//   - shadow/on: re-adopts every live tmux pane that was mirrored before the
//     restart (epoch+1, lifecycle{recovered}, reset + snapshot);
//   - on: respawns every running direct terminal whose shell died with the
//     previous daemon onto its journal (lifecycle{crashed} for the lost epoch
//     when it never ended, then spawned + reset + snapshot), so a client
//     reconnecting with its old cursor is resynchronized from the snapshot.
func (s *Server) recoverPTY(ctx context.Context) {
	if s.pty == nil {
		return
	}
	var names []string
	if s.store != nil {
		if agents, err := s.store.List(ctx); err == nil {
			for _, a := range agents {
				if a.Runtime != nil && a.Runtime.PinnedMode != "" {
					s.pty.SetSessionRuntimePin(a.RuntimeKey(), ptyhost.ParseMode(a.Runtime.PinnedMode))
				}
				if liveStatus(a.Status) && a.RuntimeKey() != "" {
					names = append(names, a.RuntimeKey())
				}
			}
		}
	}
	var terms []*terminalstore.Terminal
	if s.terminals != nil {
		if all, err := s.terminals.List(ctx); err == nil {
			terms = all
		}
	}
	for _, t := range terms {
		if t.Runtime != nil && t.Runtime.PinnedMode != "" {
			s.pty.SetSessionRuntimePin(t.RuntimeKey(), ptyhost.ParseMode(t.Runtime.PinnedMode))
		}
		if t.Status == terminalstore.StatusRunning && t.RuntimeKey() != "" {
			names = append(names, t.RuntimeKey())
		}
	}
	if n := s.pty.RecoverShadow(ctx, names); n > 0 {
		slog.Info("ptyhost: re-adopted mirrored panes", "count", n)
	}
	if s.life == nil {
		return
	}
	for _, t := range terms {
		key := t.RuntimeKey()
		if t.Status != terminalstore.StatusRunning || t.Workdir == "" || !s.pty.HasDirectJournal(key) {
			continue
		}
		if s.pty.EffectiveMode(key) != ptyhost.ModeOn && !s.pty.HolderHosted(key) {
			continue
		}
		if s.pty.HolderHosted(key) {
			// The shell's PTY master belongs to an independent holder: adopt the
			// SAME process. Never relaunch — a failed adoption is reported with its
			// cause and the terminal is left restorable, not silently replaced.
			s.adoptDirectTerminal(ctx, t)
			continue
		}
		if err := s.life.RestoreTerminal(ctx, t.ID, t.Workdir); err != nil {
			if !errors.Is(err, lifecycle.ErrAlreadyRunning) {
				slog.Warn("ptyhost: recover direct terminal failed", "terminal", t.ID, "err", err)
			}
			continue
		}
		// Relaunch, not survival: this shell was a child of the previous daemon
		// (started before holders existed, or WARDEN_PTY_HOLDER=off), so it died
		// with it; this is a fresh shell on the same journal.
		slog.Info("ptyhost: recovered direct terminal onto its journal (fresh shell, relaunch)", "terminal", t.ID)
	}
}

// adoptDirectTerminal re-attaches to a holder-hosted terminal's live process
// after a daemon restart/update/crash.
func (s *Server) adoptDirectTerminal(ctx context.Context, t *terminalstore.Terminal) {
	name := t.RuntimeKey()
	if err := s.pty.AdoptDirect(ctx, name); err != nil {
		detail := "cannot re-adopt the shell (" + err.Error() + "); its PTY holder is unavailable — " +
			"run `warden agent restore " + t.ID + "` to start a fresh shell (the old process is not restarted automatically)"
		slog.Error("ptyhost: direct terminal could not be adopted; not relaunching", "terminal", t.ID, "err", err)
		_ = s.pty.MarkDirectLost(name, detail)
		s.recordDirectTerminalEnd(name, &ptyevent.Lifecycle{Phase: ptyevent.PhaseCrashed, Detail: detail})
		return
	}
	slog.Info("ptyhost: re-adopted live direct terminal (same process, no relaunch)", "terminal", t.ID)
	// It may have exited while the daemon was away: the exit was journaled during
	// adoption, before the stream watcher could see it.
	if d := s.pty.Direct(name); d != nil {
		go func() {
			select {
			case <-d.Done():
				s.recordDirectTerminalEnd(name, d.Exit())
			case <-s.done:
			}
		}()
	}
}

// onPTYEvent is the stream consumer the daemon registers. It never fights
// the pollers: session status still comes from the poller/watcher for every
// session; the stream only (a) pushes an SSE refresh on lifecycle changes so
// clients see them without waiting for a poll tick, and (b) moves a direct
// terminal whose shell exited to orphaned with the same compare-and-swap the
// terminal watcher uses, so whichever sees the exit first wins and the other
// is a no-op. Classifier-driven state (t3) plugs in here.
func (s *Server) onPTYEvent(name string, ev ptyevent.Event) {
	// The classifier registered itself with the manager in stateclassifier.New;
	// feeding it here as well would double every observation and halve the
	// hysteresis it is meant to enforce.
	s.countPTYEvent(ev)
	if ev.Kind != ptyevent.KindLifecycle {
		return
	}
	if ev.Lifecycle.Phase.Terminal() && s.terminals != nil && s.pty != nil {
		if d := s.pty.Direct(name); d != nil && d.Epoch() == ev.Epoch {
			s.recordDirectTerminalEnd(name, ev.Lifecycle)
		}
	}
	s.notify()
}

// recordDirectTerminalEnd persists how a direct shell ended with the same
// compare-and-swap the terminal watcher uses, so whichever sees the end first
// wins and the other is a no-op. A shell that exited on its own with a status
// is `exited` with that ExitCode (G12); a signal or lost process stays
// `orphaned` (restorable). Lifecycle carries the code only — never input or
// output bytes.
func (s *Server) recordDirectTerminalEnd(name string, lc *ptyevent.Lifecycle) {
	ctx := context.Background()
	t, err := s.terminals.Get(ctx, name)
	if err != nil || t.Status != terminalstore.StatusRunning {
		return
	}
	next := terminalstore.StatusOrphaned
	var code *int
	if lc != nil && lc.Phase == ptyevent.PhaseExited && lc.ExitCode != nil {
		next, code = terminalstore.StatusExited, lc.ExitCode
	}
	swapped := false
	if err := s.terminals.Update(ctx, t.ID, func(u *terminalstore.Terminal) error {
		if u.Status != terminalstore.StatusRunning {
			return nil
		}
		u.Status, u.ExitCode, u.UpdatedAt = next, code, time.Now().UTC()
		swapped = true
		return nil
	}); err != nil || !swapped {
		return
	}
	if s.terminalWatcher != nil && s.terminalWatcher.OnTransition != nil {
		s.terminalWatcher.OnTransition(sessionFromTerminal(t), sessionStatusFromTerminal(terminalstore.StatusRunning), sessionStatusFromTerminal(next))
	}
}

// handlePTYStream serves GET /api/v1/sessions/{id}/pty: the §6 event stream
// (hello → plan → redacted replay → live, cursor resume, ack, resize, and for
// interactive direct sessions raw live bytes plus arbitrated human input). A
// read-only token gets an observe-only stream.
func (s *Server) handlePTYStream(w http.ResponseWriter, r *http.Request) {
	sess, err := s.resolveSessionDTO(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		var ae apiError
		if errors.As(err, &ae) && ae.code == http.StatusNotFound {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.pty == nil {
		writeErr(w, http.StatusConflict, "the PTY runtime is not enabled (runtime.pty: off); use /attach")
		return
	}
	t, ok := s.pty.Lookup(sess.RuntimeKey())
	if !ok {
		writeErr(w, http.StatusConflict, "session has no PTY event journal (runtime.pty was off when it started); use /attach")
		return
	}
	if _, scope := s.authorize(r); scope == scopeReadonly {
		t.Direct = nil // observe-only: no input, resize or raw bytes
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	if err := s.pty.Serve(r.Context(), conn, t, newPTYClientID()); err != nil {
		slog.Debug("ptyhost: stream ended", "session", sess.ID, "err", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "stream ended")
}

// directSession returns the direct PTY session hosting tmuxName, if any.
func (s *Server) directSession(tmuxName string) *ptyruntime.Session {
	if s.pty == nil {
		return nil
	}
	return s.pty.Direct(tmuxName)
}

// directInput types text and Enter into a direct terminal as operator input
// (it waits while a human is mid-line, like every non-human write).
func directInput(ctx context.Context, d *ptyruntime.Session, text string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data := append(ptyruntime.KeyBytes("-l", text), '\r')
	return d.WriteWait(ctx, ptyruntime.Write{Origin: ptyevent.OriginOperator, Data: data})
}

// directOutput renders the last lines of a direct terminal's screen model.
func directOutput(d *ptyruntime.Session, lines int) string {
	rows := strings.Split(d.Screen(), "\n")
	if lines > 0 && len(rows) > lines {
		rows = rows[len(rows)-lines:]
	}
	return strings.Join(rows, "\n")
}

func newPTYClientID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "ws-" + hex.EncodeToString(b[:])
}
