package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/terminalstore"
)

// SetTerminals wires the first-class terminal store. When set, terminal spawn /
// attach / status / terminate / project hibernation / restore / list discovery
// route through terminalstore rather than the agent session store. nil keeps the
// legacy Session-Kind path (tests that do not exercise terminals).
func (s *Server) SetTerminals(ts *terminalstore.Store) { s.terminals = ts }

// sessionFromTerminal projects a terminalstore.Terminal into the Session DTO the
// public API / TUI still speak. Terminals never carry AI fields.
func sessionFromTerminal(t *terminalstore.Terminal) *store.Session {
	if t == nil {
		return nil
	}
	return &store.Session{
		ID:          t.ID,
		Name:        t.Name,
		ProjectID:   t.ProjectID,
		Kind:        store.KindTerminal,
		TmuxSession: t.TmuxSession,
		Workdir:     t.Workdir,
		PID:         t.PID,
		Status:      sessionStatusFromTerminal(t.Status),
		ExitCode:    t.ExitCode,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

func sessionStatusFromTerminal(st terminalstore.Status) store.Status {
	switch st {
	case terminalstore.StatusExited:
		return store.StatusDone
	case terminalstore.StatusOrphaned:
		return store.StatusOrphaned
	default:
		return store.StatusWorking
	}
}

func terminalStatusFromSession(st store.Status) terminalstore.Status {
	switch st {
	case store.StatusDone, store.StatusErrored:
		return terminalstore.StatusExited
	case store.StatusOrphaned:
		return terminalstore.StatusOrphaned
	default:
		return terminalstore.StatusRunning
	}
}

// persistTerminal writes a freshly spawned terminal into terminalstore. Shell is
// best-effort from $SHELL so the record carries process identity beyond tmux.
func (s *Server) persistTerminal(ctx context.Context, sess *store.Session) error {
	if s.terminals == nil || sess == nil {
		return nil
	}
	now := time.Now().UTC()
	shell := os.Getenv("SHELL")
	t := &terminalstore.Terminal{
		ID:          sess.ID,
		ProjectID:   sess.ProjectID,
		Name:        sess.Name,
		TmuxSession: sess.TmuxSession,
		Workdir:     sess.Workdir,
		Shell:       shell,
		PID:         sess.PID,
		Status:      terminalstore.StatusRunning,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if !sess.CreatedAt.IsZero() {
		t.CreatedAt = sess.CreatedAt
	}
	return s.terminals.Spawn(ctx, t, sess.TmuxSession)
}

// lookupTerminal resolves an id-or-name against terminalstore. Name match is
// linear over List (terminals are few).
func (s *Server) lookupTerminal(ctx context.Context, ref string) (*terminalstore.Terminal, error) {
	if s.terminals == nil {
		return nil, terminalstore.ErrNotFound
	}
	t, err := s.terminals.Get(ctx, ref)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, terminalstore.ErrNotFound) {
		return nil, err
	}
	all, err := s.terminals.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, term := range all {
		if term.Name != "" && term.Name == ref {
			return term, nil
		}
	}
	return nil, terminalstore.ErrNotFound
}

// listTerminalSessions returns Session projections of every terminalstore row.
// Used by ListSessions / cockpit discovery when terminals is wired.
func (s *Server) listTerminalSessions(ctx context.Context) ([]*store.Session, error) {
	if s.terminals == nil {
		return nil, nil
	}
	all, err := s.terminals.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Session, 0, len(all))
	for _, t := range all {
		out = append(out, sessionFromTerminal(t))
	}
	return out, nil
}

// hibernateProjectTerminals terminates live terminal panes for project p and
// marks them orphaned in terminalstore so a later reopen can RestoreTerminal.
// The durable record is kept (unlike operator terminate, which deletes).
func (s *Server) hibernateProjectTerminals(ctx context.Context, projectID string) {
	if s.terminals == nil || projectID == "" {
		return
	}
	all, err := s.terminals.List(ctx)
	if err != nil {
		slog.Warn("daemon: hibernate terminals: list failed", "project", projectID, "err", err)
		return
	}
	for _, t := range all {
		if t.ProjectID != projectID || t.Status != terminalstore.StatusRunning {
			continue
		}
		if err := s.life.Terminate(ctx, t.TmuxSession); err != nil {
			slog.Warn("daemon: hibernate terminals: terminate failed", "terminal", t.ID, "err", err)
			continue
		}
		if err := s.terminals.Update(ctx, t.ID, func(u *terminalstore.Terminal) error {
			u.Status = terminalstore.StatusOrphaned
			u.UpdatedAt = time.Now().UTC()
			return nil
		}); err != nil {
			slog.Warn("daemon: hibernate terminals: mark failed", "terminal", t.ID, "err", err)
		}
	}
}

// restoreHibernatedTerminals respawns orphaned terminals for project p after
// reopen. Best-effort per terminal.
func (s *Server) restoreHibernatedTerminals(ctx context.Context, projectID string) {
	if s.terminals == nil || projectID == "" {
		return
	}
	all, err := s.terminals.List(ctx)
	if err != nil {
		slog.Warn("daemon: restore terminals: list failed", "project", projectID, "err", err)
		return
	}
	restored := false
	for _, t := range all {
		if t.ProjectID != projectID || t.Status != terminalstore.StatusOrphaned {
			continue
		}
		workdir := t.Workdir
		if workdir == "" {
			continue
		}
		if err := s.life.RestoreTerminal(ctx, t.ID, workdir); err != nil {
			slog.Warn("daemon: restore terminals: restore failed", "terminal", t.ID, "err", err)
			continue
		}
		if err := s.terminals.Update(ctx, t.ID, func(u *terminalstore.Terminal) error {
			u.Status = terminalstore.StatusRunning
			u.TmuxSession = t.ID
			u.ExitCode = nil
			u.UpdatedAt = time.Now().UTC()
			return nil
		}); err != nil {
			slog.Warn("daemon: restore terminals: status update failed", "terminal", t.ID, "err", err)
			continue
		}
		restored = true
	}
	if restored {
		s.notify()
	}
}
