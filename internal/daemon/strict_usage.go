package daemon

import (
	"context"
	"net/http"

	"github.com/srjn45/warden/internal/daemon/oapi"
)

func (s *Server) GetUsage(ctx context.Context, req oapi.GetUsageRequestObject) (oapi.GetUsageResponseObject, error) {
	if s.usage == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend usage unavailable")
	}
	snapshot, err := s.usage.Snapshot(ctx, req.Params.Refresh)
	if err != nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend usage unavailable")
	}
	// Display path stays Snapshot; SyncToStore is the separate write into backendstore (D5).
	s.syncUsageSnapshot(snapshot)
	return oapi.GetUsage200JSONResponse(snapshot), nil
}

func (s *Server) RecoverUsage(ctx context.Context, req oapi.RecoverUsageRequestObject) (oapi.RecoverUsageResponseObject, error) {
	body := oapi.UsageRecoverRequest{}
	if req.Body != nil {
		body = *req.Body
	}
	result, err := s.runUsageRecover(ctx, body)
	if err != nil {
		return nil, err
	}
	return oapi.RecoverUsage200JSONResponse(result), nil
}
