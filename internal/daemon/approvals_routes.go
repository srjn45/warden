package daemon

import (
	"net/http"

	"github.com/srajanpathak/agentctl/internal/approval"
	"github.com/srajanpathak/agentctl/internal/store"
)

// approvalsResponse is the body for GET /approvals.
type approvalsResponse struct {
	Enabled   bool            `json:"enabled"`
	Approvals []approval.View `json:"approvals"`
}

// handleApprovals returns the live queue: every waiting_for_input session parsed
// from its stored pane excerpt (recognized options or the unrecognized flag).
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if !s.approvals {
		writeJSON(w, http.StatusOK, approvalsResponse{Enabled: false, Approvals: []approval.View{}})
		return
	}
	sessions, err := s.store.List(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := []approval.View{}
	for _, sess := range sessions {
		if sess.Status != store.StatusWaitingForInput {
			continue
		}
		views = append(views, approval.BuildView(sess.ID, sess.LastPaneExcerpt))
	}
	writeJSON(w, http.StatusOK, approvalsResponse{Enabled: true, Approvals: views})
}
