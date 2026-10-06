package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/ctxstore"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/mailbox"
	"github.com/srjn45/warden/internal/store"
)

// autopilotRuntime adapts the daemon Server to autopilot.Runtime (docs/specs/
// autopilot.md §4, §7): it spawns and tears down brains through the existing
// agent lifecycle, exposes the ctx-store-backed run ledger and the recovery
// digest's live sources, and routes owner notifications. Wired into the
// Controller by SetAutopilotController.
type autopilotRuntime struct{ s *Server }

// The daemon runtime must satisfy every optional Controller seam so a drift in
// any surface (guardian liveness §2.3, overwatch fleet-tending §2.4, the digest
// sources) is a build error, not a silent no-op.
var (
	_ autopilot.Runtime           = autopilotRuntime{}
	_ autopilot.GuardianRuntime   = autopilotRuntime{}
	_ autopilot.MigrationRuntime  = autopilotRuntime{}
	_ autopilot.OverwatchRuntime  = autopilotRuntime{}
	_ autopilot.DigestSources     = autopilotRuntime{}
	_ autopilot.EvidenceRuntime   = autopilotRuntime{}
	_ autopilot.CompletionRuntime = autopilotRuntime{}
	_ autopilot.RunAgentReaper    = autopilotRuntime{}
)

// autopilotBrainRole is the built-in role the brain spawns under: it carries the
// full-auto persona + defaults (auto_approve, bypassPermissions, autopilot tag).
const autopilotBrainRole = "autopilot"

// brainTeardownTimeout bounds the rollback teardown of a half-spawned brain.
const brainTeardownTimeout = 30 * time.Second

// SpawnBrain launches a headless brain in the repo root (an orchestrator, not a
// worktree agent) with the recovery digest as its opening prompt, mirroring the
// spawn → insert → rollback-on-insert-failure flow the HTTP spawn handler uses.
// The manager slot id is deterministic (<scope>-autopilot); a live session in
// that slot is adopted instead of spawning a rival agent-<hex>.
func (rt autopilotRuntime) SpawnBrain(ctx context.Context, spec autopilot.BrainSpec) (autopilot.BrainHandle, error) {
	if spec.SlotScope == "" {
		return autopilot.BrainHandle{}, errors.New("spawn brain: slot scope required")
	}
	slotID := autopilot.ManagerSlotID(spec.SlotScope)
	if existing, err := rt.s.store.Get(ctx, slotID); err == nil {
		if handle, ok := rt.adoptSlotSession(ctx, existing); ok {
			rt.refreshBrainSession(ctx, slotID, spec)
			return handle, nil
		}
		if err := rt.clearDeadSlotSession(ctx, existing); err != nil {
			return autopilot.BrainHandle{}, err
		}
	} else if !errors.Is(err, agentstore.ErrNotFound) {
		return autopilot.BrainHandle{}, err
	}
	req := SpawnRequest{
		Ticket:    slotID,
		Cwd:       spec.Repo,
		Prompt:    spec.Prompt,
		Role:      autopilotBrainRole,
		Backend:   spec.Backend,
		Tags:      spec.Tags,
		PlanID:    spec.PlanID,
		ProjectID: spec.ProjectID,
	}
	if code, msg := rt.s.validateSpawnRequest(ctx, req); code != 0 {
		return autopilot.BrainHandle{}, errors.New(msg)
	}
	rt.s.prepareSpawnName(ctx, &req)
	sess, err := rt.s.life.Spawn(ctx, req)
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	if err := rt.s.store.Insert(ctx, sess); err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), brainTeardownTimeout)
		defer cancel()
		if errors.Is(err, agentstore.ErrExists) {
			_ = rt.s.life.Teardown(tctx, sess)
			existing, gerr := rt.s.store.Get(ctx, slotID)
			if gerr != nil {
				return autopilot.BrainHandle{}, gerr
			}
			if handle, ok := rt.adoptSlotSession(ctx, existing); ok {
				rt.refreshBrainSession(ctx, slotID, spec)
				return handle, nil
			}
			return autopilot.BrainHandle{}, fmt.Errorf("slot %s exists but is not live", slotID)
		}
		if terr := rt.s.life.Teardown(tctx, sess); terr != nil {
			return autopilot.BrainHandle{}, errors.New(err.Error() + " (rollback also failed: " + terr.Error() + ")")
		}
		return autopilot.BrainHandle{}, err
	}
	rt.s.notify()
	return autopilot.BrainHandle{AgentID: sess.ID, Backend: sess.AiCli}, nil
}

