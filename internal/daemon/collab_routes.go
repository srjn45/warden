package daemon

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/collab"
)

// conflictsResponse is the body for GET /collab/conflicts.
type conflictsResponse struct {
	Conflicts []collab.Conflict `json:"conflicts"`
}

func (s *Server) registerCollabRoutes(r chi.Router) {
	r.Get("/collab/conflicts", s.handleCollabConflicts)
}

// handleCollabConflicts returns the current set of files edited by two or more
// agents. Read-only; recomputed on each request. who-is-editing is served by
// filtering this list client-side, so the daemon never accepts a filesystem
// path (no path-injection surface).
func (s *Server) handleCollabConflicts(w http.ResponseWriter, r *http.Request) {
	if s.collab == nil {
		writeJSON(w, http.StatusOK, conflictsResponse{Conflicts: []collab.Conflict{}})
		return
	}
	conflicts, err := s.collab.Conflicts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if conflicts == nil {
		conflicts = []collab.Conflict{}
	}
	writeJSON(w, http.StatusOK, conflictsResponse{Conflicts: conflicts})
}
