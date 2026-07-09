package autopilot

import (
	"context"
	"fmt"
	"os"
	"time"
)

// autopilotTag is the tag every autopilot-created agent (brain + workers) carries
// (autopilot.md §1). The brain role's defaults also apply it; the Controller sets
// it explicitly so ownership never depends on persona/role discipline.
const autopilotTag = "autopilot"

// runTag is the per-run ownership tag (`run:<run_id>`) applied to the brain and
// inherited by its workers (autopilot.md §1, §8 ownership guard).
func runTag(runID string) string { return "run:" + runID }

// BackendLadder is the cost-tier backend preference list (autopilot.md §7). S3
// consumes only Free (brain selection hardcodes the first free backend) and the
// union (preflight trust check); S5 walks the full ladder with limit tracking.
type BackendLadder struct {
	Free         []string
	Subscription []string
	PayPerUse    []string
}

// all returns every configured backend across tiers, in preference order
// (free → subscription → pay_per_use), skipping blanks. Used by the preflight
// trust check, which must validate every backend the ladder could ever select.
func (l BackendLadder) all() []string {
	var out []string
	for _, group := range [][]string{l.Free, l.Subscription, l.PayPerUse} {
		for _, b := range group {
			if b != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// firstFree returns the first configured free-tier backend, or "" when none is
// configured. S3's hardcoded brain selection (the full tier loop is S5).
func (l BackendLadder) firstFree() string {
	for _, b := range l.Free {
		if b != "" {
			return b
		}
	}
	return ""
}

// BrainSpec is the request to launch a run's brain through the daemon's existing
// agent lifecycle. The Controller composes it; the daemon runtime adapter maps it
// onto a spawn request (role autopilot, headless, prompt = recovery digest).
type BrainSpec struct {
	RunID    string   // owning run
	Repo     string   // repo root (agent cwd)
	PlanFile string   // absolute plan-file path
	Backend  string   // selected backend ("" ⇒ the daemon's default)
	Prompt   string   // opening brief: the recovery digest
	Tags     []string // [autopilot, run:<run_id>]
	Headless bool     // run non-interactively (unattended)
}

// BrainHandle identifies a spawned brain.
type BrainHandle struct {
	AgentID string
	Backend string
}

// Runtime is the daemon-provided surface the Controller drives once a run is
// live: spawn/teardown the brain, read the ledger and digest sources, and notify
// the owner. It is injected (SetRuntime) rather than constructed so the S1 inert
// core — and every controller unit test that passes no runtime — keeps working
// unchanged (nil runtime ⇒ no brain spawns, the S1 behavior).
type Runtime interface {
	// SpawnBrain launches the run's headless brain and returns its handle. A
	// failure leaves the run degraded (autopilot.md §2.1); the guardian (S5)
	// heals it.
	SpawnBrain(ctx context.Context, spec BrainSpec) (BrainHandle, error)
	// TerminateBrain gracefully stops the brain agent (kill switch / rotation).
	// In-flight workers are untouched.
	TerminateBrain(ctx context.Context, agentID string) error
	// NewLedger returns a run-scoped ledger over the daemon's ctx store.
	NewLedger(runID string) *Ledger
	// DigestSources supplies live agents + recent audit for the recovery digest.
	DigestSources() DigestSources
	// NotifyOwner surfaces an owner-facing message — the one path §3 permits: a
	// mid-run plan edit that fails to validate (the run keeps its last-good plan).
	NotifyOwner(runID, msg string)
	// InstallDefaultAutoApprovePolicy installs the generous default auto-approve
	// policy for autopilot-owned agents when the owner has configured no rules of
	// their own (autopilot.md §10), so day-one workers don't stall on recognized
	// non-destructive prompts (anything the policy still can't answer routes to the
	// brain, §8). Idempotent: a no-op once the owner has any rules.
	InstallDefaultAutoApprovePolicy()
}

// GuardianRuntime is the optional slice of the runtime the heartbeat guardian
// needs beyond the base Runtime (autopilot.md §2.3, §7): the brain's liveness and
// context-window readings, the cheap "nudge" steering step, and owner-facing
// escalation notifications. The daemon runtime implements it; a bare Runtime that
// does not (the S1 inert core, the S3 lifecycle fakes) is simply never
// guardian-managed — RunGuardian is then a no-op. Keeping it separate from
// Runtime means those fakes need no update to keep compiling.
type GuardianRuntime interface {
	// BrainActivity returns the timestamp of the run's most recent brain-driven MCP
	// activity from the audit log — the heartbeat the guardian compares against
	// guardian.heartbeat_timeout. ok=false when nothing is recorded yet (a
	// freshly-spawned brain), in which case the guardian falls back to the brain's
	// spawn time so a cold-started brain is never mistaken for wedged.
	BrainActivity(ctx context.Context, runID string) (time.Time, bool)
	// BrainContextLevel returns the brain agent's context-window pressure level
	// ("" | ok | warning | critical), read from its session; "" when unknown. The
	// guardian triggers a planned rotation once it reaches guardian.rotate_at_context.
	BrainContextLevel(ctx context.Context, agentID string) string
	// NudgeBrain delivers a steering message to the brain's mailbox — the guardian's
	// cheapest heal step (§2.3 stage 1) before it restarts the brain.
	NudgeBrain(ctx context.Context, agentID, msg string) error
	// NotifyEscalation surfaces a guardian escalation to the owner through the
	// operator notifier (desktop/webhook), distinct from NotifyOwner's plan-edit
	// path. Used for the states an owner must see: a full-ladder stall and the
	// pay-per-use gate.
	NotifyEscalation(runID, title, body string)
}

// rotateBrain is the guardian's rotation hook (autopilot.md §7): terminate the
// current brain and spawn a fresh one on backend, cold-starting from the ledger
// via a freshly composed digest. S3 provides the signature and the mechanical
// terminate-then-respawn; S5 drives it from the heal ladder + tier selection.
// Workers in flight are untouched — the new brain re-adopts them from list_agents
// + tags.
func (c *Controller) rotateBrain(ctx context.Context, r *run, backend string) error {
	if c.runtime == nil {
		return nil
	}
	if r.brain != nil && r.brain.AgentID != "" {
		if err := c.runtime.TerminateBrain(ctx, r.brain.AgentID); err != nil {
			return fmt.Errorf("rotate: terminate current brain: %w", err)
		}
		r.brain = nil
	}
	return c.spawnBrain(ctx, r, backend)
}

// spawnBrain composes the recovery digest and launches the run's brain, recording
// the handle on r and advancing it to active. On a spawn failure the run is left
// degraded (brain nil) and the error is returned — Enable does not fail the whole
// switch for one degraded run (the guardian, S5, heals it). A nil runtime is the
// inert path: the run collapses straight to active with no brain (S1 behavior).
func (c *Controller) spawnBrain(ctx context.Context, r *run, backend string) error {
	if c.runtime == nil {
		r.state = StateActive
		return nil
	}
	prompt, err := ComposeDigest(ctx, DigestInput{
		RunID:    r.runID,
		Repo:     r.repo,
		PlanFile: r.absPlanFile,
		Plan:     r.plan,
		Ledger:   c.runtime.NewLedger(r.runID),
		Sources:  c.runtime.DigestSources(),
	})
	if err != nil {
		r.state = StateDegraded
		return fmt.Errorf("compose digest: %w", err)
	}
	handle, err := c.runtime.SpawnBrain(ctx, BrainSpec{
		RunID:    r.runID,
		Repo:     r.repo,
		PlanFile: r.absPlanFile,
		Backend:  backend,
		Prompt:   prompt,
		Tags:     []string{autopilotTag, runTag(r.runID)},
		Headless: true,
	})
	if err != nil {
		r.state = StateDegraded
		return fmt.Errorf("spawn brain: %w", err)
	}
	r.brain = &handle
	r.brainSpawnedAt = c.now() // fresh spawn counts as a heartbeat until the brain acts
	r.state = StateActive
	return nil
}

// teardownBrain gracefully terminates the run's brain (kill switch, autopilot.md
// §2.1). In-flight workers are deliberately left running. Best-effort: a
// terminate error is returned for logging but the run is dropped regardless.
func (c *Controller) teardownBrain(ctx context.Context, r *run) error {
	if c.runtime == nil || r.brain == nil || r.brain.AgentID == "" {
		return nil
	}
	err := c.runtime.TerminateBrain(ctx, r.brain.AgentID)
	r.brain = nil
	return err
}

// reloadPlanIfChanged re-reads the plan file when its mtime has advanced since
// the last load (owner steering mid-flight, autopilot.md §3). A mid-run edit that
// fails to validate NEVER stops the run: the run keeps its last-good plan and the
// error is returned so the caller notifies the owner. Returns changed=true only
// when a new, valid plan was adopted.
func (r *run) reloadPlanIfChanged() (changed bool, notify error) {
	info, err := os.Stat(r.absPlanFile)
	if err != nil {
		// The file vanished or is unreadable — keep the last-good plan and surface
		// it; a transient unlink during a save must not wedge the run.
		return false, fmt.Errorf("plan file %s: %v", r.absPlanFile, err)
	}
	mod := info.ModTime()
	if !mod.After(r.planModTime) {
		return false, nil
	}
	plan, err := LoadPlan(r.absPlanFile)
	if err != nil {
		// Advance the mtime so a persistently-corrupt file is reported once, not on
		// every tick; the run carries on with the last-good plan.
		r.planModTime = mod
		return false, err
	}
	r.plan = plan
	r.planModTime = mod
	return true, nil
}

// watchPlan polls the plan file for owner edits until ctx is cancelled (run
// disable). On a valid change the brain re-reads the file itself (persona rule 1);
// the daemon's job here is the §3 guarantee for an INVALID edit: keep last-good +
// notify the owner. Generous cadence (frictionless-safeguards philosophy) — plan
// edits are rare and human-paced.
func (c *Controller) watchPlan(ctx context.Context, r *run, interval time.Duration) {
	if interval <= 0 {
		interval = planWatchInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, notify := r.reloadPlanIfChanged()
			if notify != nil && c.runtime != nil {
				c.runtime.NotifyOwner(r.runID, "autopilot: plan edit ignored (kept last-good): "+notify.Error())
			}
		}
	}
}

// planWatchInterval is the default plan-file poll cadence. Plan edits are rare
// and human-paced, so a slow poll is plenty (frictionless-safeguards philosophy).
const planWatchInterval = 5 * time.Second
