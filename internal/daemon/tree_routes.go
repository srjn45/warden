package daemon

import (
	"context"
	"log/slog"

	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/tree"
)

// treeService is the pure hierarchy builder. It is stateless (no I/O, no store
// access), so one shared instance serves both GET /api/v1/tree and the named
// `tree` SSE event.
var treeService = tree.NewService()

// treeInputsFor assembles the non-session snapshots (projects, pipelines,
// autopilot status) the pure tree.Service needs, around an already-resolved,
// already-?all-filtered session list. The caller owns the session scan (it is
// the backbone: a degraded scan must become a 503, never a partial tree), so
// this helper never lists sessions itself and is reused by the HTTP handler and
// the SSE stream alike.
//
// Subsystem failures are non-fatal (spec §12): a pipeline-store read error marks
// the pipeline subtree degraded in place via Inputs.PipelinesDegraded rather than
// failing the whole tree; a nil project store or executor simply contributes no
// registered projects / pipelines (the membership fallback still groups sessions
// by resolved dir). autopilot.Status() is called exactly once here so callers can
// reuse the result (the SSE sessions frame reuses it too — see handleEventsStream).
func (s *Server) treeInputsFor(sessions []*store.Session) tree.Inputs {
	in := tree.Inputs{Sessions: sessions}

	if s.projects != nil {
		if list, err := s.projects.List(); err != nil {
			// A project-store read failure isn't fatal: the tree still builds from
			// sessions (grouped by resolved dir). Log and continue with no registered
			// projects rather than blanking the whole rail.
			slog.Warn("tree: project store read failed; building without registered projects", "error", err)
		} else {
			in.Projects = list
		}
	}

	if s.exec != nil {
		if ps, err := s.exec.pstore.List(); err != nil {
			// Mark the pipeline subtree degraded in place (spec §12) instead of
			// failing the tree — sessions and autopilot are still trustworthy.
			in.PipelinesDegraded = true
			slog.Warn("tree: pipeline store read failed; marking pipeline subtree degraded", "error", err)
		} else {
			in.Pipelines = ps
		}
	}

	if s.autopilot != nil {
		in.Autopilot = s.autopilot.Status()
	}

	return in
}

// GetTree implements GET /api/v1/tree. It gathers the same snapshots the daemon
// serves at /sessions, /projects, /pipelines and the autopilot status, and calls
// the pure tree.Service to compute the typed hierarchy.
//
// The session scan is the backbone: a degraded active scan is a 503 (same
// complete-or-error contract as GET /sessions), never a tree built from a partial
// fleet. An unknown ?project_id= is a 200 with empty roots (the service handles
// the scoping). A read route — ScopeReadOnly suffices (enforced by the auth
// middleware, which treats every non-attach GET as a read). The frame is a live
// snapshot, so it is served Cache-Control: no-store.
func (s *Server) GetTree(ctx context.Context, req oapi.GetTreeRequestObject) (oapi.GetTreeResponseObject, error) {
	sessions, err := s.store.List(ctx)
	if err != nil {
		if d, ok := store.IsDegraded(err); ok {
			logStoreDegraded(d)
			return oapi.GetTree503JSONResponse{Error: d.Error()}, nil
		}
		return nil, err
	}

	visible := make([]*store.Session, 0, len(sessions))
	for _, ss := range sessions {
		if !req.Params.All && ss.HasTag("system:true") {
			continue
		}
		visible = append(visible, ss)
	}

	t := treeService.Build(s.treeInputsFor(visible), req.Params.ProjectId)
	return oapi.GetTree200JSONResponse{
		Body:    *t,
		Headers: oapi.GetTree200ResponseHeaders{CacheControl: "no-store"},
	}, nil
}
