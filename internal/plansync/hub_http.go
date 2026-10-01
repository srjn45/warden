package plansync

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/srjn45/warden/internal/planstore"
)

// HTTPService implements the frozen Phase B Hub HTTP contract. Authentication
// and authorization are intentionally supplied by the host router; Warden's
// daemon mounts this handler inside its Bearer-authenticated API group.
type HTTPService struct{ Store HubStore }

// Push serves POST /api/v1/plan-sync/push.
func (h HTTPService) Push(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeHubError(w, http.StatusServiceUnavailable, hubErrorBody{Error: "plan sync hub not configured"})
		return
	}
	var env Envelope
	if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
		writeHubError(w, http.StatusBadRequest, hubErrorBody{Error: "invalid JSON request"})
		return
	}
	out, err := h.Store.Push(r.Context(), env)
	if err != nil {
		var conflict *ConflictError
		if errors.As(err, &conflict) {
			writeHubError(w, http.StatusConflict, hubErrorBody{Error: "conflict", PlanID: conflict.PlanID, Expected: conflict.Expected, Actual: conflict.Actual})
			return
		}
		writeHubError(w, http.StatusBadRequest, hubErrorBody{Error: err.Error()})
		return
	}
	writeHubJSON(w, http.StatusOK, out)
}

// Pull serves POST /api/v1/plan-sync/pull.
func (h HTTPService) Pull(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeHubError(w, http.StatusServiceUnavailable, hubErrorBody{Error: "plan sync hub not configured"})
		return
	}
	var req struct {
		Scope    Scope                  `json:"scope"`
		PlanID   string                 `json:"plan_id,omitempty"`
		Statuses []planstore.PlanStatus `json:"statuses,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHubError(w, http.StatusBadRequest, hubErrorBody{Error: "invalid JSON request"})
		return
	}
	envs, err := h.Store.Pull(r.Context(), PullQuery{Scope: req.Scope, PlanID: req.PlanID, Statuses: req.Statuses})
	if err != nil {
		writeHubError(w, http.StatusInternalServerError, hubErrorBody{Error: "plan sync pull failed", Message: err.Error()})
		return
	}
	writeHubJSON(w, http.StatusOK, envelopesResponse{Envelopes: envs})
}

// Discover serves POST /api/v1/plan-sync/discover. With no status filter it
// returns pending and in_progress envelopes, per the frozen protocol.
func (h HTTPService) Discover(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeHubError(w, http.StatusServiceUnavailable, hubErrorBody{Error: "plan sync hub not configured"})
		return
	}
	var req struct {
		Scope    Scope                  `json:"scope"`
		Statuses []planstore.PlanStatus `json:"statuses,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeHubError(w, http.StatusBadRequest, hubErrorBody{Error: "invalid JSON request"})
		return
	}
	envs, err := DiscoverHub(r.Context(), h.Store, req.Scope, req.Statuses)
	if err != nil {
		writeHubError(w, http.StatusInternalServerError, hubErrorBody{Error: "plan sync discover failed", Message: err.Error()})
		return
	}
	writeHubJSON(w, http.StatusOK, envelopesResponse{Envelopes: envs})
}

func writeHubJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeHubError(w http.ResponseWriter, status int, body hubErrorBody) {
	writeHubJSON(w, status, body)
}
