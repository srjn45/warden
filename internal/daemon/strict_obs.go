package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/role"
	"github.com/srjn45/warden/internal/schedule"
	"github.com/srjn45/warden/internal/snapshot"
)

// GetMetrics implements GET /api/v1/metrics: a live resource snapshot (an empty
// sample when the collector is unwired).
func (s *Server) GetMetrics(ctx context.Context, _ oapi.GetMetricsRequestObject) (oapi.GetMetricsResponseObject, error) {
	if s.mcollector == nil {
		return oapi.GetMetrics200JSONResponse(metrics.Sample{}), nil
	}
	sample, err := s.mcollector.Sample(ctx)
	if err != nil {
		return nil, err
	}
	return oapi.GetMetrics200JSONResponse(sample), nil
}

// GetMetricsHistory implements GET /api/v1/metrics/history: raw samples by
// default, or per-agent summaries with ?summary=true (optionally narrowed to one
// ?agent=).
func (s *Server) GetMetricsHistory(_ context.Context, req oapi.GetMetricsHistoryRequestObject) (oapi.GetMetricsHistoryResponseObject, error) {
	if s.mrecorder == nil {
		return oapi.GetMetricsHistory200JSONResponse{Samples: []oapi.MetricsSample{}}, nil
	}
	since := time.Now().Add(-metricsHistoryDefaultWindow)
	if !req.Params.Since.IsZero() {
		since = req.Params.Since
	}
	limit := metricsHistoryMaxSamples
	if n := req.Params.Limit; n > 0 && n < limit {
		limit = n
	}
	samples, err := s.mrecorder.History(since, limit)
	if err != nil {
		return nil, err
	}
	if samples == nil {
		samples = []metrics.Sample{}
	}
	if req.Params.Summary {
		summaries := metrics.SummarizeAgents(samples, metrics.HistoryThresholds{
			ContextWarn: s.mTokenWarn,
			ContextCrit: s.mTokenCrit,
		})
		if want := req.Params.Agent; want != "" {
			filtered := summaries[:0]
			for _, sum := range summaries {
				if sum.ID == want {
					filtered = append(filtered, sum)
				}
			}
			summaries = filtered
		}
		if summaries == nil {
			summaries = []metrics.AgentSummary{}
		}
		return oapi.GetMetricsHistory200JSONResponse{Summaries: summaries}, nil
	}
	return oapi.GetMetricsHistory200JSONResponse{Samples: samples}, nil
}

// ListSchedules implements GET /api/v1/schedules.
func (s *Server) ListSchedules(_ context.Context, _ oapi.ListSchedulesRequestObject) (oapi.ListSchedulesResponseObject, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	list, err := s.schedStore.List()
	if err != nil {
		return nil, err
	}
	out := make([]oapi.Schedule, 0, len(list))
	for _, sc := range list {
		out = append(out, *sc)
	}
	return oapi.ListSchedules200JSONResponse{Schedules: out}, nil
}

// CreateSchedule implements POST /api/v1/schedules. A pipeline schedule's YAML
// spec is validated at create time so a malformed spec is rejected now, not on
// the first fire.
func (s *Server) CreateSchedule(ctx context.Context, req oapi.CreateScheduleRequestObject) (oapi.CreateScheduleResponseObject, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	var b oapi.ScheduleCreateRequest
	if req.Body != nil {
		b = *req.Body
	}
	// --now is the explicit "fire once as soon as possible": a single-shot due
	// immediately. It is stored as that instant so the next tick fires it.
	now := time.Now()
	if b.Now {
		if strings.TrimSpace(b.Cron) != "" || strings.TrimSpace(b.At) != "" {
			return nil, errStatus(http.StatusBadRequest, "--now cannot be combined with --cron or --at")
		}
		b.At = now.Format(time.RFC3339)
	} else if strings.TrimSpace(b.At) != "" && strings.TrimSpace(b.Cron) == "" {
		if err := schedule.CheckAtInFuture(b.At, now); err != nil {
			return nil, errStatus(http.StatusBadRequest, err.Error())
		}
	}
	params := schedule.Params{
		Name:           b.Name,
		Cron:           b.Cron,
		At:             b.At,
		Type:           b.Type,
		Repo:           b.Repo,
		Cwd:            b.Cwd,
		Role:           b.Role,
		Prompt:         b.Prompt,
		Agent:          b.Agent,
		Branch:         b.Branch,
		Model:          b.Model,
		AiCli:          b.AiCli,
		PermissionMode: b.PermissionMode,
		AutoRestart:    b.AutoRestart,
		Tags:           b.Tags,
		Tier:           b.Tier,
		ProjectID:      b.ProjectId,
		Spec:           b.Spec,
	}
	if strings.TrimSpace(b.Spec) != "" {
		if conflicts := schedule.AgentFlagConflicts(params); len(conflicts) > 0 {
			return nil, errStatus(http.StatusBadRequest, "a pipeline schedule fires the pipeline as written; "+
				strings.Join(conflicts, ", ")+" only apply to an agent schedule — drop them or drop the pipeline")
		}
		if _, err := pipeline.ParseSpec([]byte(b.Spec)); err != nil {
			return nil, errStatus(http.StatusBadRequest, "invalid pipeline spec: "+err.Error())
		}
	}
	if strings.TrimSpace(b.Spec) == "" {
		if msg := s.validateScheduleAgent(ctx, b); msg != "" {
			return nil, errStatus(http.StatusBadRequest, msg)
		}
	}
	sc, err := schedule.New(params, now)
	if err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	if err := s.schedStore.Create(sc); errors.Is(err, schedule.ErrExists) {
		return nil, errStatus(http.StatusConflict, "schedule "+sc.ID+" already exists")
	} else if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionScheduleCreate, sc.ID, map[string]string{"kind": string(sc.Kind), "mode": string(sc.Mode)})
	return oapi.CreateSchedule201JSONResponse(*sc), nil
}

