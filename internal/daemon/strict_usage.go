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
	return oapi.GetUsage200JSONResponse(snapshot), nil
}
