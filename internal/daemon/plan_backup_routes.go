package daemon

import (
	"context"
	"errors"
	"strings"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/planbackup"
	"github.com/srjn45/warden/internal/planstore"
)

// ExportPlanBackup implements POST /api/v1/plans/export_backup.
func (s *Server) ExportPlanBackup(ctx context.Context, req oapi.ExportPlanBackupRequestObject) (oapi.ExportPlanBackupResponseObject, error) {
	if s.plans == nil {
		return oapi.ExportPlanBackup503JSONResponse{Error: "plan store not configured"}, nil
	}
	if req.Body == nil {
		return oapi.ExportPlanBackup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "request body required"}}, nil
	}
	opts := planbackup.ExportOptions{
		ProjectID: strings.TrimSpace(req.Body.ProjectId),
		All:       req.Body.All,
		PlanIDs:   append([]string(nil), req.Body.PlanIds...),
	}
	exporter := &planbackup.Exporter{Store: s.plans}
	bundle, err := exporter.Export(ctx, opts)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, planstore.ErrNotFound) || strings.Contains(msg, "get ") {
			return oapi.ExportPlanBackup404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: msg}}, nil
		}
		if strings.Contains(msg, "no plans selected") || strings.Contains(msg, "specify plan ids") {
			return oapi.ExportPlanBackup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		return nil, err
	}
	return oapi.ExportPlanBackup200JSONResponse(*bundle), nil
}

// RestorePlanBackup implements POST /api/v1/plans/restore_backup.
func (s *Server) RestorePlanBackup(ctx context.Context, req oapi.RestorePlanBackupRequestObject) (oapi.RestorePlanBackupResponseObject, error) {
	if s.plans == nil {
		return oapi.RestorePlanBackup503JSONResponse{Error: "plan store not configured"}, nil
	}
	if req.Body == nil {
		return oapi.RestorePlanBackup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "request body required"}}, nil
	}
	opts := planbackup.RestoreOptions{
		DryRun:     req.Body.DryRun,
		OnConflict: planbackup.ConflictPolicy(req.Body.OnConflict),
	}
	restorer := &planbackup.Restorer{Store: s.plans}
	res, err := restorer.Restore(ctx, &req.Body.Bundle, opts)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "conflict on plan") && res != nil {
			return oapi.RestorePlanBackup409JSONResponse(*res), nil
		}
		if strings.Contains(msg, "unsupported schema") ||
			strings.Contains(msg, "hash mismatch") ||
			strings.Contains(msg, "nil bundle") ||
			strings.Contains(msg, "no entries") ||
			strings.Contains(msg, "unknown on_conflict") ||
			strings.Contains(msg, "missing plan") {
			return oapi.RestorePlanBackup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: msg}}, nil
		}
		if res != nil && strings.Contains(msg, "conflict") {
			return oapi.RestorePlanBackup409JSONResponse(*res), nil
		}
		return nil, err
	}
	return oapi.RestorePlanBackup200JSONResponse(*res), nil
}
