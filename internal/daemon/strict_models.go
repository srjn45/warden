package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/store"
)

// ListModels implements GET /api/v1/models: list models in the catalog and their assigned tiers.
func (s *Server) ListModels(_ context.Context, req oapi.ListModelsRequestObject) (oapi.ListModelsResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	var tierFilter backendstore.ModelTier
	if strings.TrimSpace(req.Params.Tier) != "" {
		tierFilter = backendstore.ModelTier(strings.TrimSpace(req.Params.Tier))
		if !tierFilter.Valid() {
			return nil, errStatus(http.StatusBadRequest, fmt.Sprintf("invalid tier %q (valid: tier-1, tier-2, tier-3)", req.Params.Tier))
		}
	}
	models, err := s.backends.ListModels(tierFilter)
	if err != nil {
		if errors.Is(err, backendstore.ErrInvalidTier) {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
		return nil, err
	}
	if models == nil {
		models = []backendstore.ModelEntry{}
	}
	return oapi.ListModels200JSONResponse(models), nil
}

// SetModelTier implements PUT /api/v1/models/{backend}/{model}/tier: update a model's tier classification.
func (s *Server) SetModelTier(_ context.Context, req oapi.SetModelTierRequestObject) (oapi.SetModelTierResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Tier) == "" {
		return nil, errStatus(http.StatusBadRequest, "tier is required")
	}
	tier := backendstore.ModelTier(strings.TrimSpace(req.Body.Tier))
	if !tier.Valid() {
		return nil, errStatus(http.StatusBadRequest, fmt.Sprintf("invalid tier %q (valid: tier-1, tier-2, tier-3)", req.Body.Tier))
	}
	if err := s.backends.SetModelTier(req.Backend, req.Model, tier); err != nil {
		if errors.Is(err, backendstore.ErrModelNotFound) {
			return nil, errStatus(http.StatusNotFound, fmt.Sprintf("model %s/%s not found", req.Backend, req.Model))
		}
		if errors.Is(err, backendstore.ErrInvalidTier) {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
		return nil, err
	}
	m, err := s.backends.GetModel(req.Backend, req.Model)
	if err != nil {
		return nil, err
	}
	return oapi.SetModelTier200JSONResponse(m), nil
}

// ListRoleTiers implements GET /api/v1/roles/tiers: list role-to-tier mappings.
func (s *Server) ListRoleTiers(_ context.Context, _ oapi.ListRoleTiersRequestObject) (oapi.ListRoleTiersResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	mappings, err := s.backends.ListRoleTiers()
	if err != nil {
		return nil, err
	}
	if mappings == nil {
		mappings = []backendstore.RoleTierMapping{}
	}
	return oapi.ListRoleTiers200JSONResponse(mappings), nil
}

// SetRoleTier implements PUT /api/v1/roles/tiers/{role}: update default model tier for an agent role.
func (s *Server) SetRoleTier(_ context.Context, req oapi.SetRoleTierRequestObject) (oapi.SetRoleTierResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Tier) == "" {
		return nil, errStatus(http.StatusBadRequest, "tier is required")
	}
	tier := backendstore.ModelTier(strings.TrimSpace(req.Body.Tier))
	if !tier.Valid() {
		return nil, errStatus(http.StatusBadRequest, fmt.Sprintf("invalid tier %q (valid: tier-1, tier-2, tier-3)", req.Body.Tier))
	}
	if err := s.backends.SetRoleTier(req.Role, tier); err != nil {
		if errors.Is(err, backendstore.ErrInvalidTier) {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
		return nil, err
	}
	t, err := s.backends.GetRoleTier(req.Role)
	if err != nil {
		return nil, err
	}
	return oapi.SetRoleTier200JSONResponse(backendstore.RoleTierMapping{
		RoleName:    req.Role,
		DefaultTier: t,
	}), nil
}

// SwitchSession implements POST /api/v1/sessions/{id}/switch: hot-swap an agent mid-task.
func (s *Server) SwitchSession(ctx context.Context, req oapi.SwitchSessionRequestObject) (oapi.SwitchSessionResponseObject, error) {
	sess, err := s.store.GetByNameOrID(ctx, req.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "session not found")
	}
	if err != nil {
		return nil, err
	}
	if s.recovery != nil {
		s.recovery.Supersede(ctx, sess.ID, "manual_switch")
	}

	var swapReq lifecycle.SwapRequest
	if req.Body != nil {
		swapReq.Backend = strings.TrimSpace(req.Body.Backend)
		swapReq.Model = strings.TrimSpace(req.Body.Model)
		if strings.TrimSpace(req.Body.Tier) != "" {
			tier := backendstore.ModelTier(strings.TrimSpace(req.Body.Tier))
			if !tier.Valid() {
				return nil, errStatus(http.StatusBadRequest, fmt.Sprintf("invalid tier %q (valid: tier-1, tier-2, tier-3)", req.Body.Tier))
			}
			swapReq.Tier = tier
		}
		swapReq.Role = strings.TrimSpace(req.Body.Role)
		if strings.TrimSpace(req.Body.Reason) != "" {
			swapReq.Reason = lifecycle.SwapReason(strings.TrimSpace(req.Body.Reason))
		}
		swapReq.Prompt = req.Body.Prompt
	}

	res, err := s.life.HotSwap(ctx, sess, swapReq)
	if err != nil {
		if errors.Is(err, lifecycle.ErrNoSwapTarget) || errors.Is(err, lifecycle.ErrNoResolver) {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
		return nil, errStatus(http.StatusInternalServerError, "hot-swap failed: "+err.Error())
	}
	s.notify()
	return oapi.SwitchSession200JSONResponse(*res), nil
}

// GetHandoverSettings implements GET /api/v1/handover/settings: retrieve handover configuration.
func (s *Server) GetHandoverSettings(_ context.Context, _ oapi.GetHandoverSettingsRequestObject) (oapi.GetHandoverSettingsResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	settings, err := s.backends.GetHandoverSettings()
	if err != nil {
		return nil, err
	}
	return oapi.GetHandoverSettings200JSONResponse(settings), nil
}

// SetHandoverSettings implements PUT /api/v1/handover/settings: update handover configuration.
func (s *Server) SetHandoverSettings(_ context.Context, req oapi.SetHandoverSettingsRequestObject) (oapi.SetHandoverSettingsResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "handover settings body required")
	}
	if err := s.backends.SetHandoverSettings(*req.Body); err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	settings, err := s.backends.GetHandoverSettings()
	if err != nil {
		return nil, err
	}
	return oapi.SetHandoverSettings200JSONResponse(settings), nil
}
