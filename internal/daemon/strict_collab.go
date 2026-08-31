package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

// ListContext implements GET /api/v1/context.
func (s *Server) ListContext(_ context.Context, req oapi.ListContextRequestObject) (oapi.ListContextResponseObject, error) {
	entries, err := s.cstore.List(req.Params.Prefix)
	if err != nil {
		return nil, err
	}
	return oapi.ListContext200JSONResponse{Entries: entries}, nil
}

// GetContext implements GET /api/v1/context/{key}.
func (s *Server) GetContext(_ context.Context, req oapi.GetContextRequestObject) (oapi.GetContextResponseObject, error) {
	e, err := s.cstore.Get(req.Key)
	if errors.Is(err, ctxstore.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "context key not found")
	}
	if err != nil {
		return nil, err
	}
	return oapi.GetContext200JSONResponse(e), nil
}

// SetContext implements PUT /api/v1/context/{key}.
func (s *Server) SetContext(_ context.Context, req oapi.SetContextRequestObject) (oapi.SetContextResponseObject, error) {
	var b oapi.SetContextJSONRequestBody
	if req.Body != nil {
		b = *req.Body
	}
	by, err := sanitizeSender(b.By)
	if errors.Is(err, errReservedSender) {
		return nil, errStatus(http.StatusForbidden, "writer id is reserved for the daemon")
	}
	e, err := s.cstore.Set(req.Key, b.Value, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		return nil, errStatus(http.StatusBadRequest, "invalid key")
	}
	if err != nil {
		return nil, err
	}
	return oapi.SetContext200JSONResponse(e), nil
}

// AppendContext implements POST /api/v1/context/{key}/append.
func (s *Server) AppendContext(_ context.Context, req oapi.AppendContextRequestObject) (oapi.AppendContextResponseObject, error) {
	var b oapi.AppendContextJSONRequestBody
	if req.Body != nil {
		b = *req.Body
	}
	by, err := sanitizeSender(b.By)
	if errors.Is(err, errReservedSender) {
		return nil, errStatus(http.StatusForbidden, "writer id is reserved for the daemon")
	}
	e, err := s.cstore.Append(req.Key, b.Value, b.Sep, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		return nil, errStatus(http.StatusBadRequest, "invalid key")
	}
	if err != nil {
		return nil, err
	}
	return oapi.AppendContext200JSONResponse(e), nil
}

// CasContext implements POST /api/v1/context/{key}/cas.
func (s *Server) CasContext(_ context.Context, req oapi.CasContextRequestObject) (oapi.CasContextResponseObject, error) {
	var b oapi.CasContextJSONRequestBody
	if req.Body != nil {
		b = *req.Body
	}
	by, err := sanitizeSender(b.By)
	if errors.Is(err, errReservedSender) {
		return nil, errStatus(http.StatusForbidden, "writer id is reserved for the daemon")
	}
	e, err := s.cstore.CompareAndSet(req.Key, b.Expected, b.Value, by)
	if errors.Is(err, ctxstore.ErrBadKey) {
		return nil, errStatus(http.StatusBadRequest, "invalid key")
	}
	if errors.Is(err, ctxstore.ErrConflict) {
		return nil, errStatus(http.StatusConflict, "value conflict: current value does not match expected")
	}
	if err != nil {
		return nil, err
	}
	return oapi.CasContext200JSONResponse(e), nil
}

// DeleteContext implements DELETE /api/v1/context/{key}.
func (s *Server) DeleteContext(_ context.Context, req oapi.DeleteContextRequestObject) (oapi.DeleteContextResponseObject, error) {
	err := s.cstore.Del(req.Key)
	if errors.Is(err, ctxstore.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "context key not found")
	}
	if err != nil {
		return nil, err
	}
	return oapi.DeleteContext200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "deleted"}}, nil
}

