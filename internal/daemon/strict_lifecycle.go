package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/plugin"
	"github.com/srjn45/warden/internal/store"
)

// spawnRequestFromOAPI maps the generated spawn body onto the daemon's
// SpawnRequest DTO, which the Lifecycle interface is defined in terms of.
func spawnRequestFromOAPI(b oapi.SpawnRequest) SpawnRequest {
	return SpawnRequest{
		Type:           b.Type,
		Ticket:         b.Ticket,
		Name:           b.Name,
		Repo:           b.Repo,
		Branch:         b.Branch,
		PR:             b.Pr,
		Worktree:       b.Worktree,
		InRepo:         b.InRepo,
		Prompt:         b.Prompt,
		Cwd:            b.Cwd,
		PermissionMode: b.PermissionMode,
		AutoRestart:    b.AutoRestart,
		Force:          b.Force,
		Model:          b.Model,
		Backend:        b.Backend,
		Tags:           b.Tags,
		ParentID:       b.ParentId,
	}
}

// SpawnAgent implements POST /api/v1/spawn. The memory-pressure soft gate returns
// 428 (not an error) so the client can re-submit with force=true.
func (s *Server) SpawnAgent(ctx context.Context, req oapi.SpawnAgentRequestObject) (oapi.SpawnAgentResponseObject, error) {
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "bad json")
	}
	sr := spawnRequestFromOAPI(*req.Body)
	if code, msg := s.validateSpawnRequest(ctx, sr); code != 0 {
		return nil, errStatus(code, msg)
	}
	freeMode := sr.Type == ""
	// pre-spawn hook (#47): advisory, fail-open.
	s.plugins.Dispatch(ctx, plugin.EventPreSpawn, plugin.SessionMeta{Type: sr.Type, Repo: sr.Repo}, nil)
	s.pressMu.RLock()
	gateOn := s.spawnGate
	s.pressMu.RUnlock()
	if gateOn && !sr.Force {
		if v := s.spawnVerdict(ctx); v.Elevated {
			return oapi.SpawnAgent428JSONResponse{ConfirmationRequired: true, Verdict: v}, nil
		}
	}
	// Budget (cost) gate: a soft gate on measured Claude spend, sibling to the
	// pressure gate above and sharing its 428 confirmation contract. A spawn that
	// would push past a configured $ cap warns once; force re-submits past it.
	if !sr.Force {
		if v, over := s.budgetVerdict(); over {
			return oapi.SpawnAgent428JSONResponse{ConfirmationRequired: true, Verdict: v}, nil
		}
	}
	sess, err := s.life.Spawn(ctx, sr)
	if err != nil {
		return nil, err
	}
	if err := s.store.Insert(ctx, sess); err != nil {
		// Roll back the tmux session (and any worktree) so a failed insert doesn't
		// leak an untracked agent.
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if terr := s.life.Teardown(tctx, sess); terr != nil {
			slog.Warn("spawn rollback failed", "agent", sess.ID, "err", terr)
		}
		return nil, err
	}
	s.notify()
	s.recordAuditCtx(ctx, audit.ActionSpawn, sess.ID, spawnAuditDetail(sess, sr))
	// post-spawn hook (#47): advisory, fail-open.
	s.plugins.Dispatch(ctx, plugin.EventPostSpawn, plugin.MetaFromSession(sess), nil)
	// Background, best-effort enrichment (detached contexts): a missing
	// type/name never blocks or fails the spawn.
	if freeMode && sr.Prompt != "" {
		go s.classifyAndUpdate(sess.ID, sr.Prompt)
	}
	if sr.Name == "" && sr.Prompt != "" {
		go s.nameAndUpdate(sess.ID, sr.Prompt)
	}
	return oapi.SpawnAgent201JSONResponse(*sess), nil
}

