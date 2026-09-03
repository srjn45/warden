package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
)

// serverCapabilities is the fixed set of optional-feature flags this daemon
// advertises via GET /api/v1/capabilities, for client version-skew negotiation.
// "terminal-sessions" means the session `kind` field is supported end-to-end:
// kind=terminal on spawn, ?kind= on list, `kind` on every row, and `terminal`
// removed as a backend.
// "scheduled-agents" means schedule↔session linkage is supported end-to-end:
// schedule-fired runs carry `schedule_id`/`schedule_name` on every session
// surface (list/get/SSE), the Schedule row carries durable last_run_session_id/
// last_run_status, and the control verbs GET /schedules/{id} and
// POST /schedules/{id}/enable|disable exist.
// Append new flags here as capabilities land; never rename or drop a shipped flag
// (clients feature-detect on the exact string).
// "store-health" means the daemon keeps active fleet reads complete-or-error
// (a degraded active scan returns 503 from GET /api/v1/sessions and is never a
// silent partial) and exposes GET /api/v1/store/health for operator/TUI
// diagnostics.
var serverCapabilities = []string{"terminal-sessions", "scheduled-agents", "store-health", "backend-recovery"}

// GetCapabilities implements GET /api/v1/capabilities.
func (s *Server) GetCapabilities(_ context.Context, _ oapi.GetCapabilitiesRequestObject) (oapi.GetCapabilitiesResponseObject, error) {
	caps := append([]string(nil), serverCapabilities...) // copy so the response can't alias the package var
	return oapi.GetCapabilities200JSONResponse{Capabilities: caps}, nil
}

// backendFor resolves a session's agent backend, falling back to the Claude
// default for an empty or unrecognized backend id (back-compat).
func backendFor(id string) agentbackend.Backend {
	if b, err := agentbackend.Get(id); err == nil && b != nil {
		return b
	}
	return agentbackend.Default()
}

// approvalView builds the wire view for a session's pending approval by parsing
// the pane through its backend (each backend recognizes its own prompt UI).
func approvalView(b agentbackend.Backend, id, pane string) approval.View {
	ap, ok := b.ParseApproval(pane)
	if !ok || ap == nil {
		return approval.View{ID: id, Recognized: false}
	}
	return approval.View{
		ID:          id,
		Action:      ap.Action,
		Question:    ap.Question,
		Options:     ap.Options,
		Fingerprint: approval.Fingerprint(ap.Options),
		Recognized:  true,
	}
}

// ListSessions implements GET /api/v1/sessions. store.List returns pointers; the
// generated SessionList holds values, so deref into a non-nil slice (the spec
// marks sessions required, so an empty list still emits []).
//
// An optional ?kind= filter narrows the result to one session kind: "agent"
// returns only AI agents (kind empty or "agent"), "terminal" only plain shells.
// Omitted (the default) returns every session, unchanged.
func (s *Server) ListSessions(ctx context.Context, req oapi.ListSessionsRequestObject) (oapi.ListSessionsResponseObject, error) {
	sessions, err := s.store.List(ctx)
	if err != nil {
		if d, ok := store.IsDegraded(err); ok {
			// Complete-or-error: a degraded active scan is 503, never a silent
			// partial fleet. The client/TUI keeps its last-known-good snapshot.
			logStoreDegraded(d)
			return nil, errStatus(http.StatusServiceUnavailable, d.Error())
		}
		return nil, err
	}
	out := make([]oapi.Session, 0, len(sessions))
	for _, ss := range sessions {
		if !req.Params.All && ss.HasTag("system:true") {
			continue
		}
		if !kindMatches(req.Params.Kind, ss.IsTerminal()) {
			continue
		}
		out = append(out, *ss)
	}
	return oapi.ListSessions200JSONResponse{Sessions: out}, nil
}

// kindMatches reports whether a session (isTerminal) passes the ?kind= filter.
// An empty filter matches everything (the default); "terminal" matches only
// terminals; anything else ("agent") matches only agents.
func kindMatches(filter oapi.ListSessionsParamsKind, isTerminal bool) bool {
	switch filter {
	case "":
		return true
	case oapi.ListSessionsParamsKindTerminal:
		return isTerminal
	default:
		return !isTerminal
	}
}