// RotateBrain hot-swaps a successor backend into the existing manager session
// (WP5). It calls Lifecycle.HotSwap directly — never BackendRecoveryCoordinator —
// so guardian heal-ladder rotation cannot double-switch a session the recovery
// loop owns. The session id is preserved; the handoff lands at the stable
// `.warden/handoff-<id>.md` path HotSwap already writes.
func (rt autopilotRuntime) RotateBrain(ctx context.Context, spec autopilot.RotateBrainSpec) (autopilot.BrainHandle, error) {
	if rt.s == nil || rt.s.store == nil || rt.s.life == nil {
		return autopilot.BrainHandle{}, errors.New("rotate brain: runtime not configured")
	}
	sess, err := rt.s.store.Get(ctx, spec.AgentID)
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	backend := spec.Backend
	if backend == "" {
		backend = sess.AiCli
	}
	if backend == "" {
		backend = agentbackend.DefaultID
	}
	reason := lifecycle.SwapReason(spec.Reason)
	if reason == "" {
		reason = lifecycle.SwapReasonManual
	}
	res, err := rt.s.life.HotSwap(ctx, sess, lifecycle.SwapRequest{
		Backend: backend,
		Reason:  reason,
		Prompt:  spec.Prompt,
	})
	if err != nil {
		return autopilot.BrainHandle{}, fmt.Errorf("hot-swap brain: %w", err)
	}
	// Manager/brain handoff prose → attributed notes only (never factual events).
	if res != nil {
		rt.s.recordPlanBoundHandoffNote(sess, res.Handoff, res.HandoffPath)
	}
	toBackend, toModel := backend, sess.Model
	if res != nil {
		if res.ToBackend != "" {
			toBackend = res.ToBackend
		}
		if res.ToModel != "" {
			toModel = res.ToModel
		}
		if res.Agent != nil {
			sess = res.Agent
		}
	}
	// Persist even when the lifecycle implementation already wrote (adapter): a
	// second Update is idempotent and covers fake/raw Lifecycle doubles used in tests.
	if err := rt.s.store.Update(ctx, sess.ID, func(s *agentstore.Agent) error {
		s.AiCli = toBackend
		s.Model = toModel
		if res != nil && res.Agent != nil {
			s.AICLISessionID = res.Agent.AICLISessionID
			s.UpdatedAt = res.Agent.UpdatedAt
		}
		return nil
	}); err != nil {
		return autopilot.BrainHandle{}, fmt.Errorf("persist rotated brain: %w", err)
	}
	rt.s.notify()
	return autopilot.BrainHandle{AgentID: sess.ID, Backend: toBackend}, nil
}

// TerminateBrain gracefully stops the brain's tmux session (record + worktree
// kept). In-flight workers are untouched — they are separate agents.
func (rt autopilotRuntime) TerminateBrain(ctx context.Context, agentID string) error {
	sess, err := rt.s.store.Get(ctx, agentID)
	if err != nil {
		return err
	}
	if err := rt.s.life.Terminate(ctx, sess.TmuxSession); err != nil {
		return err
	}
	rt.s.notify()
	return nil
}

func guardianSessionLive(status store.Status) bool {
	switch status {
	case store.StatusSpawning, store.StatusWorking, store.StatusWaitingForInput, store.StatusIdle, store.StatusRateLimited:
		return true
	default:
		return false
	}
}

// adoptSlotSession returns a handle when sess is a live manager slot session.
func (rt autopilotRuntime) adoptSlotSession(ctx context.Context, sess *agentstore.Agent) (autopilot.BrainHandle, bool) {
	if sess == nil || !guardianSessionLive(sess.Status) {
		return autopilot.BrainHandle{}, false
	}
	if rt.s.poller != nil && sess.TmuxSession != "" && !rt.s.poller.SessionAlive(ctx, sess.TmuxSession) {
		return autopilot.BrainHandle{}, false
	}
	return autopilot.BrainHandle{AgentID: sess.ID, Backend: sess.AiCli}, true
}

