package daemon

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/branchtrack"
)

// branchesResponse is the body for GET /collab/branches.
type branchesResponse struct {
	Branches []branchtrack.BranchStatus `json:"branches"`
}

func (s *Server) registerBranchRoutes(r chi.Router) {
	r.Get("/collab/branches", s.handleBranchStatuses)
}

// handleBranchStatuses returns each tracked agent's CI + branch-vs-main status.
// Read-only; recomputed on each request (mirrors handleCollabConflicts). A nil
// tracker (feature disabled) returns an empty list, not an error.
func (s *Server) handleBranchStatuses(w http.ResponseWriter, r *http.Request) {
	if s.branchTracker == nil {
		writeJSON(w, http.StatusOK, branchesResponse{Branches: []branchtrack.BranchStatus{}})
		return
	}
	statuses, err := s.branchTracker.Statuses(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if statuses == nil {
		statuses = []branchtrack.BranchStatus{}
	}
	writeJSON(w, http.StatusOK, branchesResponse{Branches: statuses})
}
