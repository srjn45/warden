package daemon

import (
	"context"
	"errors"
	"time"

	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/schedule"
)

// schedulerDisabledMsg mirrors snapshotsDisabledMsg: a friendly hint when the
// feature gate is off rather than a bare 403.
const schedulerDisabledMsg = "scheduler disabled (enable with scheduler_enabled: true in the config file)"

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