// AdoptSession implements POST /api/v1/adopt: register a resume- or live-mode
// session warden did not spawn. Rollback (kill tmux) runs ONLY in resume mode.
func (s *Server) AdoptSession(ctx context.Context, req oapi.AdoptSessionRequestObject) (oapi.AdoptSessionResponseObject, error) {
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "bad json")
	}
	b := *req.Body
	if b.Cwd == "" {
		return nil, errStatus(http.StatusBadRequest, "adopt requires cwd")
	}
	if fi, err := os.Stat(b.Cwd); err != nil || !fi.IsDir() {
		return nil, errStatus(http.StatusBadRequest, "cwd is not an existing directory: "+b.Cwd)
	}
	resume := b.TmuxSession == ""
	claudeID := b.SessionId
	if claudeID == "" {
		if id, err := s.life.NewestClaudeSession(ctx, b.Cwd); err == nil {
			claudeID = id
		}
	}
	if resume && claudeID == "" {
		return nil, errStatus(http.StatusBadRequest, "no claude session found to resume in "+b.Cwd+" (pass session_id)")
	}
	// Two-heads guard: never adopt a conversation an active session already tracks.
	if claudeID != "" {
		sessions, err := s.store.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, ex := range sessions {
			if ex.ClaudeSessionID == claudeID {
				return nil, errStatus(http.StatusConflict, "claude session already adopted as "+ex.ID)
			}
		}
	}
	chosenID := ""
	if !resume && store.SafeID(b.TmuxSession) == nil {
		if _, err := s.store.Get(ctx, b.TmuxSession); errors.Is(err, store.ErrNotFound) {
			chosenID = b.TmuxSession
		}
	}
	sess, err := s.life.Adopt(ctx, AdoptParams{
		ID: chosenID, Cwd: b.Cwd, ClaudeSessionID: claudeID, TmuxSession: b.TmuxSession,
	})
	if err != nil {
		if errors.Is(err, lifecycle.ErrTmuxGone) {
			return nil, errStatus(http.StatusNotFound, err.Error())
		}
		return nil, err
	}
	if err := s.store.Insert(ctx, sess); err != nil {
		// Only resume mode created the tmux session; never kill a live one.
		if resume {
			tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if terr := s.life.Teardown(tctx, sess); terr != nil {
				slog.Warn("adopt rollback failed", "agent", sess.ID, "err", terr)
			}
		}
		if errors.Is(err, store.ErrExists) {
			return nil, errStatus(http.StatusConflict, "already registered: "+sess.ID)
		}
		return nil, err
	}
	warn := ""
	if claudeID == "" {
		warn = "registered without a claude session id (monitoring only; restore unavailable)"
	}
	s.notify()
	return oapi.AdoptSession201JSONResponse{Session: *sess, Warning: warn}, nil
}

// TerminateSession implements POST /api/v1/sessions/{id}/terminate.
func (s *Server) TerminateSession(ctx context.Context, req oapi.TerminateSessionRequestObject) (oapi.TerminateSessionResponseObject, error) {
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	if err := s.life.Terminate(ctx, sess.TmuxSession); err != nil {
		return nil, err
	}
	if err := s.store.UpdateStatus(ctx, req.Id, store.StatusDone); err != nil {
		return nil, err
	}
	s.notify()
	// Terminate sets the session done directly (no poller swap), so reconcile the
	// owning pipeline job here too — otherwise it stays stuck running.
	s.reconcileJobOnTerminal(sess, store.StatusDone)
	s.recordAuditCtx(ctx, audit.ActionTerminate, req.Id, nil)
	return oapi.TerminateSession200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "terminated"}}, nil
}

// liveChildren returns the direct children of parent id whose status implies a
// still-running agent. Direct children suffice to anchor the sub-tree: a live
// grandchild implies its own parent is itself a live (or tombstoned) child, so
// the chain stays rooted. A store error yields nil (treated as "no live
// children" — delete falls through to its normal hard/archive path).
func (s *Server) liveChildren(ctx context.Context, id string) []*store.Session {
	all, err := s.store.List(ctx)
	if err != nil {
		return nil
	}
	var kids []*store.Session
	for _, c := range all {
		if c.ParentID == id && liveStatus(c.Status) {
			kids = append(kids, c)
		}
	}
	return kids
}

// DeleteSession implements POST /api/v1/sessions/{id}/delete.
func (s *Server) DeleteSession(ctx context.Context, req oapi.DeleteSessionRequestObject) (oapi.DeleteSessionResponseObject, error) {
	hard := false
	if req.Body != nil {
		hard = req.Body.Hard
	}
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	// A parent that still has live children is tombstoned rather than removed:
	// tear down its tmux so no live pane remains, but keep the record active and
	// terminal so the children stay anchored under it in the sub-tree view
	// (design §6). It is reaped once its last live child ends (Phase 3). Silent —
	// no confirmation; the live-child count surfaces in the TUI header (Phase 4).
	if kids := s.liveChildren(ctx, req.Id); len(kids) > 0 {
		if err := s.life.Terminate(ctx, sess.TmuxSession); err != nil {
			return nil, err
		}
		term := store.StatusDone
		if hard {
			term = store.StatusOrphaned // force-kill semantics
		}
		if err := s.store.UpdateStatus(ctx, req.Id, term); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		s.notify()
		s.reconcileJobOnTerminal(sess, term)
		s.recordAuditCtx(ctx, audit.ActionDelete, req.Id, map[string]string{
			"tombstoned":    "true",
			"hard":          strconv.FormatBool(hard),
			"live_children": strconv.Itoa(len(kids)),
		})
		return oapi.DeleteSession200JSONResponse{Status: "tombstoned"}, nil
	}
	warn := ""
	if liveStatus(sess.Status) {
		warn = "agent may still be running (status " + string(sess.Status) + "); terminate it first or it becomes untracked"
	}
	var derr error
	if hard {
		derr = s.store.Delete(ctx, req.Id)
	} else {
		derr = s.store.Archive(ctx, req.Id)
	}
	if derr != nil && !errors.Is(derr, store.ErrNotFound) {
		return nil, derr
	}
	// Only a hard delete drops the agent's inbox; an archive keeps it. Best-effort.
	if hard && s.mbox != nil {
		_ = s.mbox.DeleteInbox(req.Id)
	}
	// Retention policy (worktree_keep_done=false): guarded best-effort removal after
	// a successful archive of a worktree-owning session.
	if !hard && s.removeDoneWorktree && sess.Worktree != "" {
		s.removeDoneWorktreeBestEffort(sess)
	}
	s.notify()
	s.recordAuditCtx(ctx, audit.ActionDelete, req.Id, map[string]string{"hard": strconv.FormatBool(hard)})
	return oapi.DeleteSession200JSONResponse{Status: "deleted", Warning: warn}, nil
}