// GetSession implements GET /api/v1/sessions/{id}.
func (s *Server) GetSession(ctx context.Context, req oapi.GetSessionRequestObject) (oapi.GetSessionResponseObject, error) {
	sess, err := s.store.GetByNameOrID(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return oapi.GetSession404JSONResponse{Error: "session not found"}, nil
	}
	if err != nil {
		return nil, err
	}
	return oapi.GetSession200JSONResponse(*sess), nil
}

// IngestEvent implements POST /api/v1/events. It never errors a hook for an
// unknown session — that path fails soft to 204.
func (s *Server) IngestEvent(ctx context.Context, req oapi.IngestEventRequestObject) (oapi.IngestEventResponseObject, error) {
	if req.Body == nil {
		return oapi.IngestEvent204Response{}, nil
	}
	b := *req.Body
	ev := store.Event{Type: b.Type, Detail: b.Detail}
	to := statusForHook(b.Type)
	// Append the event and apply any status transition in one atomic write so a
	// crash can't log the event without the status change (or vice versa).
	if err := s.store.AppendEventStatus(ctx, b.Session, ev, to); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return oapi.IngestEvent204Response{}, nil
		}
		return nil, err
	}
	s.notify()
	// The SessionEnd hook moves a session to a terminal status (done) — reconcile
	// the owning pipeline job (see reconcileJobOnTerminal).
	if to == store.StatusDone {
		if sess, gerr := s.store.Get(ctx, b.Session); gerr == nil {
			s.reconcileJobOnTerminal(sess, to)
		}
	}
	return oapi.IngestEvent200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "ok"}}, nil
}

// GuardHook implements POST /api/v1/hooks/guard. It fails soft to allow in every
// direction it is not certain about (an unknown session is already gone or never
// warden-spawned).
func (s *Server) GuardHook(ctx context.Context, req oapi.GuardHookRequestObject) (oapi.GuardHookResponseObject, error) {
	var b oapi.GuardRequest
	if req.Body != nil {
		b = *req.Body
	}
	sess, err := s.store.Get(ctx, b.Session)
	if err != nil {
		return oapi.GuardHook200JSONResponse{Decision: "allow"}, nil
	}
	if deny, reason := guardDecision(sess, b.Tool, b.Path); deny {
		return oapi.GuardHook200JSONResponse{Decision: "deny", Reason: reason}, nil
	}
	return oapi.GuardHook200JSONResponse{Decision: "allow"}, nil
}

// ListDirs implements GET /api/v1/fs/dirs: the immediate subdirectories of
// ?path= (defaulting to the user's home directory).
func (s *Server) ListDirs(_ context.Context, req oapi.ListDirsRequestObject) (oapi.ListDirsResponseObject, error) {
	path := req.Params.Path
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, errStatus(http.StatusInternalServerError, "no home directory: "+err.Error())
		}
		path = home
	}
	path = filepath.Clean(path)
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		return nil, errStatus(http.StatusBadRequest, "not a directory: "+path)
	}
	items, err := os.ReadDir(path)
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, "cannot read directory: "+err.Error())
	}
	entries := []oapi.DirEntry{}
	for _, it := range items {
		if !it.IsDir() || strings.HasPrefix(it.Name(), ".") {
			continue
		}
		entries = append(entries, oapi.DirEntry{Name: it.Name(), Path: filepath.Join(path, it.Name())})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	parent := filepath.Dir(path)
	if parent == path {
		parent = "" // already at the filesystem root
	}
	return oapi.ListDirs200JSONResponse{Path: path, Parent: parent, Entries: entries}, nil
}

