package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/projectstore"
)

// ListProjectGroups implements GET /api/v1/project-groups: every project group
// (Project Groups feature, Phase 1), sorted by display name. Read-only.
func (s *Server) ListProjectGroups(_ context.Context, _ oapi.ListProjectGroupsRequestObject) (oapi.ListProjectGroupsResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	list, err := s.projects.ListGroups()
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "list project groups: "+err.Error())
	}
	if list == nil {
		list = []projectstore.ProjectGroup{}
	}
	return oapi.ListProjectGroups200JSONResponse{Groups: list}, nil
}

// CreateProjectGroup implements POST /api/v1/project-groups: create a named group
// with an optional initial member set. The name is required; the id is minted by
// the store. Returns the created group.
func (s *Server) CreateProjectGroup(_ context.Context, req oapi.CreateProjectGroupRequestObject) (oapi.CreateProjectGroupResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	if req.Body == nil {
		return oapi.CreateProjectGroup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		return oapi.CreateProjectGroup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	g, err := s.projects.CreateGroup(projectstore.ProjectGroup{Name: name, ProjectIDs: req.Body.ProjectIds})
	if err != nil {
		if errors.Is(err, projectstore.ErrInvalidGroupName) {
			return oapi.CreateProjectGroup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "create project group: "+err.Error())
	}
	return oapi.CreateProjectGroup200JSONResponse(g), nil
}

// GetProjectGroup implements GET /api/v1/project-groups/{id}: one group by id, or
// 404. The id is a store-minted key, but is still percent-decoded for symmetry with
// the other id-in-path routes.
func (s *Server) GetProjectGroup(_ context.Context, req oapi.GetProjectGroupRequestObject) (oapi.GetProjectGroupResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	id := decodeID(req.Id)
	g, err := s.projects.GetGroup(id)
	if err != nil {
		if errors.Is(err, projectstore.ErrGroupNotFound) {
			return oapi.GetProjectGroup404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project group not found"}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "get project group: "+err.Error())
	}
	return oapi.GetProjectGroup200JSONResponse(g), nil
}

// UpdateProjectGroup implements PUT /api/v1/project-groups/{id}: overwrite the
// group's name and membership. Returns the updated group, 404 if the id is unknown,
// or 400 on a blank name.
func (s *Server) UpdateProjectGroup(_ context.Context, req oapi.UpdateProjectGroupRequestObject) (oapi.UpdateProjectGroupResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	id := decodeID(req.Id)
	if req.Body == nil {
		return oapi.UpdateProjectGroup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		return oapi.UpdateProjectGroup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	g, err := s.projects.UpdateGroup(projectstore.ProjectGroup{ID: id, Name: name, ProjectIDs: req.Body.ProjectIds})
	if err != nil {
		switch {
		case errors.Is(err, projectstore.ErrGroupNotFound):
			return oapi.UpdateProjectGroup404JSONResponse{NotFoundJSONResponse: oapi.NotFoundJSONResponse{Error: "project group not found"}}, nil
		case errors.Is(err, projectstore.ErrInvalidGroupName):
			return oapi.UpdateProjectGroup400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
		}
		return nil, errStatus(http.StatusInternalServerError, "update project group: "+err.Error())
	}
	return oapi.UpdateProjectGroup200JSONResponse(g), nil
}

// DeleteProjectGroup implements DELETE /api/v1/project-groups/{id}: remove the
// group (member projects untouched). Idempotent — an unknown id still returns 200.
func (s *Server) DeleteProjectGroup(_ context.Context, req oapi.DeleteProjectGroupRequestObject) (oapi.DeleteProjectGroupResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	if err := s.projects.DeleteGroup(decodeID(req.Id)); err != nil {
		return nil, errStatus(http.StatusInternalServerError, "delete project group: "+err.Error())
	}
	return oapi.DeleteProjectGroup200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "deleted"}}, nil
}

// decodeID percent-decodes an id-in-path segment, falling back to the raw value if
// it is not valid encoding — mirroring the CloseProject handler's PathUnescape.
func decodeID(raw string) string {
	id := strings.TrimSpace(raw)
	if dec, err := url.PathUnescape(id); err == nil {
		id = dec
	}
	return id
}
