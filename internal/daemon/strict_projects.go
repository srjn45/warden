package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/projectstore"
)

// ListProjects implements GET /api/v1/projects: every persisted project
// (docs/specs/2026-08-28-project-centric-ui.md Phase 1), sorted by display name.
// Read-only. Closed (hibernated) projects are included with status "closed" so a
// client can filter them.
func (s *Server) ListProjects(_ context.Context, _ oapi.ListProjectsRequestObject) (oapi.ListProjectsResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	list, err := s.projects.List()
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "list projects: "+err.Error())
	}
	if list == nil {
		list = []projectstore.Project{}
	}
	return oapi.ListProjects200JSONResponse{Projects: list}, nil
}

// OpenProject implements POST /api/v1/projects/open: register a project by its
// canonical id and mark it open (reopening a closed one flips it back). Phase 1
// only persists the record — the git clone/init mechanics are Phase 2. An empty
// name/path leaves the stored value unchanged.
func (s *Server) OpenProject(_ context.Context, req oapi.OpenProjectRequestObject) (oapi.OpenProjectResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	if req.Body == nil {
		return oapi.OpenProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "id is required"}}, nil
	}
	id := strings.TrimSpace(req.Body.Id)
	if id == "" {
		return oapi.OpenProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "id is required"}}, nil
	}
	p, err := s.projects.OpenProject(id, strings.TrimSpace(req.Body.Name), strings.TrimSpace(req.Body.Path))
	if err != nil {
		if errors.Is(err, projectstore.ErrInvalidID) {
			return oapi.OpenProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "open project: "+err.Error())
	}
	return oapi.OpenProject200JSONResponse(p), nil
}

// CloseProject implements POST /api/v1/projects/{id}/close: hibernate a project
// (IDE-like) — the record is kept, only its status flips to closed. Returns 404
// if the id is unknown.
func (s *Server) CloseProject(_ context.Context, req oapi.CloseProjectRequestObject) (oapi.CloseProjectResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	p, err := s.projects.CloseProject(strings.TrimSpace(req.Id))
	if err != nil {
		if errors.Is(err, projectstore.ErrNotFound) {
			return oapi.CloseProject404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "close project: "+err.Error())
	}
	return oapi.CloseProject200JSONResponse(p), nil
}