// DeleteSchedule implements DELETE /api/v1/schedules/{id}.
func (s *Server) DeleteSchedule(ctx context.Context, req oapi.DeleteScheduleRequestObject) (oapi.DeleteScheduleResponseObject, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	if err := s.schedStore.Delete(req.Id); errors.Is(err, schedule.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "schedule not found")
	} else if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionScheduleDelete, req.Id, nil)
	return oapi.DeleteSchedule200JSONResponse{OKJSONResponse: oapi.OKJSONResponse{Status: "deleted"}}, nil
}

// GetSchedule implements GET /api/v1/schedules/{id}.
func (s *Server) GetSchedule(_ context.Context, req oapi.GetScheduleRequestObject) (oapi.GetScheduleResponseObject, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	sc, err := s.schedStore.Get(req.Id)
	if errors.Is(err, schedule.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "schedule not found")
	} else if err != nil {
		return nil, err
	}
	return oapi.GetSchedule200JSONResponse(*sc), nil
}

// EnableSchedule implements POST /api/v1/schedules/{id}/enable. Idempotent:
// re-arms next_run from now. Returns the updated schedule.
func (s *Server) EnableSchedule(ctx context.Context, req oapi.EnableScheduleRequestObject) (oapi.EnableScheduleResponseObject, error) {
	sc, err := s.setScheduleEnabled(ctx, req.Id, true)
	if err != nil {
		return nil, err
	}
	return oapi.EnableSchedule200JSONResponse(*sc), nil
}

// DisableSchedule implements POST /api/v1/schedules/{id}/disable. Idempotent:
// clears next_run; the record and its last-run history are preserved.
func (s *Server) DisableSchedule(ctx context.Context, req oapi.DisableScheduleRequestObject) (oapi.DisableScheduleResponseObject, error) {
	sc, err := s.setScheduleEnabled(ctx, req.Id, false)
	if err != nil {
		return nil, err
	}
	return oapi.DisableSchedule200JSONResponse(*sc), nil
}

// setScheduleEnabled flips a schedule's enabled state under the store lock,
// re-arming (enable) or clearing (disable) next_run, records the audit event, and
// returns the updated record. Shared by EnableSchedule/DisableSchedule.
func (s *Server) setScheduleEnabled(ctx context.Context, id string, enabled bool) (*schedule.Schedule, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	var recomputeErr error
	err := s.schedStore.Update(id, func(stored *schedule.Schedule) {
		recomputeErr = schedule.SetEnabled(stored, enabled, time.Now())
	})
	if errors.Is(err, schedule.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "schedule not found")
	} else if err != nil {
		return nil, err
	}
	if recomputeErr != nil {
		return nil, errStatus(http.StatusBadRequest, recomputeErr.Error())
	}
	sc, err := s.schedStore.Get(id)
	if err != nil {
		return nil, err
	}
	action := audit.ActionScheduleDisable
	if enabled {
		action = audit.ActionScheduleEnable
	}
	s.recordAuditCtx(ctx, action, id, nil)
	s.notify()
	return sc, nil
}

