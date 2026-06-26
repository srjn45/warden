package daemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/daemon/oapi"
	"github.com/srjn45/warden/internal/metrics"
	"github.com/srjn45/warden/internal/pipeline"
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
	if b.Spec != "" {
		if _, err := pipeline.ParseSpec([]byte(b.Spec)); err != nil {
			return nil, errStatus(http.StatusBadRequest, "invalid pipeline spec: "+err.Error())
		}
	}
	sc, err := schedule.New(schedule.Params{
		Name:   b.Name,
		Cron:   b.Cron,
		At:     b.At,
		Type:   b.Type,
		Repo:   b.Repo,
		Prompt: b.Prompt,
		Agent:  b.Agent,
		Branch: b.Branch,
		Spec:   b.Spec,
	}, time.Now())
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
	res, err := s.snap.Restore(ctx, req.Id, force)
	if errors.Is(err, snapshot.ErrNotFound) {
		return nil, errStatus(http.StatusNotFound, "snapshot not found: "+req.Id)
	}
	if err != nil {
		return nil, errStatus(http.StatusConflict, err.Error())
	}
	return oapi.RestoreSnapshot200JSONResponse(*res), nil
}