// ListRecentMessages implements GET /api/v1/messages: a read-only global view of
// recent traffic that never marks anything read.
func (s *Server) ListRecentMessages(_ context.Context, req oapi.ListRecentMessagesRequestObject) (oapi.ListRecentMessagesResponseObject, error) {
	all, err := s.mbox.All()
	if err != nil {
		return nil, err
	}
	return oapi.ListRecentMessages200JSONResponse{Messages: recentMessages(all, req.Params.Limit)}, nil
}

// GetInbox implements GET /api/v1/sessions/{id}/messages: returned messages are
// marked read.
func (s *Server) GetInbox(_ context.Context, req oapi.GetInboxRequestObject) (oapi.GetInboxResponseObject, error) {
	msgs, err := s.mbox.Messages(req.Id)
	if errors.Is(err, mailbox.ErrBadRecipient) {
		return nil, errStatus(http.StatusBadRequest, "invalid recipient")
	}
	if err != nil {
		return nil, err
	}
	out := []mailbox.Message{}
	ids := []string{}
	for _, m := range msgs {
		if req.Params.Unread && m.Read {
			continue
		}
		out = append(out, m)
		if !m.Read {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) > 0 {
		if err := s.mbox.MarkRead(req.Id, ids); err != nil {
			return nil, err
		}
	}
	return oapi.GetInbox200JSONResponse{Messages: out}, nil
}

// SendMessage implements POST /api/v1/sessions/{id}/messages. The inbox is the
// source of truth; the wake is best-effort.
func (s *Server) SendMessage(ctx context.Context, req oapi.SendMessageRequestObject) (oapi.SendMessageResponseObject, error) {
	if _, err := s.store.Get(ctx, req.Id); errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	} else if err != nil {
		return nil, err
	}
	var b oapi.SendMessageJSONRequestBody
	if req.Body != nil {
		b = *req.Body
	}
	if b.Body == "" {
		return nil, errStatus(http.StatusBadRequest, "empty message body")
	}
	from, err := sanitizeSender(b.From)
	if errors.Is(err, errReservedSender) {
		return nil, errStatus(http.StatusForbidden, "sender id is reserved for the daemon")
	}
	m, err := s.mbox.Append(mailbox.Message{To: req.Id, From: from, Body: b.Body})
	if errors.Is(err, mailbox.ErrBadRecipient) {
		return nil, errStatus(http.StatusBadRequest, "invalid recipient")
	}
	if err != nil {
		return nil, err
	}
	// Re-Get status as close to the injection as possible to shrink the
	// idle→working TOCTOU; a parked recipient is woken with an injected notice.
	woke := false
	if fresh, gerr := s.store.Get(ctx, req.Id); gerr == nil && parked(fresh.Status) {
		notice := fmt.Sprintf("📨 New message from %s. Run `warden msg inbox` to read.", from)
		if err := s.life.Input(ctx, fresh.TmuxSession, notice); err == nil {
			woke = true
		}
	}
	s.notify() // release any blocking waiters
	return oapi.SendMessage201JSONResponse{Message: m, Woke: woke}, nil
}

// WaitForMessage implements GET /api/v1/sessions/{id}/messages/wait: a server
// long-poll that returns as soon as an unread message arrives or found=false
// when the timeout elapses.
func (s *Server) WaitForMessage(ctx context.Context, req oapi.WaitForMessageRequestObject) (oapi.WaitForMessageResponseObject, error) {
	timeoutSec := defaultWaitSec
	if n := req.Params.Timeout; n > 0 {
		timeoutSec = n
	}
	if timeoutSec > maxWaitSec {
		timeoutSec = maxWaitSec
	}

	ch, unsub := s.hub.subscribe()
	defer unsub()

	deadline := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer deadline.Stop()

	for {
		m, ok, err := s.mbox.TakeFirstUnread(req.Id, req.Params.From)
		if errors.Is(err, mailbox.ErrBadRecipient) {
			return nil, errStatus(http.StatusBadRequest, "invalid recipient")
		}
		if err != nil {
			return nil, err
		}
		if ok {
			msg := m
			return oapi.WaitForMessage200JSONResponse{Found: true, Message: &msg}, nil
		}
		select {
		case <-ctx.Done():
			return oapi.WaitForMessage200JSONResponse{Found: false}, nil // client hung up
		case <-s.done:
			return oapi.WaitForMessage200JSONResponse{Found: false}, nil
		case <-deadline.C:
			return oapi.WaitForMessage200JSONResponse{Found: false}, nil
		case <-ch:
			// something changed — loop and re-check the inbox
		}
	}
}