// RemoveWorktree implements POST /api/v1/sessions/{id}/remove-worktree.
func (s *Server) RemoveWorktree(ctx context.Context, req oapi.RemoveWorktreeRequestObject) (oapi.RemoveWorktreeResponseObject, error) {
	force, deleteAdopted := false, false
	if req.Body != nil {
		force, deleteAdopted = req.Body.Force, req.Body.DeleteAdoptedBranch
	}
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	if err := s.life.RemoveWorktree(ctx, sess, force, deleteAdopted); err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrNoWorktree):
			return nil, errStatus(http.StatusUnprocessableEntity, err.Error())
		case errors.Is(err, lifecycle.ErrWorktreeAgentAlive),
			errors.Is(err, lifecycle.ErrDirtyWorktree),
			errors.Is(err, lifecycle.ErrUnpushedCommits):
			return nil, errStatus(http.StatusConflict, err.Error())
		default:
			return nil, err
		}
	}
	if err := s.store.ClearWorktree(ctx, req.Id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	s.notify()
	return oapi.RemoveWorktree200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "worktree removed"}}, nil
}

// RestoreSession implements POST /api/v1/sessions/{id}/restore.
func (s *Server) RestoreSession(ctx context.Context, req oapi.RestoreSessionRequestObject) (oapi.RestoreSessionResponseObject, error) {
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	if err := s.life.Restore(ctx, sess); err != nil {
		switch {
		case errors.Is(err, lifecycle.ErrAlreadyRunning):
			return nil, errStatus(http.StatusConflict, err.Error())
		case errors.Is(err, lifecycle.ErrNoSessionID),
			errors.Is(err, lifecycle.ErrWorkdirMissing),
			errors.Is(err, lifecycle.ErrNoTranscript):
			return nil, errStatus(http.StatusUnprocessableEntity, err.Error())
		default:
			return nil, err
		}
	}
	if err := s.store.UpdateStatus(ctx, req.Id, store.StatusSpawning); err != nil {
		return nil, err
	}
	s.notify()
	return oapi.RestoreSession200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "restoring"}}, nil
}

// SendInput implements POST /api/v1/sessions/{id}/input.
func (s *Server) SendInput(ctx context.Context, req oapi.SendInputRequestObject) (oapi.SendInputResponseObject, error) {
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	text := ""
	if req.Body != nil {
		text = req.Body.Text
	}
	if err := s.life.Input(ctx, sess.TmuxSession, text); err != nil {
		return nil, err
	}
	s.notify()
	return oapi.SendInput200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "sent"}}, nil
}

// GetOutput implements GET /api/v1/sessions/{id}/output. A capture failure
// degrades to an empty 200 (the agent is mid-spawn or just terminated).
func (s *Server) GetOutput(ctx context.Context, req oapi.GetOutputRequestObject) (oapi.GetOutputResponseObject, error) {
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	lines := 200
	if req.Params.Lines != 0 {
		lines = req.Params.Lines
	}
	out, err := s.life.Output(ctx, sess.TmuxSession, lines)
	if err != nil {
		out = ""
	}
	return oapi.GetOutput200JSONResponse{Output: out}, nil
}

// ListWorktrees implements GET /api/v1/worktrees: the read-only join of git's
// .worktrees inventory against active + archived records.
func (s *Server) ListWorktrees(ctx context.Context, req oapi.ListWorktreesRequestObject) (oapi.ListWorktreesResponseObject, error) {
	repo := req.Params.Repo
	if repo == "" {
		return nil, errStatus(http.StatusBadRequest, "repo is required")
	}
	active, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	archived, err := s.store.ListClosed(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.life.ListWorktrees(ctx, repo, active, archived)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []lifecycle.WorktreeListing{}
	}
	return oapi.ListWorktrees200JSONResponse{Worktrees: rows}, nil
}