// CloneRepo implements POST /api/v1/fs/clone: clones url via `git clone` into
// <workspace_path>/<repo-name> (workspace_path from the live config,
// config.defaultWorkspaceDir when unset), creating the workspace directory if
// needed. Backs the TUI's "Open remote project" flow.
func (s *Server) CloneRepo(ctx context.Context, req oapi.CloneRepoRequestObject) (oapi.CloneRepoResponseObject, error) {
	var b oapi.CloneRepoRequest
	if req.Body != nil {
		b = *req.Body
	}
	remote := strings.TrimSpace(b.Url)
	if remote == "" {
		return nil, errStatus(http.StatusBadRequest, "url is required")
	}
	name := repoNameFromURL(remote)
	if name == "" {
		return nil, errStatus(http.StatusBadRequest, "cannot derive a directory name from url: "+remote)
	}
	workspace := s.snapshotConfig().WorkspacePath
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, errStatus(http.StatusInternalServerError, "cannot create workspace dir: "+err.Error())
	}
	dest := filepath.Join(workspace, name)
	if _, err := os.Stat(dest); err == nil {
		return nil, errStatus(http.StatusBadRequest, "destination already exists: "+dest)
	}
	out, err := exec.CommandContext(ctx, "git", "clone", remote, dest).CombinedOutput()
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "git clone failed: "+strings.TrimSpace(string(out)))
	}
	return oapi.CloneRepo200JSONResponse{Dir: dest}, nil
}

// repoNameFromURL derives a destination directory name from a git remote URL,
// e.g. "https://github.com/user/repo.git" or "git@github.com:user/repo.git" ->
// "repo". Returns "" when no usable name can be derived (e.g. a bare host).
func repoNameFromURL(remote string) string {
	trimmed := strings.TrimSuffix(strings.TrimRight(remote, "/"), ".git")
	if i := strings.LastIndexAny(trimmed, "/:"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	return trimmed
}

// ListApprovals implements GET /api/v1/approvals: the live queue of every
// waiting-for-input session parsed from its stored pane excerpt.
func (s *Server) ListApprovals(ctx context.Context, _ oapi.ListApprovalsRequestObject) (oapi.ListApprovalsResponseObject, error) {
	if !s.approvals {
		return oapi.ListApprovals200JSONResponse{Enabled: false, Approvals: []oapi.ApprovalView{}}, nil
	}
	sessions, err := s.store.List(ctx)
	if err != nil {
		if d, ok := store.IsDegraded(err); ok {
			// Same complete-or-error contract as ListSessions: a degraded active scan
			// is a 503, never an approval queue built from a partial fleet.
			logStoreDegraded(d)
			return nil, errStatus(http.StatusServiceUnavailable, d.Error())
		}
		return nil, err
	}
	views := []oapi.ApprovalView{}
	for _, sess := range sessions {
		// Terminals never surface approvals (a shell has no yes/no prompt warden
		// answers); guard by kind so they can't leak into the queue even if some
		// path parks one at waiting_for_input.
		if sess.Status != store.StatusWaitingForInput || sess.IsTerminal() {
			continue
		}
		views = append(views, approvalView(backendFor(sess.Backend), sess.ID, sess.LastPaneExcerpt))
	}
	return oapi.ListApprovals200JSONResponse{Enabled: true, Approvals: views}, nil
}

// ApproveSession implements POST /api/v1/sessions/{id}/approve with the
// re-verify guard: re-capture the pane, re-parse, and inject the digit ONLY if
// the fingerprint still matches — otherwise 409, so a prompt that changed
// underneath the user is never answered.
func (s *Server) ApproveSession(ctx context.Context, req oapi.ApproveSessionRequestObject) (oapi.ApproveSessionResponseObject, error) {
	if !s.approvals {
		return nil, errStatus(http.StatusForbidden, "approvals disabled")
	}
	sess, err := s.store.Get(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return oapi.ApproveSession404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "session not found"}}, nil
	}
	if err != nil {
		return nil, err
	}
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "bad json")
	}
	b := *req.Body
	pane, err := s.life.Output(ctx, sess.TmuxSession, 200)
	if err != nil {
		return nil, err
	}
	a, ok := backendFor(sess.Backend).ParseApproval(pane)
	if !ok || a == nil || approval.Fingerprint(a.Options) != b.Fingerprint {
		return nil, errStatus(http.StatusConflict, "prompt changed; reopen")
	}
	if b.Option < 1 || b.Option > len(a.Options) {
		return nil, errStatus(http.StatusBadRequest, "option out of range")
	}
	if err := s.life.SendKeys(ctx, sess.TmuxSession, strconv.Itoa(b.Option)); err != nil {
		return nil, err
	}
	s.notify()
	s.recordAuditCtx(ctx, audit.ActionApprove, req.Id, map[string]string{"option": strconv.Itoa(b.Option)})
	return oapi.ApproveSession200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "answered"}}, nil
}
