package daemon

import (
	"context"
	"net/http"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/daemon/oapi"
)

// SetAutoApprovePersist wires the durable half of the auto-approve policy
// endpoint: the callback persists a replaced policy (the daemon points it at
// config.WriteAutoApprove). A nil callback (the default) makes PUT runtime-only —
// the live policy changes but is not written back to the config file.
func (s *Server) SetAutoApprovePersist(fn func(approval.Policy) error) {
	s.autoApprovePersist = fn
}

// GetAutoApprovePolicy implements GET /api/v1/auto-approve/policy: the live
// policy the poller is running (default rules + per-agent overrides).
func (s *Server) GetAutoApprovePolicy(_ context.Context, _ oapi.GetAutoApprovePolicyRequestObject) (oapi.GetAutoApprovePolicyResponseObject, error) {
	if s.poller == nil {
		return oapi.GetAutoApprovePolicy200JSONResponse(approval.Policy{}), nil
	}
	return oapi.GetAutoApprovePolicy200JSONResponse(s.poller.AutoApprovePolicySnapshot()), nil
}

// SetAutoApprovePolicy implements PUT /api/v1/auto-approve/policy: validate the
// submitted policy, swap it into the poller (effective immediately), and persist
// it to config when a writer is wired. Returns the stored policy.
func (s *Server) SetAutoApprovePolicy(_ context.Context, req oapi.SetAutoApprovePolicyRequestObject) (oapi.SetAutoApprovePolicyResponseObject, error) {
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "missing policy body")
	}
	pol := approval.Policy(*req.Body)
	if err := pol.Validate(); err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	if s.poller != nil {
		s.poller.SetAutoApprovePolicy(pol)
	}
	if s.autoApprovePersist != nil {
		if err := s.autoApprovePersist(pol); err != nil {
			return nil, err
		}
	}
	s.notify()
	return oapi.SetAutoApprovePolicy200JSONResponse(pol), nil
}
