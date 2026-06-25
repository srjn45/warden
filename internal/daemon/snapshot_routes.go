package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/srjn45/warden/internal/snapshot"
)

// snapshotsDisabledMsg mirrors approvalsDisabledMsg: a friendly hint when the
// feature gate is off rather than a bare 403.
const snapshotsDisabledMsg = "snapshots disabled (enable with snapshots: true in the config file)"

// registerSnapshotRoutes wires the checkpoint endpoints (#46). create + list are
// worktree/session-pinned like the git routes; restore is keyed by snapshot id
// (which carries its own recorded worktree).
func (s *Server) registerSnapshotRoutes(r chi.Router) {
	r.Post("/snapshots", s.handleSnapshotCreate)
	r.Get("/snapshots", s.handleSnapshotList)
	r.Post("/snapshots/{id}/restore", s.handleSnapshotRestore)
}

// snapshotsReady reports whether the feature is enabled AND configured, writing
// the disabled response and returning false otherwise.
func (s *Server) snapshotsReady(w http.ResponseWriter) bool {
	if !s.snapshots || s.snap == nil {
		writeErr(w, http.StatusForbidden, snapshotsDisabledMsg)
		return false
	}
	return true
}

// handleSnapshotCreate captures the calling agent's worktree + transcript. It
// reuses resolveGitTarget so the worktree is pinned to the agent's own Workdir
// (an agent cannot snapshot a sibling worktree by passing a different dir), and
// records the action against the agent.
func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	if !s.snapshotsReady(w) {
		return
	}
	dir, sess, req, ok := s.resolveGitTarget(w, r)
	if !ok {
		return
	}
	cr := snapshot.CaptureRequest{
		SessionID: req.Session, // raw id so list-by-session works even for an unknown/human session
		Workdir:   dir,
		Message:   req.Message,
	}
	if sess != nil {
		cr.SessionID = sess.ID
		cr.TmuxSession = sess.TmuxSession
	}
	snap, err := s.snap.Capture(r.Context(), cr)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if sess != nil {
		s.recordGitEvent(sess.ID, "snapshot", "captured "+snap.ID)
	}
	writeJSON(w, http.StatusOK, snap)
}

// handleSnapshotList returns snapshots newest-first, optionally filtered to one
// session via the `session` query param ("" = all).
func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	if !s.snapshotsReady(w) {
		return
	}
	snaps, err := s.snap.List(r.Context(), r.URL.Query().Get("session"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if snaps == nil {
		snaps = []*snapshot.Snapshot{}
	}
	writeJSON(w, http.StatusOK, snapshotListResponse{Snapshots: snaps})
}

type snapshotListResponse struct {
	Snapshots []*snapshot.Snapshot `json:"snapshots"`
}

type snapshotRestoreRequest struct {
	Force bool `json:"force,omitempty"`
}

// handleSnapshotRestore re-applies a snapshot onto its recorded worktree. A
// dirty-tree refusal (without force) maps to 409 with the "use --force" hint; a
// missing snapshot to 404.
func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	if !s.snapshotsReady(w) {
		return
	}
	id := chi.URLParam(r, "id")
	var req snapshotRestoreRequest
	// An empty body is fine (force defaults false); ignore a decode miss.
	_ = json.NewDecoder(r.Body).Decode(&req)
	res, err := s.snap.Restore(r.Context(), id, req.Force)
	if errors.Is(err, snapshot.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "snapshot not found: "+id)
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
