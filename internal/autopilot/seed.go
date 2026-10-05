package autopilot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/store"
)

// SeedRuntime is the optional slice of the runtime the guardian/overwatch need to
// supervise a failed initial-prompt seed (typed-prompt backends only): read an
// agent's seed status and re-deliver text to its pane. A runtime that does not
// implement it makes every seed check a no-op.
type SeedRuntime interface {
	// SeedState returns the agent's seed status (store.SeedPending / SeedDelivered
	// / SeedFailed; "" = none). ok=false when unknown — never acted on.
	SeedState(ctx context.Context, agentID string) (status string, ok bool)
	// RedeliverSeed types text into the agent's pane via Input; on success the
	// agent's seed status becomes delivered.
	RedeliverSeed(ctx context.Context, agentID, text string) error
}

// superviseSeed handles a manager whose initial digest never reached its pane
// (seed_status=failed): it re-delivers the digest once; if that also fails (or the
// status is still failed afterwards) it escalates as a spawn failure through the
// existing classification + heal ladder, so the run is never left "active" with an
// empty-composer manager. Reports true when it escalated and the tick should end.
func (c *Controller) superviseSeed(ctx context.Context, gr GuardianRuntime, r *run, now time.Time) bool {
	sr, ok := c.runtime.(SeedRuntime)
	if !ok || r.brain == nil || r.brain.AgentID == "" {
		return false
	}
	id := r.brain.AgentID
	if status, ok := sr.SeedState(ctx, id); !ok || status != store.SeedFailed || r.seedEscalated == id {
		return false
	}
	var cause error
	if r.seedRedelivered != id {
		r.seedRedelivered = id
		cause = c.redeliverDigest(ctx, sr, r, id)
		if cause == nil {
			slog.Info("autopilot guardian: re-delivered manager digest after failed seed", "run", r.runID, "agent", id)
			gr.AuditRunEvent(ctx, r.runID, "autopilot.seed_redelivered", id, "initial prompt seed failed; re-delivered the digest")
			return false
		}
	} else {
		cause = fmt.Errorf("prompt still undelivered after re-delivery")
	}
	r.seedEscalated = id
	r.state = StateDegraded
	c.enterBackoff(gr, r, now, classifySpawnError(cause), false, fmt.Errorf("manager %s never received its prompt: %w", id, cause))
	return true
}

// redeliverDigest composes the run's current digest and types it into the manager.
func (c *Controller) redeliverDigest(ctx context.Context, sr SeedRuntime, r *run, id string) error {
	if c.planBound(r) {
		if err := c.hydratePlanFromSource(ctx, r); err != nil {
			return err
		}
	}
	digest, err := ComposeDigest(ctx, DigestInput{
		RunID:             r.runID,
		Repo:              r.repo,
		PlanFile:          r.absPlanFile,
		Plan:              r.plan,
		Ledger:            c.runtime.NewLedger(r.runID),
		Sources:           c.runtime.DigestSources(),
		IntegrationBranch: r.integrationBranch,
	})
	if err != nil {
		return err
	}
	return sr.RedeliverSeed(ctx, id, digest)
}

// tellManagerSeedFailed wakes the manager once per worker whose initial prompt
// never got typed, so it can re-send the task or replace the worker. Reports true
// when a message went out.
func (c *Controller) tellManagerSeedFailed(ctx context.Context, ow OverwatchRuntime, r *run, roster []AgentInfo) bool {
	var failed []string
	for _, a := range roster {
		if a.ID == r.brain.AgentID || a.SeedStatus != store.SeedFailed || r.seedReported[a.ID] {
			continue
		}
		failed = append(failed, fmt.Sprintf("%s (%s)", a.Name+" "+a.ID, a.Branch))
	}
	if len(failed) == 0 {
		return false
	}
	msg := overwatchNudgePrefix + "these workers never received their initial prompt and are idle at an empty composer — " +
		"re-send each its task with send_to_agent, or terminate and respawn it: " + strings.Join(failed, "; ")
	if err := ow.WakeAgent(ctx, r.brain.AgentID, msg); err != nil {
		slog.Warn("autopilot overwatch: seed-failure wake failed", "run", r.runID, "err", err)
		return false
	}
	if r.seedReported == nil {
		r.seedReported = map[string]bool{}
	}
	for _, a := range roster {
		if a.SeedStatus == store.SeedFailed {
			r.seedReported[a.ID] = true
		}
	}
	return true
}