// UpdateSchedule implements PATCH /api/v1/schedules/{id}: edit timing or payload
// in place, keeping the id, created time and last-run history. The patch is
// validated on a copy (including the same fireability check create uses) and
// then applied to the stored record under the store lock, so an edit racing a
// scheduler tick cannot clobber the tick's last-run bookkeeping.
func (s *Server) UpdateSchedule(ctx context.Context, req oapi.UpdateScheduleRequestObject) (oapi.UpdateScheduleResponseObject, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	var b oapi.ScheduleUpdateRequest
	if req.Body != nil {
		b = *req.Body
	}
	patch := schedule.Patch{
		Cron: b.Cron, At: b.At, Repo: b.Repo, Cwd: b.Cwd, Role: b.Role, Prompt: b.Prompt,
		Agent: b.Agent, Branch: b.Branch, Model: b.Model, AiCli: b.AiCli, Spec: b.Spec,
	}
	cur, err := s.schedStore.Get(req.Id)
	if errors.Is(err, schedule.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "schedule not found")
	} else if err != nil {
		return nil, err
	}
	now := time.Now()
	// A new repo replaces the working-directory default, as on create — but only
	// for a worktree-owning role, whose agent runs in the repo's worktree.
	if patch.Repo != nil && strings.TrimSpace(*patch.Repo) != "" && patch.Cwd == nil && cur.Mode == schedule.ModeAgent {
		probe := *cur
		probe.Repo = *patch.Repo
		if lifecycle.RoleOwnsWorktree(scheduleSpawnRequest(&probe).Role) {
			empty := ""
			patch.Cwd = &empty
		}
	}
	cand := *cur
	if err := schedule.ApplyPatch(&cand, patch, now); err != nil {
		return nil, errStatus(http.StatusBadRequest, err.Error())
	}
	if cand.Mode == schedule.ModePipeline {
		if _, err := pipeline.ParseSpec([]byte(cand.Spec)); err != nil {
			return nil, errStatus(http.StatusBadRequest, "invalid pipeline spec: "+err.Error())
		}
	} else if msg := s.validateScheduleProbe(ctx, &cand); msg != "" {
		return nil, errStatus(http.StatusBadRequest, msg)
	}
	var applyErr error
	err = s.schedStore.Update(req.Id, func(stored *schedule.Schedule) {
		applyErr = schedule.ApplyPatch(stored, patch, now)
	})
	if errors.Is(err, schedule.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "schedule not found")
	} else if err != nil {
		return nil, err
	}
	if applyErr != nil {
		return nil, errStatus(http.StatusBadRequest, applyErr.Error())
	}
	sc, err := s.schedStore.Get(req.Id)
	if err != nil {
		return nil, err
	}
	s.recordAuditCtx(ctx, audit.ActionScheduleUpdate, sc.ID, nil)
	s.notify()
	return oapi.UpdateSchedule200JSONResponse(*sc), nil
}

// RunSchedule implements POST /api/v1/schedules/{id}/run: fire the schedule once
// now through the same path the scheduler tick uses, recording the outcome
// without re-arming — next run, enabled state and a single-shot's pending state
// are untouched.
func (s *Server) RunSchedule(ctx context.Context, req oapi.RunScheduleRequestObject) (oapi.RunScheduleResponseObject, error) {
	if !s.scheduler || s.schedStore == nil {
		return nil, errStatus(http.StatusForbidden, schedulerDisabledMsg)
	}
	sc, err := s.schedStore.Get(req.Id)
	if errors.Is(err, schedule.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "schedule not found")
	} else if err != nil {
		return nil, err
	}
	now := time.Now()
	runID, fireErr := s.fireSchedule(ctx, sc)
	if fireErr != nil {
		runID = ""
	}
	// Persist under the same store update the tick uses.
	if uerr := s.schedStore.Update(req.Id, func(stored *schedule.Schedule) {
		schedule.RecordRun(stored, now, runID, fireErr)
	}); uerr != nil && !errors.Is(uerr, schedule.ErrNotFound) {
		return nil, uerr
	}
	s.recordAuditCtx(ctx, audit.ActionScheduleRun, req.Id, nil)
	s.notify()
	if fireErr != nil {
		return nil, errStatus(http.StatusConflict, fireErr.Error())
	}
	cur, err := s.schedStore.Get(req.Id)
	if err != nil {
		return nil, err
	}
	return oapi.RunSchedule200JSONResponse{Schedule: *cur, RunId: runID}, nil
}

// ListSnapshots implements GET /api/v1/snapshots, newest first, optionally
// filtered to one ?session=.
func (s *Server) ListSnapshots(ctx context.Context, req oapi.ListSnapshotsRequestObject) (oapi.ListSnapshotsResponseObject, error) {
	if !s.snapshots || s.snap == nil {
		return nil, errStatus(http.StatusForbidden, snapshotsDisabledMsg)
	}
	snaps, err := s.snap.List(ctx, req.Params.Session)
	if err != nil {
		return nil, err
	}
	out := make([]oapi.Snapshot, 0, len(snaps))
	for _, sn := range snaps {
		out = append(out, *sn)
	}
	return oapi.ListSnapshots200JSONResponse{Snapshots: out}, nil
}