func (rt autopilotRuntime) refreshBrainSession(ctx context.Context, id string, spec autopilot.BrainSpec) {
	if err := rt.s.store.Update(ctx, id, func(sess *agentstore.Agent) error {
		sess.Tags = spec.Tags
		sess.Repo = spec.Repo
		sess.Workdir = spec.Repo
		return nil
	}); err != nil {
		slog.Warn("autopilot: refresh adopted brain session failed", "agent", id, "err", err)
		return
	}
	rt.s.notify()
}

func (rt autopilotRuntime) clearDeadSlotSession(ctx context.Context, sess *agentstore.Agent) error {
	if sess == nil {
		return nil
	}
	if sess.TmuxSession != "" {
		tctx, cancel := context.WithTimeout(ctx, brainTeardownTimeout)
		defer cancel()
		_ = rt.s.life.Terminate(tctx, sess.TmuxSession)
	}
	return rt.s.store.Archive(ctx, sess.ID)
}

// NewLedger returns a run-scoped ledger over the shared-context blackboard, or nil
// when no ctx store is wired (a bare Server literal) — ComposeDigest tolerates a
// nil ledger.
func (rt autopilotRuntime) NewLedger(runID string) *autopilot.Ledger {
	if rt.s.cstore == nil {
		return nil
	}
	return autopilot.NewLedger(ctxLedgerStore{cs: rt.s.cstore}, runID)
}

// DigestSources returns the live-agent + audit sources for the recovery digest.
func (rt autopilotRuntime) DigestSources() autopilot.DigestSources { return rt }

// RunAgents projects the session store to the agents tagged for runID (the live
// list_agents filtered to `run:<run_id>`).
func (rt autopilotRuntime) RunAgents(ctx context.Context, runID string) ([]autopilot.AgentInfo, error) {
	sessions, err := rt.s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	tag := "run:" + runID
	out := []autopilot.AgentInfo{}
	for _, sess := range sessions {
		if !containsTag(sess.Tags, tag) {
			continue
		}
		out = append(out, autopilot.AgentInfo{
			ID:     sess.ID,
			Name:   sess.Name,
			Role:   sess.Role,
			State:  string(sess.Status),
			Branch: sess.Branch,
			Tags:   sess.Tags,

			SeedStatus: sess.SeedStatus,
		})
	}
	return out, nil
}

