package daemon

import (
	"context"
	"errors"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/srjn45/warden/internal/projectstore"
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

// resolveSessionDTO is the compatibility boundary for the shared session HTTP API.
// Internal agent operations use resolveSession and cannot resolve a terminal.
func (s *Server) resolveSessionDTO(ctx context.Context, ref string) (*store.Session, error) {
	if t, err := s.lookupTerminal(ctx, ref); err == nil {
		return sessionFromTerminal(t), nil
	} else if !errors.Is(err, terminalstore.ErrNotFound) {
		return nil, err
	}
	a, err := s.resolveSession(ctx, ref)
	if err != nil {
		return nil, err
	}
	return a.ToSession(), nil
}

// resolveTerminalProject returns the id of the project a new terminal joins. A
// terminal always belongs to a project, so the project is resolved (and opened or
// registered) BEFORE any tmux session exists, and a terminal that cannot be given
// one is refused rather than created project-less:
//
//   - an explicit req.ProjectID must name a known project (reopened if hibernated;
//     an absolute path that is not yet registered is registered);
//   - otherwise the project is path-matched/registered from req.Cwd, as for agents;
//   - when nothing resolves (empty or unregisterable cwd) the spawn is a 400.
//
// A daemon with no project store wired (embedded/test setups) has no project
// membership at all, so the guard cannot apply and the request id passes through.
func (s *Server) resolveTerminalProject(req SpawnRequest) (id string, code int, msg string) {
	if s.projects == nil {
		return req.ProjectID, 0, ""
	}
	if req.ProjectID != "" {
		id = s.ensureExplicitProjectID(req.ProjectID)
		if _, err := s.projects.Get(id); err != nil {
			if errors.Is(err, projectstore.ErrNotFound) {
				return "", http.StatusBadRequest, "unknown project " + req.ProjectID + ": a terminal must belong to an existing project"
			}
			return "", http.StatusInternalServerError, "resolve project: " + err.Error()
		}
		return id, 0, ""
	}
	if id = s.ensureProjectID(req.Cwd, "terminal"); id == "" {
		return "", http.StatusBadRequest, "a terminal must belong to a project: cwd " + strconv.Quote(req.Cwd) +
			" could not be resolved to one — pass project_id or a working directory that can be registered as a project"
	}
	return id, 0, ""
}

func (s *Server) spawnTerminal(ctx context.Context, req SpawnRequest) (oapi.SpawnAgentResponseObject, error) {
	if s.terminals == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "terminal store unavailable")
	}
	req.Type = ""
	if code, msg := s.validateSpawnRequest(ctx, req); code != 0 {
		return nil, errStatus(code, msg)
	}
	// Resolve the owning project first: refuse before creating a pane, so a
	// rejected spawn leaves no tmux session or record to clean up.
	projectID, code, msg := s.resolveTerminalProject(req)
	if code != 0 {
		return nil, errStatus(code, msg)
	}
	req.ProjectID = projectID
	t, err := s.life.SpawnTerminal(ctx, req)
	if err != nil {
		return nil, err
	}
	t.ProjectID = projectID
	if err := s.terminals.Spawn(ctx, t, t.TmuxSession); err != nil {
		_ = s.life.Terminate(ctx, t.TmuxSession)
		return nil, err
	}
	if s.projects != nil && t.ProjectID != "" {
		if _, err := s.projects.AddTerminalToProject(t.ProjectID, t.ID); err != nil {
			// Project.Terminals is the authoritative membership edge; a terminal that
			// cannot be recorded there would be an orphan of its own project, so roll
			// the spawn back instead of leaving a half-registered shell.
			_ = s.life.Terminate(ctx, t.TmuxSession)
			_ = s.terminals.Delete(ctx, t.ID)
			return nil, errStatus(http.StatusInternalServerError, "record terminal in project "+t.ProjectID+": "+err.Error())
		}
	}
	s.notify()
	return oapi.SpawnAgent201JSONResponse(*sessionFromTerminal(t)), nil
}

func (s *Server) stopTerminal(ctx context.Context, t *terminalstore.Terminal) error {
	if err := s.life.Terminate(ctx, t.TmuxSession); err != nil {
		return err
	}
	if err := s.terminals.Terminate(ctx, t.ID); err != nil {
		return err
	}
	if s.projects != nil && t.ProjectID != "" {
		_, _ = s.projects.RemoveTerminalFromProject(t.ProjectID, t.ID)
	}
	s.notify()
	return nil
}