// PruneWorktrees implements POST /api/v1/prune. Dirty/unpushed worktrees come
// back as skipped result entries, not HTTP errors.
func (s *Server) PruneWorktrees(ctx context.Context, req oapi.PruneWorktreesRequestObject) (oapi.PruneWorktreesResponseObject, error) {
	var b oapi.PruneRequest
	if req.Body != nil {
		b = *req.Body
	}
	if b.Repo == "" {
		return nil, errStatus(http.StatusBadRequest, "repo is required")
	}
	active, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	archived, err := s.store.ListClosed(ctx)
	if err != nil {
		return nil, err
	}
	results, err := s.life.PruneWorktrees(ctx, b.Repo, lifecycle.PruneOpts{
		DryRun:          b.DryRun,
		Force:           b.Force,
		IncludeArchived: b.IncludeArchived,
		Active:          active,
		Archived:        archived,
	})
	if err != nil {
		return nil, err
	}
	if results == nil {
		results = []lifecycle.PruneResult{}
	}
	if !b.DryRun {
		s.notify()
	}
	return oapi.PruneWorktrees200JSONResponse{Results: results}, nil
}

// SetAutoApprove implements PATCH /api/v1/sessions/{id}/auto-approve.
func (s *Server) SetAutoApprove(ctx context.Context, req oapi.SetAutoApproveRequestObject) (oapi.SetAutoApproveResponseObject, error) {
	enabled := false
	if req.Body != nil {
		enabled = req.Body.Enabled
	}
	if err := s.store.UpdateAutoApprove(ctx, req.Id, enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "session not found")
		}
		return nil, err
	}
	s.notify()
	return oapi.SetAutoApprove200JSONResponse{AutoApprove: enabled}, nil
}

// SetForceCompact implements PATCH /api/v1/sessions/{id}/force-compact. It sets
// the per-agent force-compact override: "on"/"off" pin the agent, "inherit"
// clears the override so it follows the global token_force_compact.
func (s *Server) SetForceCompact(ctx context.Context, req oapi.SetForceCompactRequestObject) (oapi.SetForceCompactResponseObject, error) {
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "missing body")
	}
	var override *bool
	switch req.Body.State {
	case oapi.SetForceCompactJSONBodyStateOn:
		on := true
		override = &on
	case oapi.SetForceCompactJSONBodyStateOff:
		off := false
		override = &off
	case oapi.SetForceCompactJSONBodyStateInherit:
		override = nil
	default:
		return nil, errStatus(http.StatusBadRequest, "state must be one of: on, off, inherit")
	}
	if err := s.store.SetForceCompact(ctx, req.Id, override); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "session not found")
		}
		return nil, err
	}
	s.notify()
	return oapi.SetForceCompact200JSONResponse{State: oapi.SetForceCompact200JSONResponseBodyState(req.Body.State)}, nil
}

// SetPermissionMode implements PATCH /api/v1/sessions/{id}/permission-mode. An
// empty mode is valid — it means "use the global default".
func (s *Server) SetPermissionMode(ctx context.Context, req oapi.SetPermissionModeRequestObject) (oapi.SetPermissionModeResponseObject, error) {
	mode := ""
	if req.Body != nil {
		mode = string(req.Body.PermissionMode)
	}
	if !lifecycle.ValidPermissionMode(mode) {
		return nil, errStatus(http.StatusBadRequest, "invalid permission mode")
	}
	if err := s.store.UpdatePermissionMode(ctx, req.Id, mode); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "session not found")
		}
		return nil, err
	}
	s.notify()
	return oapi.SetPermissionMode200JSONResponse{PermissionMode: mode}, nil
}

// SetName implements PATCH /api/v1/sessions/{id}/name. A blank name clears it;
// an invalid format or duplicate name is rejected (mirrors spawn).
func (s *Server) SetName(ctx context.Context, req oapi.SetNameRequestObject) (oapi.SetNameResponseObject, error) {
	name := ""
	if req.Body != nil {
		name = req.Body.Name
	}
	name = strings.TrimSpace(name)
	if err := store.ValidateName(name); err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	if _, err := s.store.Get(ctx, req.Id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "session not found")
		}
		return nil, err
	}
	if name != "" {
		sessions, err := s.store.List(ctx)
		if err != nil {
			return nil, errStatus(http.StatusInternalServerError, "failed to check name uniqueness: "+err.Error())
		}
		for _, sess := range sessions {
			if sess.ID != req.Id && sess.Name == name {
				return nil, errStatus(http.StatusConflict, "name already in use: "+name)
			}
		}
	}
	if err := s.store.UpdateName(ctx, req.Id, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "session not found")
		}
		return nil, err
	}
	s.notify()
	return oapi.SetName200JSONResponse{Name: name}, nil
}
