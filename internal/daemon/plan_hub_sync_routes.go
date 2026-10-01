package daemon

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/plansync"
)

// handlePlanSyncPush is the explicit operator action that offers one local
// canonical revision to the configured Hub. It intentionally has no scheduler
// or startup side effect.
func (s *Server) handlePlanSyncPush(w http.ResponseWriter, r *http.Request) {
	if s.plans == nil {
		writeErr(w, http.StatusServiceUnavailable, "plan store not configured")
		return
	}
	var req struct {
		PlanID     string              `json:"plan_id"`
		Scope      plansync.Scope      `json:"scope"`
		Visibility plansync.Visibility `json:"visibility"`
		OwnerID    string              `json:"owner_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	p, err := s.plans.Get(r.Context(), strings.TrimSpace(req.PlanID))
	if err != nil {
		writeErr(w, http.StatusNotFound, "plan not found")
		return
	}
	provider := s.planSync
	if provider == nil {
		provider = plansync.Default()
	}
	if hub, ok := provider.(*plansync.HubProvider); ok {
		err = hub.SyncPushPlan(r.Context(), p, plansync.EnvelopeOptions{Scope: req.Scope, Visibility: req.Visibility, OwnerID: req.OwnerID})
		if err == nil {
			err = s.plans.Update(r.Context(), p.ID, func(stored *planstore.Plan) error { plansync.StampPlan(stored, p.RemoteID, *p.SyncedAt); return nil })
		}
	} else {
		var env plansync.Envelope
		env, err = plansync.EnvelopeFromPlan(p, plansync.EnvelopeOptions{Scope: req.Scope, Visibility: req.Visibility, OwnerID: req.OwnerID})
		if err == nil {
			err = provider.Push(r.Context(), env)
		}
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	updated, _ := s.plans.Get(r.Context(), p.ID)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handlePlanSyncPull(w http.ResponseWriter, r *http.Request) {
	s.handlePlanSyncQuery(w, r, false)
}

func (s *Server) handlePlanSyncDiscover(w http.ResponseWriter, r *http.Request) {
	s.handlePlanSyncQuery(w, r, true)
}

func (s *Server) handlePlanSyncQuery(w http.ResponseWriter, r *http.Request, discover bool) {
	var req struct {
		Scope    plansync.Scope         `json:"scope"`
		PlanID   string                 `json:"plan_id,omitempty"`
		Statuses []planstore.PlanStatus `json:"statuses,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON request")
		return
	}
	provider := s.planSync
	if provider == nil {
		provider = plansync.Default()
	}
	var (
		envs []plansync.Envelope
		err  error
	)
	if discover {
		envs, err = provider.Discover(r.Context(), req.Scope, req.Statuses)
	} else {
		envs, err = provider.Pull(r.Context(), plansync.PullQuery{Scope: req.Scope, PlanID: req.PlanID, Statuses: req.Statuses})
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// Only a successful Hub response may stamp a local record. Pull/discover do
	// not import or overwrite definitions; Hub remains a discovery transport.
	if provider.Name() == plansync.ProviderHub && s.plans != nil {
		for _, env := range envs {
			if env.RemoteID != "" && env.SyncedAt != nil {
				_ = s.plans.Update(r.Context(), env.PlanID, func(p *planstore.Plan) error { plansync.StampPlanFromEnvelope(p, env); return nil })
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"envelopes": envs})
}
