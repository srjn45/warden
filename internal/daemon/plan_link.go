package daemon

import (
	"context"
	"errors"
	"net/http"

	"github.com/srjn45/warden/internal/planstore"
)

// validatePlanLink checks the optional Agent/Pipeline → Plan back-ref at
// creation time (plan-links-on-agent-pipeline).
//
// Rules:
//   - empty planID is always valid (planless executor)
//   - a non-empty planID must name an existing plan whose ProjectID equals the
//     resolved projectID for the executor being created
//
// Returns (0, "") when acceptable, otherwise an HTTP status + message for
// errStatus. Does not mutate membership or execution state.
func (s *Server) validatePlanLink(ctx context.Context, planID, projectID string) (int, string) {
	if planID == "" {
		return 0, ""
	}
	if s.plans == nil {
		return http.StatusServiceUnavailable, "plans store not configured"
	}
	p, err := s.plans.Get(ctx, planID)
	if errors.Is(err, planstore.ErrNotFound) {
		return http.StatusBadRequest, "plan not found: " + planID
	}
	if err != nil {
		return http.StatusInternalServerError, "failed to look up plan: " + err.Error()
	}
	if projectID == "" {
		return http.StatusBadRequest, "plan_id requires the agent/pipeline to belong to a project"
	}
	if p.ProjectID != projectID {
		return http.StatusBadRequest, "plan " + planID + " does not belong to project " + projectID
	}
	return 0, ""
}