// RecentAudit returns the run's most recent audit entries, newest-first: entries
// targeting one of the run's agents or the autopilot switch itself. Best-effort —
// a read error only degrades the digest's observability.
func (rt autopilotRuntime) RecentAudit(ctx context.Context, runID string, limit int) ([]autopilot.AuditEntry, error) {
	path := rt.s.audit.Path()
	if path == "" {
		return nil, nil
	}
	events, err := audit.Read(path, audit.Filter{})
	if err != nil {
		return nil, err
	}
	agents, _ := rt.RunAgents(ctx, runID)
	ids := map[string]bool{}
	for _, a := range agents {
		ids[a.ID] = true
	}
	out := []autopilot.AuditEntry{}
	for i := len(events) - 1; i >= 0; i-- { // audit.Read is oldest-first; reverse
		e := events[i]
		if !ids[e.Target] && !strings.HasPrefix(e.Action, "autopilot") {
			continue
		}
		out = append(out, autopilot.AuditEntry{
			Time:   e.Time.UTC().Format(time.RFC3339),
			Action: e.Action,
			Target: e.Target,
			Detail: flattenDetail(e.Detail),
		})
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// NotifyOwner surfaces an owner-facing message. S3 routes it to the daemon log
// (the one §3-permitted notification: a mid-run plan edit that failed to
// validate); richer notification channels can hook in later.
func (rt autopilotRuntime) NotifyOwner(runID, msg string) {
	slog.Warn("autopilot: owner notification", "run", runID, "msg", msg)
}

// guardianNudgeSender stamps the guardian's steering nudge. It reuses the trusted
// daemon-origin provenance (agents cannot forge it), matching the approval router.
const guardianNudgeSender = autopilotForwardSender

// BrainActivity returns the timestamp of the run's most recent audit entry — the
// heartbeat the guardian compares against guardian.heartbeat_timeout (§2.3). It
// reuses RecentAudit's run-scoping (entries targeting a run agent or the autopilot
// switch), so any MCP progress the brain drives (spawning workers, landing,
// switch events) counts as a heartbeat. ok=false when nothing is recorded yet,
// letting the guardian fall back to the brain's spawn time.
func (rt autopilotRuntime) BrainActivity(ctx context.Context, runID string) (time.Time, bool) {
	entries, err := rt.RecentAudit(ctx, runID, 1)
	if err != nil || len(entries) == 0 {
		return time.Time{}, false
	}
	t, perr := time.Parse(time.RFC3339, entries[0].Time)
	if perr != nil {
		return time.Time{}, false
	}
	return t, true
}

// BrainContextLevel reads the brain agent's context-window pressure level from its
// session ("" | ok | warning | critical). "" when the session is unreadable or no
// model turn has set a level yet.
func (rt autopilotRuntime) BrainContextLevel(ctx context.Context, agentID string) string {
	sess, err := rt.s.store.Get(ctx, agentID)
	if err != nil || sess == nil {
		return ""
	}
	return sess.ContextState
}

// NudgeBrain delivers the guardian's steering message to the brain's mailbox — the
// cheapest heal step (§2.3 stage 1). Best-effort: a mailbox error is returned for
// logging but the guardian escalates on the next tick regardless.
func (rt autopilotRuntime) NudgeBrain(ctx context.Context, agentID, msg string) error {
	if _, err := rt.s.store.Get(ctx, agentID); errors.Is(err, agentstore.ErrNotFound) {
		return fmt.Errorf("%w: %s", autopilot.ErrAgentNotFound, agentID)
	}
	if rt.s.mbox == nil {
		return nil
	}
	_, err := rt.s.mbox.Append(mailbox.Message{To: agentID, From: guardianNudgeSender, Body: msg})
	return err
}

// WakeAgent delivers the overwatch nudge as a real input turn typed into the
// manager's pane (the same path send_to_agent / POST /sessions/{id}/input uses),
// which genuinely wakes an idle agent — the mailbox is pull-only, and an idle
// manager runs no loop that would ever read it (autopilot.md §2.4). On an
// injection failure the message falls back to the mailbox so the steer at least
// lands somewhere durable; the injection error is still returned for logging.
func (rt autopilotRuntime) WakeAgent(ctx context.Context, agentID, msg string) error {
	sess, err := rt.s.store.Get(ctx, agentID)
	if errors.Is(err, agentstore.ErrNotFound) {
		return fmt.Errorf("%w: %s", autopilot.ErrAgentNotFound, agentID)
	}
	if err != nil {
		return err
	}
	if err := rt.s.life.Input(ctx, sess.TmuxSession, msg); err != nil {
		if merr := rt.NudgeBrain(ctx, agentID, msg); merr != nil {
			return errors.New(err.Error() + " (mailbox fallback also failed: " + merr.Error() + ")")
		}
		return err
	}
	return nil
}

// tmuxGoneDebounce is how long a live-looking record must have lost its tmux
// session before the manager counts as lost: an out-of-band hot-swap kills and
// relaunches the session, and must not be mistaken for a death.
const tmuxGoneDebounce = 20 * time.Second

var brainTmuxGoneSince sync.Map // agent id -> time.Time first seen without tmux

// brainLiveness classifies the manager: "" = alive/unknown-safe, else the loss
// cause (missing | terminal | tmux_gone). known=false means never act on it.
// The agent store holds only active records, so ErrNotFound covers deleted and
// archived. Hot-swap rewrites the record in place; any other store error is
// unknown.
func (rt autopilotRuntime) brainLiveness(ctx context.Context, agentID string) (cause string, known bool) {
	if rt.s == nil || rt.s.store == nil {
		return "", false
	}
	sess, err := rt.s.store.Get(ctx, agentID)
	switch {
	case errors.Is(err, agentstore.ErrNotFound):
		return "missing", true
	case err != nil:
		return "", false
	}
	switch sess.Status {
	case store.StatusDone, store.StatusErrored, store.StatusOrphaned:
		return "terminal", true
	}
	if rt.s.poller != nil && sess.TmuxSession != "" && guardianSessionLive(sess.Status) {
		if rt.s.poller.SessionAlive(ctx, sess.TmuxSession) {
			brainTmuxGoneSince.Delete(agentID)
			return "", true
		}
		first, _ := brainTmuxGoneSince.LoadOrStore(agentID, time.Now())
		if time.Since(first.(time.Time)) >= tmuxGoneDebounce {
			return "tmux_gone", true
		}
	}
	return "", true
}

// BrainSession reports whether the manager is lost: record missing, terminal
// status (terminate_agent, done, errored, orphaned) or tmux session gone.
func (rt autopilotRuntime) BrainSession(ctx context.Context, agentID string) autopilot.SessionPresence {
	cause, known := rt.brainLiveness(ctx, agentID)
	switch {
	case !known:
		return autopilot.SessionUnknown
	case cause != "":
		return autopilot.SessionMissing
	}
	return autopilot.SessionPresent
}

// BrainLossCause names why BrainSession reported the manager missing.
func (rt autopilotRuntime) BrainLossCause(ctx context.Context, agentID string) string {
	cause, _ := rt.brainLiveness(ctx, agentID)
	return cause
}

// AuditRunEvent writes a guardian event to the audit log, targeting the manager.
func (rt autopilotRuntime) AuditRunEvent(ctx context.Context, runID, action, agentID, detail string) {
	rt.s.recordAuditCtx(ctx, action, agentID, map[string]string{"run": runID, "detail": detail})
}

// NotifyEscalation surfaces a guardian escalation through the operator notifier
// (desktop/webhook) when one is wired, always logging as a fallback.
func (rt autopilotRuntime) NotifyEscalation(runID, title, body string) {
	slog.Warn("autopilot guardian escalation", "run", runID, "title", title, "body", body)
	if rt.s.apNotifier != nil {
		rt.s.apNotifier.Notify(title, body+" (run "+runID+")")
	}
}

// InstallDefaultAutoApprovePolicy installs the generous default auto-approve
// policy (autopilot.md §10) when — and only when — the owner has configured no
// rules of their own, so day-one autopilot workers don't stall on recognized
// non-destructive prompts. The default enables auto-approve and allows every
// non-destructive recognized prompt (an empty allow rule matches all; the
// destructive guard still blocks irreversible actions unconditionally upstream,
// and anything the policy still can't answer routes to the brain per §8). It
// preserves the owner's other settings (per-agent overrides, max_repeats). A
// no-op once any rules exist, so it never clobbers a configured policy and a
// re-enable is idempotent. Best-effort persistence mirrors the PUT
// /auto-approve/policy handler.
func (rt autopilotRuntime) InstallDefaultAutoApprovePolicy() {
	if rt.s.poller == nil {
		return
	}
	cur := rt.s.poller.AutoApprovePolicySnapshot()
	if cur.HasRules() {
		return // owner configured rules — respect them, install nothing
	}
	pol := cur
	pol.Enabled = true
	pol.Rules = approval.Rules{Allow: []approval.Rule{{}}}
	rt.s.poller.SetAutoApprovePolicy(pol)
	if rt.s.autoApprovePersist != nil {
		if err := rt.s.autoApprovePersist(pol); err != nil {
			slog.Warn("autopilot: persist default auto-approve policy failed", "err", err)
		}
	}
	slog.Info("autopilot: installed generous default auto-approve policy (owner had none configured)")
}

// ctxLedgerStore adapts *ctxstore.Store to autopilot.CtxStore, translating the
// store's sentinel errors into the ledger's (ErrNotFound → ErrLedgerMissing,
// ErrConflict → ErrLedgerConflict) so the autopilot package stays decoupled.
type ctxLedgerStore struct{ cs *ctxstore.Store }

func (l ctxLedgerStore) Get(key string) (string, error) {
	e, err := l.cs.Get(key)
	if errors.Is(err, ctxstore.ErrNotFound) {
		return "", autopilot.ErrLedgerMissing
	}
	if err != nil {
		return "", err
	}
	return e.Value, nil
}

func (l ctxLedgerStore) Set(key, value, by string) error {
	_, err := l.cs.Set(key, value, by)
	return err
}

func (l ctxLedgerStore) CompareAndSet(key, expected, value, by string) error {
	_, err := l.cs.CompareAndSet(key, expected, value, by)
	if errors.Is(err, ctxstore.ErrConflict) {
		return autopilot.ErrLedgerConflict
	}
	return err
}

// containsTag reports whether tags contains tag.
func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// flattenDetail renders an audit event's detail map as a stable "k=v" list.
func flattenDetail(d map[string]string) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+d[k])
	}
	return strings.Join(parts, ",")
}

