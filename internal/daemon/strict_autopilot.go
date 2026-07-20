package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
)

// autopilotDisabledMsg is returned when the feature is unconfigured (no
// Controller wired). The daemon builds one from config at startup, so this only
// happens on a bare Server literal (some tests).
const autopilotDisabledMsg = "autopilot is not configured"

// GetAutopilot implements GET /api/v1/autopilot: the master switch + per-run
// status (autopilot.md §5). Reports disabled/empty when unconfigured.
func (s *Server) GetAutopilot(_ context.Context, _ oapi.GetAutopilotRequestObject) (oapi.GetAutopilotResponseObject, error) {
	if s.autopilot == nil {
		return oapi.GetAutopilot200JSONResponse(autopilot.Status{Enabled: false, Runs: []autopilot.RunStatus{}}), nil
	}
	return oapi.GetAutopilot200JSONResponse(s.autopilot.Status()), nil
}

// SetAutopilot implements POST /api/v1/autopilot: flip the master switch. Enabling
// runs the enable-time preflight and returns 409 with the full failure list on
// any failure (autopilot.md §5.1), changing no state. Disabling is the kill
// switch.
func (s *Server) SetAutopilot(ctx context.Context, req oapi.SetAutopilotRequestObject) (oapi.SetAutopilotResponseObject, error) {
	if s.autopilot == nil {
		return nil, errStatus(http.StatusForbidden, autopilotDisabledMsg)
	}
	var b oapi.AutopilotToggleRequest
	if req.Body != nil {
		b = *req.Body
	}
	if b.Enabled {
		st, err := s.autopilot.Enable(ctx)
		var pfe *autopilot.PreflightError
		if errors.As(err, &pfe) {
			return oapi.SetAutopilot409JSONResponse{
				Error:    "autopilot enable-time preflight failed",
				Failures: pfe.Failures,
			}, nil
		}
		if err != nil {
			return nil, err
		}
		s.recordAuditCtx(ctx, audit.ActionAutopilotOn, "", map[string]string{"runs": strconv.Itoa(len(st.Runs))})
		return oapi.SetAutopilot200JSONResponse(st), nil
	}
	st := s.autopilot.Disable(ctx)
	s.recordAuditCtx(ctx, audit.ActionAutopilotOff, "", nil)
	return oapi.SetAutopilot200JSONResponse(st), nil
}

// CompleteAutopilot implements POST /api/v1/autopilot/complete: the brain's
// completion signal (autopilot.md §2.1). The owning run is derived from the
// CALLING brain's own session identity (the actor header) — a brain may only
// complete its own run — so the request carries no body. The Controller writes
// the in-place completion marker into the plan file (preflight then skips it),
// tears the brain down, and retains the ledger. Idempotent. A caller that is not
// an autopilot brain (a human, the web UI, an ordinary agent, or a brain with no
// run tag) gets 403 with nothing changed.
func (s *Server) CompleteAutopilot(ctx context.Context, _ oapi.CompleteAutopilotRequestObject) (oapi.CompleteAutopilotResponseObject, error) {
	if s.autopilot == nil {
		return oapi.CompleteAutopilot403JSONResponse{Error: autopilotDisabledMsg}, nil
	}
	caller := s.callerSession(ctx)
	if caller == nil || caller.Role != autopilotBrainRole {
		return oapi.CompleteAutopilot403JSONResponse{Error: "only an autopilot brain may complete its run"}, nil
	}
	runID := runIDFromTags(caller.Tags)
	if runID == "" {
		return oapi.CompleteAutopilot403JSONResponse{Error: "calling brain carries no run tag — nothing to complete"}, nil
	}
	st, err := s.autopilot.CompleteRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionAutopilotComplete, runID, nil)
	return oapi.CompleteAutopilot200JSONResponse(st), nil
}
