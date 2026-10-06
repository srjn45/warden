package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/knownprompts"
)

// knownStore is the poller's known-prompts store, or nil when the feature is absent.
func (s *Server) knownStore() *knownprompts.Store {
	if s.poller == nil {
		return nil
	}
	return s.poller.Known
}

// ListKnownPrompts implements GET /api/v1/known-prompts.
func (s *Server) ListKnownPrompts(_ context.Context, _ oapi.ListKnownPromptsRequestObject) (oapi.ListKnownPromptsResponseObject, error) {
	out := []knownprompts.Entry{}
	if ks := s.knownStore(); ks != nil {
		out = append(out, ks.List()...)
	}
	return oapi.ListKnownPrompts200JSONResponse{Prompts: out}, nil
}

// ForgetKnownPrompt implements DELETE /api/v1/known-prompts/{id}.
func (s *Server) ForgetKnownPrompt(ctx context.Context, req oapi.ForgetKnownPromptRequestObject) (oapi.ForgetKnownPromptResponseObject, error) {
	ks := s.knownStore()
	if ks == nil {
		return nil, errStatus(http.StatusNotFound, "known prompt not found")
	}
	if err := ks.Delete(ctx, req.Id); errors.Is(err, knownprompts.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "known prompt not found")
	} else if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionKnownPromptForget, req.Id, nil)
	return oapi.ForgetKnownPrompt200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "forgotten"}}, nil
}

// ForgetAllKnownPrompts implements DELETE /api/v1/known-prompts.
func (s *Server) ForgetAllKnownPrompts(ctx context.Context, _ oapi.ForgetAllKnownPromptsRequestObject) (oapi.ForgetAllKnownPromptsResponseObject, error) {
	n := 0
	if ks := s.knownStore(); ks != nil {
		var err error
		if n, err = ks.DeleteAll(ctx); err != nil {
			return nil, err
		}
	}
	s.recordAuditCtx(ctx, audit.ActionKnownPromptForgetAll, "", map[string]string{"removed": strconv.Itoa(n)})
	return oapi.ForgetAllKnownPrompts200JSONResponse{Removed: n}, nil
}