// SpawnConsultBrain launches an on-demand role=brain Agent (headless / system:true)
// for consultation. Distinct from SpawnBrain which creates the role=autopilot manager.
func (rt autopilotRuntime) SpawnConsultBrain(ctx context.Context, spec autopilot.ConsultBrainRuntimeSpec) (autopilot.BrainHandle, error) {
	req := SpawnRequest{
		Cwd:     spec.Repo,
		Repo:    spec.Repo,
		Prompt:  spec.Prompt,
		Role:    "brain",
		Backend: spec.Backend,
		Tags:    spec.Tags,
		PlanID:  spec.PlanID,
	}
	if code, msg := rt.s.validateSpawnRequest(ctx, req); code != 0 {
		return autopilot.BrainHandle{}, errors.New(msg)
	}
	rt.s.prepareSpawnName(ctx, &req)
	sess, err := rt.s.life.Spawn(ctx, req)
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	if err := rt.s.store.Insert(ctx, sess); err != nil {
		tctx, cancel := context.WithTimeout(context.Background(), brainTeardownTimeout)
		defer cancel()
		_ = rt.s.life.Teardown(tctx, sess)
		return autopilot.BrainHandle{}, err
	}
	rt.s.notify()
	return autopilot.BrainHandle{AgentID: sess.ID, Backend: sess.AiCli}, nil
}

