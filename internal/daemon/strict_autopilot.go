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
