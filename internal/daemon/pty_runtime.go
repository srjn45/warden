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
	if s.pty == nil || s.pty.Mode() == ptyhost.ModeOff {
		return
	}
	var names []string
	if s.store != nil {
		if agents, err := s.store.List(ctx); err == nil {
			for _, a := range agents {
				if liveStatus(a.Status) && a.TmuxSession != "" {
					names = append(names, a.TmuxSession)
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
		if t.Status == terminalstore.StatusRunning && t.TmuxSession != "" {
			names = append(names, t.TmuxSession)
		}
	}
	if n := s.pty.RecoverShadow(ctx, names); n > 0 {
		slog.Info("ptyhost: re-adopted mirrored panes", "count", n)
	}
	if s.pty.Mode() != ptyhost.ModeOn || s.life == nil {
		return
	}
	for _, t := range terms {
		if t.Status != terminalstore.StatusRunning || t.Workdir == "" || !s.pty.HasDirectJournal(t.TmuxSession) {
			continue
		}
		if err := s.life.RestoreTerminal(ctx, t.ID, t.Workdir); err != nil {
			if !errors.Is(err, lifecycle.ErrAlreadyRunning) {
				slog.Warn("ptyhost: recover direct terminal failed", "terminal", t.ID, "err", err)
			}
			continue
		}
		slog.Info("ptyhost: recovered direct terminal onto its journal", "terminal", t.ID)
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
	if s.classifier != nil {
		s.classifier.OnPTYEvent(name, ev)
	}
	if ev.Kind != ptyevent.KindLifecycle {
		return
	}
	if ev.Lifecycle.Phase.Terminal() && s.terminals != nil && s.pty != nil {
		if d := s.pty.Direct(name); d != nil && d.Epoch() == ev.Epoch {
			if t, err := s.terminals.Get(context.Background(), name); err == nil && t.Status != terminalstore.StatusOrphaned {
				prev := sessionStatusFromTerminal(t.Status)
				deps := &terminalPollerDeps{terms: s.terminals}
				if ok, _ := deps.UpdateStatusIf(context.Background(), t.ID, prev, store.StatusOrphaned); ok &&
					s.terminalWatcher != nil && s.terminalWatcher.OnTransition != nil {
					s.terminalWatcher.OnTransition(sessionFromTerminal(t), prev, store.StatusOrphaned)
				}
			}
		}
	}
	s.notify()
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
	t, ok := s.pty.Lookup(sess.TmuxSession)
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
