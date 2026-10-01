package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/planexport"
	"github.com/srjn45/warden/internal/planstore"
)

// SetPlanExportStore wires the per-repository plan-export record store used by
// sync_to_repo. A nil store leaves SyncPlanToRepo unconfigured (503).
func (s *Server) SetPlanExportStore(store planexport.RecordStore) {
	s.planExports = store
}

// SetPlanSyncGitHost injects the Git/GitHub seam for sync_to_repo (tests).
// Nil ⇒ RunnerGitHost with ExecRunner.
func (s *Server) SetPlanSyncGitHost(h planexport.GitHost) {
	s.planSyncGit = h
}

func (s *Server) planSyncer() *planexport.Syncer {
	if s.plans == nil || s.planExports == nil {
		return nil
	}
	git := s.planSyncGit
	if git == nil {
		git = planexport.NewRunnerGitHost(lifecycle.ExecRunner{})
	}
	return &planexport.Syncer{
		Plans:    s.plans,
		Exports:  s.planExports,
		PlansMut: s.plans,
		Git:      git,
		// Renderer left nil so SyncOptions.Format selects via planexport.New.
	}
}

// SyncPlanToRepo implements POST /api/v1/plans/{plan_id}/sync_to_repo.
func (s *Server) SyncPlanToRepo(ctx context.Context, req oapi.SyncPlanToRepoRequestObject) (oapi.SyncPlanToRepoResponseObject, error) {
	syncer := s.planSyncer()
	if syncer == nil {
		return oapi.SyncPlanToRepo503JSONResponse{Error: "plan export store not configured"}, nil
	}
	if req.Body == nil {
		return oapi.SyncPlanToRepo400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "request body required"}}, nil
	}
	targetRef := strings.TrimSpace(req.Body.TargetRef)
	if targetRef == "" {
		return oapi.SyncPlanToRepo400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "target_ref is required"}}, nil
	}

	plan, err := s.plans.Get(ctx, req.PlanId)
	if err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.SyncPlanToRepo404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		return nil, err
	}

	repoPath := strings.TrimSpace(req.Body.RepositoryPath)
	if repoPath == "" {
		repoPath = s.resolvePlanRoot(plan.ProjectID)
	}
	if repoPath == "" {
		return oapi.SyncPlanToRepo400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "repository_path could not be resolved"}}, nil
	}

	format := planexport.Format(strings.TrimSpace(string(req.Body.Format)))
	if format == "" {
		format = planexport.FormatYAML
	}
	if _, ferr := planexport.ParseFormat(string(format)); ferr != nil {
		return oapi.SyncPlanToRepo400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: ferr.Error()}}, nil
	}

	res, err := syncer.Sync(ctx, planexport.SyncOptions{
		PlanID:     req.PlanId,
		RepoPath:   repoPath,
		TargetRef:  targetRef,
		Format:     format,
		OutputPath: strings.TrimSpace(req.Body.OutputPath),
		Repository: strings.TrimSpace(req.Body.Repository),
	})
	if res == nil && err != nil {
		if errors.Is(err, planstore.ErrNotFound) {
			return oapi.SyncPlanToRepo404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "plan not found"}}, nil
		}
		msg := err.Error()
		if strings.Contains(msg, "required") || strings.Contains(msg, "invalid") {
			return oapi.SyncPlanToRepo400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		return nil, err
	}
	out := syncResultToOAPI(res)
	if err != nil {
		switch {
		case errors.Is(err, planexport.ErrSyncConflict):
			return oapi.SyncPlanToRepo409JSONResponse(out), nil
		case errors.Is(err, planexport.ErrGitHubAuth):
			return oapi.SyncPlanToRepo503JSONResponse{Error: res.ErrorMessage}, nil
		default:
			if res.Outcome == planexport.OutcomeFailed {
				return oapi.SyncPlanToRepo503JSONResponse{Error: firstNonEmpty(res.ErrorMessage, err.Error())}, nil
			}
			return nil, err
		}
	}
	return oapi.SyncPlanToRepo200JSONResponse(out), nil
}

func syncResultToOAPI(res *planexport.SyncResult) oapi.PlanSyncToRepoResult {
	if res == nil {
		return oapi.PlanSyncToRepoResult{}
	}
	return oapi.PlanSyncToRepoResult{
		PlanId:       res.PlanID,
		Revision:     res.Revision,
		ContentHash:  res.ContentHash,
		Repository:   res.Repository,
		TargetRef:    res.TargetRef,
		OutputPath:   res.OutputPath,
		Branch:       res.Branch,
		CommitSha:    res.CommitSHA,
		PrUrl:        res.PRURL,
		PrCreated:    res.PRCreated,
		Outcome:      oapi.PlanSyncToRepoResultOutcome(res.Outcome),
		Reason:       res.Reason,
		ErrorMessage: res.ErrorMessage,
		Reused:       res.Reused,
		RecordId:     res.RecordID,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
