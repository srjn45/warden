package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/store"
)

// approvalsResponse is the body for GET /approvals.
type approvalsResponse struct {
	Enabled   bool            `json:"enabled"`
	Approvals []approval.View `json:"approvals"`
}

// ApproveRequest is the body for POST /sessions/{id}/approve.
type ApproveRequest struct {
	Option      int    `json:"option"`      // 1-based choice
	Fingerprint string `json:"fingerprint"` // the options hash the UI rendered
}

// handleApprove answers a recognized prompt with a re-verify guard: it
// re-captures the pane fresh, re-parses, and injects the digit ONLY if the
// fingerprint still matches — otherwise 409, so we never answer a prompt that
// changed underneath the user.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	if !s.approvals {
		writeErr(w, http.StatusForbidden, "approvals disabled")
		return
	}
	id := chi.URLParam(r, "id")
	sess, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "session not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	pane, err := s.life.Output(r.Context(), sess.TmuxSession, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a, ok := approval.Parse(pane)
	if !ok || approval.Fingerprint(a.Options) != req.Fingerprint {
		writeErr(w, http.StatusConflict, "prompt changed; reopen")
		return
	}
	if req.Option < 1 || req.Option > len(a.Options) {
		writeErr(w, http.StatusBadRequest, "option out of range")
		return
	}
	if err := s.life.SendKeys(r.Context(), sess.TmuxSession, strconv.Itoa(req.Option)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notify()
	writeJSON(w, http.StatusOK, map[string]string{"status": "answered"})
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
