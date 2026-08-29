package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// OpenLocalProject implements POST /api/v1/projects/local: open an existing local
// directory as a project (docs/specs/2026-08-28-project-centric-ui.md Phase 2,
// Local). The path is normalized to an absolute, symlink-resolved path used as
// both the canonical id and the stored path; the directory must already exist.
// When name is blank the directory's base name is used.
func (s *Server) OpenLocalProject(_ context.Context, req oapi.OpenLocalProjectRequestObject) (oapi.OpenLocalProjectResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	if req.Body == nil || strings.TrimSpace(req.Body.Path) == "" {
		return oapi.OpenLocalProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "path is required"}}, nil
	}
	abs, err := normalizeDir(strings.TrimSpace(req.Body.Path))
	if err != nil {
		return oapi.OpenLocalProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: err.Error()}}, nil
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return oapi.OpenLocalProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "directory does not exist: " + abs}}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		name = filepath.Base(abs)
	}
	p, err := s.projects.OpenProject(abs, name, abs)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "open local project: "+err.Error())
	}
	return oapi.OpenLocalProject200JSONResponse(p), nil
}

// OpenRemoteProject implements POST /api/v1/projects/remote: clone a remote URL
// into <workspace_path>/<repo-name> and open it as a project (docs/specs/
// 2026-08-28-project-centric-ui.md Phase 2, Remote). The clone destination is the
// canonical id and stored path; a blank name defaults to the repo name derived
// from the URL. Fails if the destination already exists.
func (s *Server) OpenRemoteProject(ctx context.Context, req oapi.OpenRemoteProjectRequestObject) (oapi.OpenRemoteProjectResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	if req.Body == nil {
		return oapi.OpenRemoteProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "url is required"}}, nil
	}
	remote := strings.TrimSpace(req.Body.Url)
	if remote == "" {
		return oapi.OpenRemoteProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "url is required"}}, nil
	}
	repo := repoNameFromURL(remote)
	if repo == "" {
		return oapi.OpenRemoteProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "cannot derive a directory name from url: " + remote}}, nil
	}
	workspace := s.snapshotConfig().WorkspacePath
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, errStatus(http.StatusInternalServerError, "cannot create workspace dir: "+err.Error())
	}
	dest := filepath.Join(workspace, repo)
	if _, err := os.Stat(dest); err == nil {
		return oapi.OpenRemoteProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "destination already exists: " + dest}}, nil
	}
	if out, err := exec.CommandContext(ctx, "git", "clone", remote, dest).CombinedOutput(); err != nil {
		return nil, errStatus(http.StatusInternalServerError, "git clone failed: "+strings.TrimSpace(string(out)))
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		name = repo
	}
	p, err := s.projects.OpenProject(dest, name, dest)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "open remote project: "+err.Error())
	}
	return oapi.OpenRemoteProject200JSONResponse(p), nil
}

// CreateProject implements POST /api/v1/projects/new: scaffold a fresh project in
// <workspace_path>/<name> — git init, a template README.md, then an initial commit
// — and open it (docs/specs/2026-08-28-project-centric-ui.md Phase 2, New). The
// new directory is the canonical id and stored path. Fails if it already exists.
func (s *Server) CreateProject(ctx context.Context, req oapi.CreateProjectRequestObject) (oapi.CreateProjectResponseObject, error) {
	if s.projects == nil {
		return nil, errStatus(http.StatusServiceUnavailable, "project store not configured")
	}
	if req.Body == nil {
		return oapi.CreateProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	name := strings.TrimSpace(req.Body.Name)
	if name == "" {
		return oapi.CreateProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "name is required"}}, nil
	}
	// The name doubles as the workspace directory component: reject anything that
	// escapes a single segment so it can only ever land inside the workspace.
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || name == ".." {
		return oapi.CreateProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "invalid project name: " + name}}, nil
	}
	workspace := s.snapshotConfig().WorkspacePath
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return nil, errStatus(http.StatusInternalServerError, "cannot create workspace dir: "+err.Error())
	}
	dest := filepath.Join(workspace, name)
	if _, err := os.Stat(dest); err == nil {
		return oapi.CreateProject400JSONResponse{BadRequestJSONResponse: oapi.BadRequestJSONResponse{Error: "destination already exists: " + dest}}, nil
	}
	if err := scaffoldProject(ctx, dest, name); err != nil {
		// Best-effort cleanup so a half-scaffolded dir does not block a retry.
		_ = os.RemoveAll(dest)
		return nil, errStatus(http.StatusInternalServerError, "create project: "+err.Error())
	}
	p, err := s.projects.OpenProject(dest, name, dest)
	if err != nil {
		return nil, errStatus(http.StatusInternalServerError, "create project: "+err.Error())
	}
	return oapi.CreateProject200JSONResponse(p), nil
}

// normalizeDir turns a user-supplied path into an absolute, symlink-resolved path
// so the same directory always maps to one canonical project id regardless of how
// it was typed (relative, trailing slash, via a symlink).
func normalizeDir(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

// scaffoldProject creates dest and turns it into a fresh git repo with a template
// README and one initial commit ("chore: project initiated using warden"). It
// supplies a fallback commit identity only when the environment has none, so real
// users' configured git identity is preserved.
func scaffoldProject(ctx context.Context, dest, name string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dest, "init").CombinedOutput(); err != nil {
		return errors.New("git init: " + strings.TrimSpace(string(out)))
	}
	readme := "# " + name + "\n\nProject initiated using warden.\n"
	if err := os.WriteFile(filepath.Join(dest, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "git", "-C", dest, "add", ".").CombinedOutput(); err != nil {
		return errors.New("git add: " + strings.TrimSpace(string(out)))
	}
	args := []string{"-C", dest}
	if !gitIdentityConfigured(ctx, dest) {
		args = append(args, "-c", "user.name=warden", "-c", "user.email=warden@localhost")
	}
	args = append(args, "commit", "-m", "chore: project initiated using warden")
	if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
		return errors.New("git commit: " + strings.TrimSpace(string(out)))
	}
	return nil
}

// gitIdentityConfigured reports whether git can resolve a commit author identity
// in dest (user.email set anywhere it looks). When false the caller injects a
// fallback identity so the initial commit still succeeds in a bare environment.
func gitIdentityConfigured(ctx context.Context, dest string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", dest, "config", "user.email").Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
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