// GetConflicts implements GET /api/v1/collab/conflicts.
func (s *Server) GetConflicts(ctx context.Context, _ oapi.GetConflictsRequestObject) (oapi.GetConflictsResponseObject, error) {
	if s.collab == nil {
		return oapi.GetConflicts200JSONResponse{Conflicts: []oapi.Conflict{}}, nil
	}
	conflicts, err := s.collab.Conflicts(ctx)
	if err != nil {
		return nil, err
	}
	if conflicts == nil {
		conflicts = []oapi.Conflict{}
	}
	return oapi.GetConflicts200JSONResponse{Conflicts: conflicts}, nil
}

// GetBranchStatus implements GET /api/v1/collab/branches.
func (s *Server) GetBranchStatus(ctx context.Context, _ oapi.GetBranchStatusRequestObject) (oapi.GetBranchStatusResponseObject, error) {
	if s.branchTracker == nil {
		return oapi.GetBranchStatus200JSONResponse{Branches: []oapi.BranchStatus{}}, nil
	}
	statuses, err := s.branchTracker.Statuses(ctx)
	if err != nil {
		return nil, err
	}
	if statuses == nil {
		statuses = []oapi.BranchStatus{}
	}
	return oapi.GetBranchStatus200JSONResponse{Branches: statuses}, nil
}

// ListHistory implements GET /api/v1/history: the archived (closed) store with
// optional since/type/limit filters.
func (s *Server) ListHistory(ctx context.Context, req oapi.ListHistoryRequestObject) (oapi.ListHistoryResponseObject, error) {
	var typ store.Type
	if req.Params.Type != "" {
		typ = store.NormalizeType(req.Params.Type)
	}
	limit := 0
	if req.Params.Limit > 0 {
		limit = req.Params.Limit
	}
	closed, skipped, err := listClosedWithDegradation(ctx, s.store)
	if err != nil {
		return nil, err
	}
	return oapi.ListHistory200JSONResponse{
		Sessions: derefSessions(filterClosed(closed, req.Params.Since, typ, limit)),
		Degraded: skipped > 0, SkippedRecords: skipped,
	}, nil
}

func listClosedWithDegradation(ctx context.Context, st store.Store) ([]*store.Session, int, error) {
	if reader, ok := st.(store.ArchiveDegradationReader); ok {
		return reader.ListClosedDegraded(ctx)
	}
	closed, err := st.ListClosed(ctx)
	return closed, 0, err
}

// Search implements GET /api/v1/search: an in-memory full-text search across
// sessions (and the archived store with ?closed=true).
func (s *Server) Search(ctx context.Context, req oapi.SearchRequestObject) (oapi.SearchResponseObject, error) {
	query := req.Params.Q
	if strings.TrimSpace(query) == "" {
		return nil, errStatus(http.StatusBadRequest, "empty search query")
	}
	sessions, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	if req.Params.Closed {
		closed, err := s.store.ListClosed(ctx)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, closed...)
	}
	return oapi.Search200JSONResponse{Sessions: derefSessions(searchSessions(sessions, query))}, nil
}

// ImportSessions implements POST /api/v1/import: ingest an export envelope into
// the active store (metadata only, idempotent by id).
func (s *Server) ImportSessions(ctx context.Context, req oapi.ImportSessionsRequestObject) (oapi.ImportSessionsResponseObject, error) {
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "invalid import body: empty")
	}
	res, err := importSessions(ctx, s.store, req.Body, req.Params.Merge)
	if err != nil {
		return nil, errStatus(http.StatusUnprocessableEntity, err.Error())
	}
	return oapi.ImportSessions200JSONResponse(res), nil
}
