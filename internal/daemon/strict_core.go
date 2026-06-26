package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
)

// ListSessions implements GET /api/v1/sessions. store.List returns pointers; the
// generated SessionList holds values, so deref into a non-nil slice (the spec
// marks sessions required, so an empty list still emits []).
func (s *Server) ListSessions(ctx context.Context, _ oapi.ListSessionsRequestObject) (oapi.ListSessionsResponseObject, error) {
	sessions, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]oapi.Session, 0, len(sessions))
	for _, ss := range sessions {
		out = append(out, *ss)
	}
	return oapi.ListSessions200JSONResponse{Sessions: out}, nil
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

// ListApprovals implements GET /api/v1/approvals: the live queue of every
// waiting-for-input session parsed from its stored pane excerpt.
func (s *Server) ListApprovals(ctx context.Context, _ oapi.ListApprovalsRequestObject) (oapi.ListApprovalsResponseObject, error) {
	if !s.approvals {
		return oapi.ListApprovals200JSONResponse{Enabled: false, Approvals: []oapi.ApprovalView{}}, nil
	}
	sessions, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	views := []oapi.ApprovalView{}
	for _, sess := range sessions {
		if sess.Status != store.StatusWaitingForInput {
			continue
		}
		views = append(views, approval.BuildView(sess.ID, sess.LastPaneExcerpt))
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
	a, ok := approval.Parse(pane)
	if !ok || approval.Fingerprint(a.Options) != b.Fingerprint {
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
