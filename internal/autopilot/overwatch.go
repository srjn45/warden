package autopilot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// The overwatch is the daemon-internal backstop that keeps an autopilot manager
// tending its fleet even when its own prompt discipline lapses (autopilot.md
// §2.4). It complements the guardian: the guardian watches the manager's
// *liveness* (heartbeat → heal ladder), while the overwatch watches the
// *workers'* state and nudges a live-but-quiet manager to act on them. It never
// touches workers itself — it only ever sends the manager a steering message.
//
// Two triggers, both delivered as a wake (pane-injected input turn — mail is
// pull-only and an idle manager never reads it) and both gated on the manager
// being idle (a busy manager is never interrupted — it will see its workers on
// its own):
//
//   - periodic — a heartbeat nudge once per overwatchPeriod, so an idle manager
//     with nothing obviously wrong is still reminded to reconcile and progress;
//   - event-driven — when any worker under the run has fallen idle or is waiting
//     on input, debounced to at most one nudge per overwatchMinGap.
//
// The worker roster is derived purely from the `run:<run_id>` ownership tag
// (OverwatchRuntime.RunAgents), so it carries zero persisted state and a
// restarted daemon re-adopts it for free.
const (
	// overwatchPeriod is the periodic heartbeat-nudge cadence for an idle manager.
	// Generous by design (frictionless-safeguards philosophy) — the overwatch is a
	// backstop, not a pacer of a healthy run.
	overwatchPeriod = time.Hour
	// overwatchMinGap floors the interval between event-driven nudges so a run with
	// several workers idling at once yields one nudge, not a storm.
	overwatchMinGap = 5 * time.Minute
	// overwatchNudgeListMax caps how many needy workers a single nudge names before
	// collapsing the rest into a "(+N more)" tail, keeping the message bounded.
	overwatchNudgeListMax = 8
)

// overwatchNudgePrefix stamps every overwatch steering message, mirroring the
// guardian's nudge convention so the manager can tell the two apart.
const overwatchNudgePrefix = "autopilot overwatch: "

// OverwatchRuntime is the optional slice of the runtime the overwatch needs
// beyond the base Runtime (autopilot.md §2.4): the run's live agent roster (the
// manager plus every worker, resolved by ownership tag) and a wake delivery to
// the manager. A runtime that does not implement it (the inert core, the
// guardian/lifecycle fakes) makes every overwatch tick a no-op, so wiring it in
// unconditionally is safe.
type OverwatchRuntime interface {
	// RunAgents returns every agent currently tagged run:<runID> — the manager and
	// its workers — each with the poller's live status in AgentInfo.State. Derived
	// from tags, so a restart re-adopts the roster with no persisted state.
	RunAgents(ctx context.Context, runID string) ([]AgentInfo, error)
	// WakeAgent delivers msg to the agent as a new input turn (pane injection —
	// the send_to_agent path), which genuinely wakes an idle agent. The mailbox
	// (NudgeBrain) is NOT sufficient here: mail is pull-only, and an idle manager
	// runs no loop that would ever read it — precisely the state the overwatch
	// fires in. The overwatch's skip-while-busy gate makes injection safe by
	// construction (it never types into an agent mid-turn).
	WakeAgent(ctx context.Context, agentID, msg string) error
}

// overwatchTick supervises every live run's fleet once (autopilot.md §2.4). Like
// the guardian tick it honors the per-repo kill switch and no-ops when the
// runtime cannot support an overwatch, and holds c.mu for the whole pass so run
// state stays consistent with Enable/guardianTick. Called from RunGuardian on
// the guardian's cadence; the actual nudge cadence is time-gated per run,
// independent of the tick rate.
func (c *Controller) overwatchTick(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ow, ok := c.runtime.(OverwatchRuntime)
	if !ok {
		return
	}
	now := c.now()
	for _, r := range c.runs {
		// Per-repo kill switch — mirrors guardianTick: a disabled repo's runs are
		// supervised by nothing.
		if !c.enableStore.IsEnabled(r.repo) {
			continue
		}
		c.overwatchRun(ctx, ow, r, now)
	}
}