// CreateSnapshot implements POST /api/v1/snapshots: capture the calling agent's
// worktree + transcript (pinned to its own Workdir).
func (s *Server) CreateSnapshot(ctx context.Context, req oapi.CreateSnapshotRequestObject) (oapi.CreateSnapshotResponseObject, error) {
	if !s.snapshots || s.snap == nil {
		return nil, errStatus(http.StatusForbidden, snapshotsDisabledMsg)
	}
	var b oapi.SnapshotCreateRequest
	if req.Body != nil {
		b = *req.Body
	}
	dir, sess, err := s.pinnedGitTarget(ctx, b.Session, b.Dir)
	if err != nil {
		return nil, err
	}
	cr := snapshot.CaptureRequest{
		SessionID: b.Session, // raw id so list-by-session works even for an unknown/human session
		Workdir:   dir,
		Message:   b.Message,
	}
	if sess != nil {
		cr.SessionID = sess.ID
		cr.TmuxSession = sess.TmuxSession
	}
	snap, err := s.snap.Capture(ctx, cr)
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	if sess != nil {
		s.recordGitEvent(sess.ID, "snapshot", "captured "+snap.ID)
	}
	return oapi.CreateSnapshot200JSONResponse(*snap), nil
}

// RestoreSnapshot implements POST /api/v1/snapshots/{id}/restore. A dirty-tree
// refusal (without force) is a 409; a missing snapshot a 404.
func (s *Server) RestoreSnapshot(ctx context.Context, req oapi.RestoreSnapshotRequestObject) (oapi.RestoreSnapshotResponseObject, error) {
	if !s.snapshots || s.snap == nil {
		return nil, errStatus(http.StatusForbidden, snapshotsDisabledMsg)
	}
	var force bool
	if req.Body != nil {
		force = req.Body.Force
	}
	// Ownership guard (autopilot.md §8): a run's brain may only restore a snapshot
	// of an agent it owns. Resolve the snapshot's owning session up front so the
	// refusal lands before any state is applied. A bare-dir snapshot (no
	// SessionID) or an unknown/unresolvable owner leaves the guard a no-op.
	if snap, gerr := s.snap.Get(req.Id); gerr == nil && snap.SessionID != "" {
		if target, terr := s.store.GetByNameOrID(ctx, snap.SessionID); terr == nil {
			if err := s.guardOwnership(ctx, target); err != nil {
				return nil, err
			}
		}
	}
	res, err := s.snap.Restore(ctx, req.Id, force)
	if errors.Is(err, snapshot.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "snapshot not found: "+req.Id)
	}
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	return oapi.RestoreSnapshot200JSONResponse(*res), nil
}

// validateScheduleAgent rejects an agent-mode schedule that could never fire:
// it builds the spawn the fire would build and runs the spawn checks that do
// not depend on transient state (name collisions and existing tickets are only
// meaningful at fire time). It returns the reason, or "" when acceptable. Done
// here, not in schedule.Validate, to keep that package dependency-light.
func (s *Server) validateScheduleAgent(ctx context.Context, b oapi.ScheduleCreateRequest) string {
	return s.validateScheduleProbe(ctx, &schedule.Schedule{
		Type: b.Type, Repo: b.Repo, Cwd: b.Cwd, Role: b.Role,
		Agent: b.Agent, Branch: b.Branch, Prompt: b.Prompt,
		Model: b.Model, AiCli: b.AiCli, PermissionMode: b.PermissionMode,
		AutoRestart: b.AutoRestart, Tags: b.Tags, Tier: b.Tier, ProjectID: b.ProjectId,
	})
}

// validateScheduleProbe is the create-time fireability check on an agent
// schedule's payload, shared by create and edit. It returns the reason, or "".
func (s *Server) validateScheduleProbe(ctx context.Context, probe *schedule.Schedule) string {
	if r := strings.TrimSpace(probe.Role); r != "" {
		if _, ok := role.Get(r); !ok {
			return "unknown role " + r + " (valid: " + strings.Join(role.Names(), ", ") + ")"
		}
	}
	if t := strings.TrimSpace(probe.Tier); t != "" && !backendstore.ModelTier(t).Valid() {
		return "invalid tier " + t + " (valid: tier-1, tier-2, tier-3)"
	}
	req := scheduleSpawnRequest(probe)
	if code, msg := s.validateSpawnRequestOpts(ctx, req, true); code != 0 {
		return scheduleFireHint(code, msg, req)
	}
	return ""
}
