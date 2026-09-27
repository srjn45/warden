package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/brainconsult"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/store"
)

// defaultAutopilotAllowed is the closed-action subset offered to the autopilot
// manager when the request omits Allowed. Prefer reuse of the D2 enum over
// growth; pipeline-specific actions (retry_job / mark_failed / skip_job) are
// omitted because the manager has no Executor — it nudges / waits / escalates.
var defaultAutopilotAllowed = []brainconsult.Action{
	brainconsult.ActionNudgeAgent,
	brainconsult.ActionWait,
	brainconsult.ActionEscalate,
	brainconsult.ActionNoop,
}

// SetBrainConsultor wires the shared Consultor used by PipelineWatcher and the
// autopilot manager's POST /autopilot/brain-consult entry point (spec D5). A
// nil consultor leaves both paths off. maxCon caps simultaneous pipeline
// consults (PipelineWatcher semaphore); the MCP/HTTP path is manager-serialized.
// Call before ListenAndServe when brain_consult.enabled is true.
func (s *Server) SetBrainConsultor(c brainconsult.Consultor, maxCon int) {
	s.brainConsultor = c
	if s.pipelineWatcher != nil && c != nil {
		s.pipelineWatcher.SetBrainConsult(c, maxCon, s.life)
	}
}

// ConfigureBrainConsult builds and wires the shared Consultor from config.
// When enabled=false the consultor stays nil (feature off). Must be called after
// SetAudit and SetExecutor so audit events and PipelineWatcher are available.
func (s *Server) ConfigureBrainConsult(cfg config.BrainConsultConfig) {
	if !cfg.Enabled {
		s.brainConsultor = nil
		return
	}
	maxCon := cfg.MaxConcurrent
	if maxCon < 1 {
		maxCon = 1
	}
	s.SetBrainConsultor(newBrainConsultor(s.life, s.store, s.audit, cfg), maxCon)
}

// ConsultBrain implements POST /api/v1/autopilot/brain-consult: the autopilot
// manager's thin wrapper over the shared Consultor (spec D5). The owning run is
// derived from the calling manager's session identity. Does not touch the
// manager slot, Guardian, or run ledger — only spawn/teardown/audit of the
// short-lived resolver go through Consultor (identical to Phase 1 / pipeline).
func (s *Server) ConsultBrain(ctx context.Context, req oapi.ConsultBrainRequestObject) (oapi.ConsultBrainResponseObject, error) {
	cfg := s.snapshotConfig()
	if !cfg.BrainConsult.Enabled {
		return oapi.ConsultBrain403JSONResponse{Error: "brain consult is disabled"}, nil
	}
	if s.brainConsultor == nil {
		return oapi.ConsultBrain403JSONResponse{Error: "brain consult is not configured"}, nil
	}
	if s.autopilot == nil {
		return oapi.ConsultBrain403JSONResponse{Error: autopilotDisabledMsg}, nil
	}

	caller := s.callerSession(ctx)
	if caller == nil || caller.Role != autopilotBrainRole {
		return oapi.ConsultBrain403JSONResponse{Error: "only an autopilot manager may consult the brain"}, nil
	}
	runID := runIDFromTags(caller.Tags)
	if runID == "" {
		return oapi.ConsultBrain403JSONResponse{Error: "calling manager carries no run tag"}, nil
	}
	if !s.autopilot.CanBrainComplete(runID, caller.ID) {
		// Reuse the "active brain for run" check — same identity gate as complete.
		return oapi.ConsultBrain403JSONResponse{Error: "only the run's active manager may consult the brain"}, nil
	}

	if req.Body == nil || strings.TrimSpace(req.Body.Intent) == "" {
		return nil, errStatus(http.StatusBadRequest, "intent is required")
	}
	body := *req.Body

	repo := caller.Repo
	if repo == "" {
		repo = caller.Workdir
	}

	bcReq := brainconsult.Request{
		Intent:       body.Intent,
		Situation:    body.Situation,
		Goal:         body.Goal,
		AlreadyTried: body.AlreadyTried,
		Evidence:     body.Evidence,
		Allowed:      mapAllowed(body.Allowed),
		RunID:        runID,
		TaskID:       body.TaskId,
		Repo:         repo,
	}
	if len(bcReq.Allowed) == 0 {
		bcReq.Allowed = append([]brainconsult.Action(nil), defaultAutopilotAllowed...)
	}

	result, err := s.brainConsultor.Consult(ctx, bcReq)
	if errors.Is(err, brainconsult.ErrNoBrainReply) {
		slog.Warn("autopilot brain consult: no reply", "run", runID, "brain_id", result.BrainID, "err", err)
		return oapi.ConsultBrain504JSONResponse{Error: err.Error()}, nil
	}
	if err != nil {
		slog.Warn("autopilot brain consult failed", "run", runID, "err", err)
		return oapi.ConsultBrain403JSONResponse{Error: err.Error()}, nil
	}

	return oapi.ConsultBrain200JSONResponse{
		Action:  oapi.BrainConsultResultAction(result.Action),
		Reason:  result.Reason,
		BrainId: result.BrainID,
	}, nil
}

// mapAllowed translates the OpenAPI allowed enum into brainconsult.Action values.
func mapAllowed(in []oapi.BrainConsultRequestAllowed) []brainconsult.Action {
	if len(in) == 0 {
		return nil
	}
	out := make([]brainconsult.Action, 0, len(in))
	for _, a := range in {
		out = append(out, brainconsult.Action(a))
	}
	return out
}

// newBrainConsultor builds the shared Consultor from config + the daemon's
// lifecycle/audit surface. Role follows the frozen Phase 1 default
// (autopilotBrainRole / "autopilot") so consult spawns share the same Lifecycle
// Role surface as SpawnBrain without touching the manager slot (empty Ticket).
func newBrainConsultor(life Lifecycle, st store.Store, aw *audit.Writer, cfg config.BrainConsultConfig) brainconsult.Consultor {
	timeout := 10 * time.Minute
	if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
		timeout = d
	}
	return brainconsult.New(
		brainConsultSpawner{life: life, store: st},
		aw,
		brainconsult.Options{
			Timeout: timeout,
			Role:    autopilotBrainRole,
		},
	)
}