var _ autopilot.ConsultBrainRuntime = autopilotRuntime{}

// evidencePaneLines is the pane tail AgentEvidence captures (get_agent_output's
// default depth).
const evidencePaneLines = 200

// promptRedeliverer is the optional Lifecycle extension that re-runs prompt-seed
// delivery; the production adapter implements it, test fakes need not.
type promptRedeliverer interface {
	RedeliverPrompt(agent *agentstore.Agent) bool
}

func (rt autopilotRuntime) evidenceSession(ctx context.Context, agentID string) (*agentstore.Agent, error) {
	sess, err := rt.s.store.Get(ctx, agentID)
	if errors.Is(err, agentstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", autopilot.ErrAgentNotFound, agentID)
	}
	return sess, err
}

// AgentEvidence reads the agent's status, context level and pane tail (the same
// Output capture GET /sessions/{id}/output serves), and summarizes any approval
// prompt the pane shows through the agent's backend parser.
func (rt autopilotRuntime) AgentEvidence(ctx context.Context, agentID string) (autopilot.AgentEvidence, error) {
	sess, err := rt.evidenceSession(ctx, agentID)
	if err != nil {
		return autopilot.AgentEvidence{}, err
	}
	ev := autopilot.AgentEvidence{
		AgentID:      sess.ID,
		Status:       string(sess.Status),
		RateLimited:  sess.Status == store.StatusRateLimited,
		ContextLevel: sess.ContextState,
	}
	if pane, err := rt.s.life.Output(ctx, sess.TmuxSession, evidencePaneLines); err == nil {
		ev.PaneTail = pane
		if v := rt.s.approvalView(sess, pane); v.Recognized {
			ev.PendingApproval = v.Action + ": " + v.Question
		}
	}
	return ev, nil
}