// overwatchRun refreshes one run's worker roster and, when warranted, nudges the
// manager to tend it. It always refreshes the cached in-flight count (so status
// reflects reality regardless of state), then applies the nudge gates: only an
// active run with a live, idle manager is ever nudged — a starting/healing/
// degraded/complete run is left to the guardian (or is finished).
func (c *Controller) overwatchRun(ctx context.Context, ow OverwatchRuntime, r *run, now time.Time) {
	if r.brain == nil || r.brain.AgentID == "" {
		r.workersInFlight = 0
		return // no manager to nudge, and no fleet without one
	}
	roster, err := ow.RunAgents(ctx, r.runID)
	if err != nil {
		slog.Warn("autopilot overwatch: roster read failed", "run", r.runID, "err", err)
		return // keep the last-known count; try again next tick
	}

	inFlight := 0
	managerBusy := false
	var needy []AgentInfo
	for _, a := range roster {
		if a.ID == r.brain.AgentID {
			managerBusy = isAgentBusy(a.State) // the manager is the brain
			continue
		}
		if isAgentBusy(a.State) {
			inFlight++
			continue
		}
		needy = append(needy, a)
	}
	r.workersInFlight = inFlight

	// Only a healthy, active run with an idle manager is nudged. While the run is
	// starting/healing/degraded the guardian owns the manager, and a busy manager
	// will notice its workers itself — interrupting it just derails the run.
	if r.state != StateActive || managerBusy {
		return
	}

	// The nudge clock floors at the manager's spawn instant (the guardian's
	// cold-start convention): a manager that spawned moments ago is never nudged
	// for being briefly idle while its CLI boots or between its first turns.
	last := r.overwatchLastNudgeAt
	if last.IsZero() || r.brainSpawnedAt.After(last) {
		last = r.brainSpawnedAt
	}
	since := now.Sub(last)
	periodic := since >= overwatchPeriod
	event := len(needy) > 0 && since >= overwatchMinGap
	if !periodic && !event {
		return
	}

	if err := ow.WakeAgent(ctx, r.brain.AgentID, composeOverwatchNudge(needy)); err != nil {
		slog.Warn("autopilot overwatch: wake failed", "run", r.runID, "err", err)
	}
	r.overwatchLastNudgeAt = now
}

// isAgentBusy reports whether an agent's live status means it is actively working
// and needs no attention. Everything else — waiting_for_input, idle, done,
// errored, orphaned, rate_limited — is "not busy" and, for a worker, something
// the manager should tend (autopilot.md §2.4).
func isAgentBusy(state string) bool {
	switch state {
	case "spawning", "working":
		return true
	default:
		return false
	}
}

// composeOverwatchNudge builds the manager-facing steering message. With no needy
// workers it is a periodic check-in; otherwise it names the idle/waiting workers
// (bounded) and asks the manager to answer or steer waiting ones and clean up
// finished ones before pulling the next task.
func composeOverwatchNudge(needy []AgentInfo) string {
	if len(needy) == 0 {
		return overwatchNudgePrefix + "periodic check-in — reconcile the ledger against list_agents, " +
			"pull the next pending task if a worker slot is free, and verify the plan's done_when so a " +
			"finished run gets marked complete and stops spawning."
	}

	var b strings.Builder
	b.WriteString(overwatchNudgePrefix)
	b.WriteString("these workers are idle or waiting on input and need you — ")
	shown := needy
	extra := 0
	if len(shown) > overwatchNudgeListMax {
		extra = len(shown) - overwatchNudgeListMax
		shown = shown[:overwatchNudgeListMax]
	}
	parts := make([]string, 0, len(shown))
	for _, a := range shown {
		label := a.Name
		if label == "" {
			label = a.ID
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", label, a.ID, a.State))
	}
	b.WriteString(strings.Join(parts, "; "))
	if extra > 0 {
		fmt.Fprintf(&b, " (+%d more)", extra)
	}
	b.WriteString(". Check every worker that is waiting_for_input and answer or steer it; " +
		"clean up finished or idle workers (terminate then remove_worktree, mark the task landed in the ledger); " +
		"then pull the next pending task.")
	return b.String()
}
