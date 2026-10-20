package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
)

// catalogOAPI converts the durable catalog types to their generated transport
// counterparts. Keeping the OpenAPI types at this boundary means the store can
// evolve independently and, importantly, never receives raw HTTP payloads.
func catalogOAPI[T any](in any) (T, error) {
	var out T
	b, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(b, &out)
	return out, err
}

func catalogServiceUnavailable() error {
	return errStatus(http.StatusServiceUnavailable, "backend registry not configured")
}

// PreviewModelCatalog records structured discovery evidence and produces an
// unapplied proposal. It deliberately accepts no path, command or credentials.
func (s *Server) PreviewModelCatalog(_ context.Context, req oapi.PreviewModelCatalogRequestObject) (oapi.PreviewModelCatalogResponseObject, error) {
	if s.backends == nil {
		return nil, catalogServiceUnavailable()
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Aicli) == "" {
		return nil, errStatus(http.StatusBadRequest, "aicli and observation are required")
	}
	b := req.Body
	obs := backendstore.CatalogObservation{
		Source: backendstore.SourceKind(b.Observation.Source), SourceDetail: b.Observation.SourceDetail,
		SourceFetchedAt: b.Observation.SourceFetchedAt, Completeness: backendstore.Completeness(b.Observation.Completeness),
	}
	if b.Observation.Error.Code != "" || b.Observation.Error.Message != "" {
		obs.Err = &backendstore.CatalogError{Code: b.Observation.Error.Code, Message: b.Observation.Error.Message}
	}
	for _, m := range b.Observation.Models {
		if strings.TrimSpace(m.ModelId) == "" {
			return nil, errStatus(http.StatusBadRequest, "observation model_id cannot be empty")
		}
		obs.Models = append(obs.Models, backendstore.ObservedModel{ModelID: m.ModelId, DisplayName: m.DisplayName, Visibility: m.Visibility, ReasoningLevels: m.ReasoningLevels, DefaultReasoning: m.DefaultReasoning, ContextWindow: m.ContextWindow, UpgradeTo: m.UpgradeTo})
	}
	if b.ScopeFp != "" && !backendstore.ValidScopeFP(b.ScopeFp) {
		return nil, errStatus(http.StatusBadRequest, "scope_fp must be sha256: followed by 24 hex digits")
	}
	scope := backendstore.CatalogScope{AiCli: strings.TrimSpace(b.Aicli), AuthMode: b.AuthMode, PlanLabel: b.PlanLabel, CLIVersion: b.CliVersion}
	if b.ScopeFp != "" {
		scope.Fingerprint = b.ScopeFp
	}
	p, err := s.backends.Preview(backendstore.CatalogRefresh{Scope: scope, Observation: obs, ObservedAt: b.ObservedAt, SnapshotID: b.SnapshotId}, time.Now().UTC())
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	s.recordCatalogAudit(audit.ActionModelCatalogRefresh, p, nil)
	out, err := catalogOAPI[oapi.CatalogProposal](p)
	if err != nil {
		return nil, err
	}
	return oapi.PreviewModelCatalog200JSONResponse(out), nil
}

// ApplyModelCatalogProposal changes policy only; it never initiates a session
// switch or recovery, even when the applied catalog affects later routing.
func (s *Server) ApplyModelCatalogProposal(_ context.Context, req oapi.ApplyModelCatalogProposalRequestObject) (oapi.ApplyModelCatalogProposalResponseObject, error) {
	if s.backends == nil {
		return nil, catalogServiceUnavailable()
	}
	r, err := s.backends.ApplyCatalogProposal(req.ProposalId, time.Now().UTC())
	if err != nil {
		if errors.Is(err, backendstore.ErrProposalNotFound) {
			return nil, errStatus(http.StatusNotFound, err.Error())
		}
		if errors.Is(err, backendstore.ErrProposalStale) || errors.Is(err, backendstore.ErrProposalExpired) {
			return nil, errStatus(http.StatusConflict, err.Error())
		}
		return nil, err
	}
	if p, getErr := s.backends.GetCatalogProposal(req.ProposalId); getErr == nil {
		s.recordCatalogAudit(audit.ActionModelCatalogApply, p, &r)
	} else {
		s.recordCatalogRevisionAudit(audit.ActionModelCatalogApply, r)
	}
	out, err := catalogOAPI[oapi.CatalogRevision](r)
	if err != nil {
		return nil, err
	}
	return oapi.ApplyModelCatalogProposal200JSONResponse(out), nil
}

func (s *Server) ListModelCatalogHistory(_ context.Context, req oapi.ListModelCatalogHistoryRequestObject) (oapi.ListModelCatalogHistoryResponseObject, error) {
	if s.backends == nil {
		return nil, catalogServiceUnavailable()
	}
	snaps, err := s.backends.ListCatalogSnapshots(strings.TrimSpace(req.Params.Aicli), "")
	if err != nil {
		return nil, err
	}
	revs, err := s.backends.ListCatalogRevisions()
	if err != nil {
		return nil, err
	}
	out, err := catalogOAPI[oapi.CatalogHistory](map[string]any{"snapshots": snaps, "revisions": revs})
	if err != nil {
		return nil, err
	}
	return oapi.ListModelCatalogHistory200JSONResponse(out), nil
}

