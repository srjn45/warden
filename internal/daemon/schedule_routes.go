package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/schedule"
)

// schedulerDisabledMsg mirrors snapshotsDisabledMsg: a friendly hint when the
// feature gate is off rather than a bare 403.
const schedulerDisabledMsg = "scheduler disabled (enable with scheduler_enabled: true in the config file)"

type createScheduleRequest struct {
	Name   string `json:"name"`
	Cron   string `json:"cron,omitempty"`
	At     string `json:"at,omitempty"`
	Type   string `json:"type,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Branch string `json:"branch,omitempty"`
	Spec   string `json:"spec,omitempty"` // pipeline YAML; non-empty ⇒ pipeline mode
}

type schedulesResponse struct {
	Schedules []*schedule.Schedule `json:"schedules"`
}

func (s *Server) registerScheduleRoutes(r chi.Router) {
	r.Post("/schedules", s.handleCreateSchedule)
	r.Get("/schedules", s.handleListSchedules)
	r.Delete("/schedules/{id}", s.handleDeleteSchedule)
}

// schedulerReady reports whether the feature is enabled AND configured, writing
// the disabled 403 and returning false otherwise.
func (s *Server) schedulerReady(w http.ResponseWriter) bool {
	if !s.scheduler || s.schedStore == nil {
		writeErr(w, http.StatusForbidden, schedulerDisabledMsg)
		return false
	}
	return true
}

func (s *Server) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.schedulerReady(w) {
		return
	}
	var req createScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// A pipeline schedule carries a YAML spec — validate it here (with the pipeline
	// package) so a malformed spec is rejected at create time, not silently on the
	// first fire. The schedule package stays dependency-light and only stores it.
	if req.Spec != "" {
		if _, err := pipeline.ParseSpec([]byte(req.Spec)); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid pipeline spec: "+err.Error())
			return
		}
	}
	sc, err := schedule.New(schedule.Params{
		Name:   req.Name,
		Cron:   req.Cron,
		At:     req.At,
		Type:   req.Type,
		Repo:   req.Repo,
		Prompt: req.Prompt,
		Agent:  req.Agent,
		Branch: req.Branch,
		Spec:   req.Spec,
	}, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.schedStore.Create(sc); errors.Is(err, schedule.ErrExists) {
		writeErr(w, http.StatusConflict, "schedule "+sc.ID+" already exists")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.recordAudit(r, audit.ActionScheduleCreate, sc.ID, map[string]string{"kind": string(sc.Kind), "mode": string(sc.Mode)})
	writeJSON(w, http.StatusCreated, sc)
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	if !s.schedulerReady(w) {
		return
	}
	list, err := s.schedStore.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []*schedule.Schedule{}
	}
	writeJSON(w, http.StatusOK, schedulesResponse{Schedules: list})
}

func (s *Server) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.schedulerReady(w) {
		return
	}
	id := chi.URLParam(r, "id")
	if err := s.schedStore.Delete(id); errors.Is(err, schedule.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "schedule not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.recordAudit(r, audit.ActionScheduleDelete, id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// fireSchedule performs the side effect a due schedule decides: either one agent
// spawn or one pipeline create+start. It reuses the SAME internal seams the HTTP
// handlers use (life.Spawn + store.Insert, or pipeline.ParseSpec + pstore.Create
// + exec.Reconcile) rather than shelling out to the CLI. It is fail-soft: the
// returned error is recorded in the schedule's LastError by the caller and never
// crashes the reconcile loop.
func (s *Server) fireSchedule(ctx context.Context, sc *schedule.Schedule) error {
	switch sc.Mode {
	case schedule.ModePipeline:
		return s.fireSchedulePipeline(ctx, sc)
	default:
		return s.fireScheduleAgent(ctx, sc)
	}
}

// fireScheduleAgent spawns one agent from the schedule's payload, mirroring
// handleSpawn's spawn → insert → rollback-on-insert-failure flow. The agent name
// is passed through as given; a collision with an existing agent fails this fire
// (recorded in LastError) rather than silently renaming — honest over clever.
func (s *Server) fireScheduleAgent(ctx context.Context, sc *schedule.Schedule) error {
	req := SpawnRequest{
		Type:   sc.Type,
		Name:   sc.Agent,
		Repo:   sc.Repo,
		Branch: sc.Branch,
		Prompt: sc.Prompt,
	}
	if code, msg := s.validateSpawnRequest(ctx, req); code != 0 {
		return errors.New(msg)
	}
	sess, err := s.life.Spawn(ctx, req)
	if err != nil {
		return err
	}
	if err := s.store.Insert(ctx, sess); err != nil {
		// Roll back the tmux session (and any worktree) so a failed insert doesn't
		// leak an untracked agent — same guard handleSpawn applies.
		tctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if terr := s.life.Teardown(tctx, sess); terr != nil {
			return errors.New(err.Error() + " (rollback also failed: " + terr.Error() + ")")
		}
		return err
	}
	s.notify()
	return nil
}

// fireSchedulePipeline creates and starts a pipeline from the schedule's stored
// YAML spec. A recurring schedule fires repeatedly, but a pipeline's id == its
// name, so the spec's name is suffixed with a timestamp to keep each fire a
// distinct record (otherwise the second fire would collide on ErrExists). It then
// reconciles on a daemon-owned context (spawned jobs outlive this tick).
func (s *Server) fireSchedulePipeline(ctx context.Context, sc *schedule.Schedule) error {
	if s.exec == nil {
		return errors.New("pipeline execution is not configured")
	}
	p, err := pipeline.ParseSpec([]byte(sc.Spec))
	if err != nil {
		return err
	}
	// Uniquify so repeated fires don't collide. ParseSpec set ID == Name.
	suffix := "-" + time.Now().Format("20060102-150405")
	p.Name += suffix
	p.ID = p.Name
	p.Status = pipeline.StatusRunning
	if err := s.exec.pstore.Create(p); err != nil {
		return err
	}
	// Reconcile on a background context: spawning worktree jobs can outlast this
	// reconcile tick (mirrors handleStartPipeline).
	if err := s.exec.Reconcile(context.Background(), p.ID); err != nil {
		return err
	}
	return nil
}
