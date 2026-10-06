package daemon

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/daemon/oapi"
)

// autopilotDisabledMsg is returned when the feature is unconfigured (no
// Controller wired). The daemon builds one from config at startup, so this only
// happens on a bare Server literal (some tests).
const autopilotDisabledMsg = "autopilot is not configured"

// GetAutopilot implements GET /api/v1/autopilot: the per-run
// status (autopilot.md §5). Reports disabled/empty when unconfigured.
func (s *Server) GetAutopilot(_ context.Context, _ oapi.GetAutopilotRequestObject) (oapi.GetAutopilotResponseObject, error) {
	if s.autopilot == nil {
		return oapi.GetAutopilot200JSONResponse(autopilot.Status{Runs: []autopilot.RunStatus{}}), nil
	}
	return oapi.GetAutopilot200JSONResponse(s.autopilot.Status()), nil
}

// SetAutopilot implements POST /api/v1/autopilot (DEPRECATED — there is no
// per-repo switch). enabled=true is a no-op; enabled=false pauses every active
// autopilot run in the repo. Start runs with POST /plans/{id}/run.
func (s *Server) SetAutopilot(ctx context.Context, req oapi.SetAutopilotRequestObject) (oapi.SetAutopilotResponseObject, error) {
	if s.autopilot == nil {
		return nil, errStatus(http.StatusForbidden, autopilotDisabledMsg)
	}
	var b oapi.AutopilotToggleRequest
	if req.Body != nil {
		b = *req.Body
	}
	// repo scopes the toggle to one repository (empty ⇒ the daemon's working
	// directory, resolved by the Controller for backward compatibility).
	if b.Enabled {
		st, err := s.autopilot.Enable(ctx, b.Repo)
		if err != nil {
			return nil, err
		}
		s.recordAuditCtx(ctx, audit.ActionAutopilotOn, "", map[string]string{"repo": b.Repo, "runs": strconv.Itoa(len(st.Runs))})
		return oapi.SetAutopilot200JSONResponse(st), nil
	}
	st, paused := s.autopilot.Disable(ctx, b.Repo)
	s.recordAuditCtx(ctx, audit.ActionAutopilotOff, "", map[string]string{"repo": b.Repo, "paused": strconv.Itoa(len(paused))})
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
	if !s.autopilot.CanBrainComplete(runID, caller.ID) {
		return oapi.CompleteAutopilot403JSONResponse{Error: "only the run's active brain may complete it"}, nil
	}
	if s.autopilot.CompletionManaged() {
		// The daemon owns completion (run-to-final-pr §E): the brain's signal only
		// confirms done_when. The run completes when the single final PR is green,
		// and autopilot never merges that PR.
		st, err := s.autopilot.MarkVerified(runID)
		if err != nil {
			return nil, err
		}
		s.recordAuditCtx(ctx, audit.ActionAutopilotComplete, runID, map[string]string{"verified": "true"})
		return oapi.CompleteAutopilot200JSONResponse(st), nil
	}
	st, err := s.autopilot.CompleteRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionAutopilotComplete, runID, nil)

	if params, ok := s.autopilot.LandParams(runID); ok && params.Repo != "" && params.IntegrationBranch != "" && params.DefaultBranch != "" {
		host := s.landHost(params.Repo)
		if prInfo, found, err := host.FindPR(ctx, params.IntegrationBranch); err == nil {
			if !found {
				if dHost, ok := host.(daemonLandHost); ok {
					title := fmt.Sprintf("autopilot: complete %s", runID)
					body := fmt.Sprintf("Autopilot run %s completed successfully.", runID)
					if out, prErr := dHost.runGH(ctx, "pr", "create", "--base", params.DefaultBranch, "--head", params.IntegrationBranch, "--title", title, "--body", body); prErr != nil {
						s.recordAuditCtx(ctx, audit.ActionAutopilotComplete, runID, map[string]string{
							"integration_branch": params.IntegrationBranch,
							"default_branch":     params.DefaultBranch,
							"pr_create_error":    prErr.Error(),
						})
					} else {
						s.recordAuditCtx(ctx, audit.ActionAutopilotComplete, runID, map[string]string{
							"integration_branch": params.IntegrationBranch,
							"default_branch":     params.DefaultBranch,
							"pr_url":             strings.TrimSpace(out),
						})
					}
				}
			} else {
				s.recordAuditCtx(ctx, audit.ActionAutopilotComplete, runID, map[string]string{
					"integration_branch": params.IntegrationBranch,
					"pr":                 strconv.Itoa(prInfo.Number),
				})
			}
		}
	}

	return oapi.CompleteAutopilot200JSONResponse(st), nil
}