func (s *Server) RollbackModelCatalogRevision(_ context.Context, req oapi.RollbackModelCatalogRevisionRequestObject) (oapi.RollbackModelCatalogRevisionResponseObject, error) {
	if s.backends == nil {
		return nil, catalogServiceUnavailable()
	}
	if req.Revision < 1 {
		return nil, errStatus(http.StatusBadRequest, "revision must be positive")
	}
	prior, _ := s.backends.GetCatalogRevision(uint64(req.Revision))
	r, err := s.backends.RollbackCatalogRevision(uint64(req.Revision), time.Now().UTC())
	if err != nil {
		if errors.Is(err, backendstore.ErrNotFound) {
			return nil, errStatus(http.StatusNotFound, "catalog revision not found")
		}
		if errors.Is(err, backendstore.ErrRevisionConflict) {
			return nil, errStatus(http.StatusConflict, err.Error())
		}
		return nil, err
	}
	if prior.ProposalID != "" {
		if p, getErr := s.backends.GetCatalogProposal(prior.ProposalID); getErr == nil {
			s.recordCatalogAudit(audit.ActionModelCatalogRollback, p, &r)
		} else {
			s.recordCatalogRevisionAudit(audit.ActionModelCatalogRollback, r)
		}
	} else {
		s.recordCatalogRevisionAudit(audit.ActionModelCatalogRollback, r)
	}
	out, err := catalogOAPI[oapi.CatalogRevision](r)
	if err != nil {
		return nil, err
	}
	return oapi.RollbackModelCatalogRevision200JSONResponse(out), nil
}

// recordCatalogAudit keeps the rich, non-secret decision provenance in audit,
// not in metric labels. The target is an opaque proposal/revision id and the
// account scope is already a one-way fingerprint.
func (s *Server) recordCatalogAudit(action string, p backendstore.CatalogProposal, r *backendstore.CatalogRevision) {
	if s == nil || s.audit == nil {
		return
	}
	d := map[string]string{"aicli": p.Scope.AiCli, "scope_fingerprint": p.Scope.ActiveScopeFP, "snapshot_ids": strings.Join(p.Snapshots, ","), "proposal_state": p.State, "ops": strconv.Itoa(len(p.Ops)), "blocked": strconv.Itoa(len(p.Blocked))}
	if r != nil {
		d["revision"] = strconv.FormatUint(r.Rev, 10)
		d["parent_revision"] = strconv.FormatUint(r.ParentRev, 10)
	}
	for i, op := range p.Ops {
		if i == 20 {
			d["operations_truncated"] = "true"
			break
		}
		d["op_"+strconv.Itoa(i)] = op.Op + ":" + op.Key + ":" + strings.Join(op.Reasons, "+")
	}
	s.audit.Log(audit.Event{Action: action, Target: p.ID, Detail: audit.SafeDetail(d)})
}

func (s *Server) recordCatalogRevisionAudit(action string, r backendstore.CatalogRevision) {
	p := backendstore.CatalogProposal{ID: r.ProposalID, State: r.Kind, Snapshots: r.Snapshots, Ops: r.Ops}
	if len(r.Ops) > 0 && r.Ops[0].After != nil {
		p.Scope.AiCli = r.Ops[0].After.BackendID
	}
	s.recordCatalogAudit(action, p, &r)
}

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

// AddModel implements POST /api/v1/models: register a custom model in the catalog.
func (s *Server) AddModel(_ context.Context, req oapi.AddModelRequestObject) (oapi.AddModelResponseObject, error) {
	if s.backends == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "backend registry not configured")
	}
	if req.Body == nil {
		return nil, errStatus(http.StatusBadRequest, "request body is required")
	}
	b := req.Body
	backendID, modelID := strings.TrimSpace(b.BackendId), strings.TrimSpace(b.ModelId)
	if backendID == "" || modelID == "" {
		return nil, errStatus(http.StatusBadRequest, "backend_id and model_id are required")
	}
	tier := backendstore.ModelTier(strings.TrimSpace(b.Tier))
	if !tier.Valid() {
		return nil, errStatus(http.StatusBadRequest, fmt.Sprintf("invalid tier %q (valid: tier-1, tier-2, tier-3)", b.Tier))
	}
	if err := s.backends.AddModel(backendID, modelID, b.DisplayName, tier, b.AutoAssign, b.QuotaScope); err != nil {
		if errors.Is(err, backendstore.ErrExists) {
			return nil, errStatus(http.StatusConflict, fmt.Sprintf("model %s/%s already exists in the catalog", backendID, modelID))
		}
		if errors.Is(err, backendstore.ErrInvalidTier) {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
		return nil, err
	}
	m, err := s.backends.GetModel(backendID, modelID)
	if err != nil {
		return nil, err
	}
	return oapi.AddModel201JSONResponse(m), nil
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
	if errors.Is(err, agentstore.ErrNotFound) {
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
		aiCli := strings.TrimSpace(req.Body.AiCli)
		if aiCli == "" {
			aiCli = strings.TrimSpace(req.Body.Backend)
		}
		swapReq.Backend = aiCli
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
		if errors.Is(err, lifecycle.ErrAmbiguousTranscript) {
			return nil, errStatus(http.StatusConflict, "hot-swap refused: "+err.Error())
		}
		return nil, errStatus(http.StatusInternalServerError, "hot-swap failed: "+err.Error())
	}
	// Handoff prose is attributed-note only — never a PlanExecutionEvent / summary.
	if res != nil {
		s.recordPlanBoundHandoffNote(sess, res.Handoff, res.HandoffPath)
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