// ResolvePrompt delegates to the poller's approval path.
func (rt autopilotRuntime) ResolvePrompt(ctx context.Context, agentID string) error {
	sess, err := rt.evidenceSession(ctx, agentID)
	if err != nil {
		return err
	}
	if rt.s.poller == nil {
		return errors.New("prompt resolution unavailable: no poller")
	}
	return rt.s.poller.ResolvePrompt(ctx, sess)
}

// ResumeRateLimit delegates to the rate-limit scheduler's resume attempt.
func (rt autopilotRuntime) ResumeRateLimit(ctx context.Context, agentID string) error {
	if _, err := rt.evidenceSession(ctx, agentID); err != nil {
		return err
	}
	if rt.s.rateLimitScheduler == nil {
		return errors.New("rate-limit resume unavailable: no scheduler")
	}
	rt.s.rateLimitScheduler.ResumeNow(agentID)
	return nil
}

// RedeliverPrompt re-runs prompt-seed delivery. A backend that seeds on its launch
// line has no typed seed to redo, so the prompt is typed in directly — the final
// step seeding itself performs.
func (rt autopilotRuntime) RedeliverPrompt(ctx context.Context, agentID string) error {
	sess, err := rt.evidenceSession(ctx, agentID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(sess.Prompt) == "" {
		return errors.New("agent has no recorded prompt to redeliver")
	}
	if r, ok := rt.s.life.(promptRedeliverer); ok && r.RedeliverPrompt(sess) {
		return nil
	}
	return rt.s.life.Input(ctx, sess.TmuxSession, sess.Prompt)
}

// SwitchLimited moves a rate-limited run agent onto another selectable backend by
// handing it to the BackendRecoveryCoordinator (the one hot-swap / usage-recovery
// path; autopilot adds the trigger, not a second swap). With no alternate it
// reports ErrNoAlternateBackend and the agent's recorded reset time so the
// guardian can schedule a timed resume.
func (rt autopilotRuntime) SwitchLimited(ctx context.Context, agentID string) (autopilot.LimitSwitch, error) {
	sess, err := rt.evidenceSession(ctx, agentID)
	if err != nil {
		return autopilot.LimitSwitch{}, err
	}
	if rt.s.recovery == nil {
		return autopilot.LimitSwitch{}, errors.New("backend recovery unavailable")
	}
	var reset time.Time
	if sess.RateLimitRestoreAt != nil {
		reset = *sess.RateLimitRestoreAt
	}
	selected, _ := rt.s.recovery.PreviewCandidates(ctx, sess)
	if selected == nil {
		return autopilot.LimitSwitch{From: sess.AiCli, Reset: reset}, autopilot.ErrNoAlternateBackend
	}
	fallbackAt := reset
	if fallbackAt.IsZero() {
		fallbackAt = time.Now().Add(30 * time.Minute)
	}
	rt.s.recovery.OnHardLimit(sess, fallbackAt)
	return autopilot.LimitSwitch{From: sess.AiCli, To: selected.BackendID, Reset: reset}, nil
}

var _ autopilot.LimitRuntime = autopilotRuntime{}

// SeedState reports an agent's initial-prompt seed status (autopilot.SeedRuntime).
func (rt autopilotRuntime) SeedState(ctx context.Context, agentID string) (string, bool) {
	if rt.s == nil || rt.s.store == nil {
		return "", false
	}
	sess, err := rt.s.store.Get(ctx, agentID)
	if err != nil {
		return "", false
	}
	return sess.SeedStatus, true
}

// RedeliverSeed types text into the agent's pane and, on success, marks its seed
// delivered (autopilot.SeedRuntime).
func (rt autopilotRuntime) RedeliverSeed(ctx context.Context, agentID, text string) error {
	sess, err := rt.s.store.Get(ctx, agentID)
	if errors.Is(err, agentstore.ErrNotFound) {
		return fmt.Errorf("%w: %s", autopilot.ErrAgentNotFound, agentID)
	}
	if err != nil {
		return err
	}
	if err := rt.s.life.Input(ctx, sess.TmuxSession, text); err != nil {
		return err
	}
	return rt.s.store.Update(ctx, agentID, func(a *agentstore.Agent) error {
		a.SeedStatus, a.SeedError = store.SeedDelivered, ""
		return nil
	})
}
