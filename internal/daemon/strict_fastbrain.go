package daemon

import (
	"context"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/fastbrain"
)

// fastBrainInspector returns the engine's telemetry/control surface, or nil
// when the wired engine is a test double without one.
func (s *Server) fastBrainInspector() fastbrain.Inspector {
	if i, ok := s.fastBrain.(fastbrain.Inspector); ok {
		return i
	}
	return nil
}

// GetFastBrainMetrics implements GET /api/v1/fastbrain/metrics. The snapshot
// is content-free by construction (bounded label vocabulary only).
func (s *Server) GetFastBrainMetrics(_ context.Context, _ oapi.GetFastBrainMetricsRequestObject) (oapi.GetFastBrainMetricsResponseObject, error) {
	insp := s.fastBrainInspector()
	if insp == nil {
		return oapi.GetFastBrainMetrics200JSONResponse(fastbrain.TelemetrySnapshot{Now: time.Now()}), nil
	}
	return oapi.GetFastBrainMetrics200JSONResponse(insp.Telemetry()), nil
}

// ListFastBrainDecisions implements GET /api/v1/fastbrain/decisions.
func (s *Server) ListFastBrainDecisions(_ context.Context, req oapi.ListFastBrainDecisionsRequestObject) (oapi.ListFastBrainDecisionsResponseObject, error) {
	out := []fastbrain.Decision{}
	if insp := s.fastBrainInspector(); insp != nil {
		limit := 50
		if req.Params.Limit > 0 {
			limit = req.Params.Limit
		}
		out = insp.Decisions(limit)
	}
	return oapi.ListFastBrainDecisions200JSONResponse{Decisions: out}, nil
}

// SetFastBrainControl implements PUT /api/v1/fastbrain/controls/{kind}.
func (s *Server) SetFastBrainControl(ctx context.Context, req oapi.SetFastBrainControlRequestObject) (oapi.SetFastBrainControlResponseObject, error) {
	insp := s.fastBrainInspector()
	if insp == nil {
		return nil, errStatus(http.StatusNotFound, "fast-brain engine not available")
	}
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "body required")
	}
	ttl := time.Duration(0)
	if req.Body.TtlSeconds > 0 {
		ttl = time.Duration(req.Body.TtlSeconds) * time.Second
	}
	kind := fastbrain.DecisionKind(req.Kind)
	if err := insp.SetKindPaused(kind, req.Body.Paused, ttl); err != nil {
		return nil, errStatus(http.StatusNotFound, "unknown decision kind")
	}
	detail := map[string]string{"paused": "false"}
	if req.Body.Paused {
		detail["paused"] = "true"
	}
	s.recordAuditCtx(ctx, audit.ActionFastBrainControl, req.Kind, detail)
	return oapi.SetFastBrainControl200JSONResponse{Controls: insp.Telemetry().Controls}, nil
}
