package daemon

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/audit"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/ctxstore"
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
	_ autopilot.Runtime              = autopilotRuntime{}
	_ autopilot.GuardianRuntime      = autopilotRuntime{}
	_ autopilot.GuardianAgentRuntime = autopilotRuntime{}
	_ autopilot.OverwatchRuntime     = autopilotRuntime{}
	_ autopilot.DigestSources        = autopilotRuntime{}
)

const (
	guardianSystemTag = "system:true"
	guardianRunPrefix = "autopilot-run:"
)

// autopilotBrainRole is the built-in role the brain spawns under: it carries the
// full-auto persona + defaults (auto_approve, bypassPermissions, autopilot tag).
const autopilotBrainRole = "autopilot"

// brainTeardownTimeout bounds the rollback teardown of a half-spawned brain.
const brainTeardownTimeout = 30 * time.Second

// SpawnBrain launches a headless brain in the repo root (an orchestrator, not a
// worktree agent) with the recovery digest as its opening prompt, mirroring the
// spawn → insert → rollback-on-insert-failure flow the HTTP spawn handler uses.
func (rt autopilotRuntime) SpawnBrain(ctx context.Context, spec autopilot.BrainSpec) (autopilot.BrainHandle, error) {
	req := SpawnRequest{
		Cwd:     spec.Repo, // free-form (no type) ⇒ launch in the repo root
		Prompt:  spec.Prompt,
		Role:    autopilotBrainRole,
		Backend: spec.Backend,
		Tags:    spec.Tags,
	}
	if code, msg := rt.s.validateSpawnRequest(ctx, req); code != 0 {
		return autopilot.BrainHandle{}, errors.New(msg)
	}
	sess, err := rt.s.life.Spawn(ctx, req)
	if err != nil {
		return autopilot.BrainHandle{}, err
	}
	if err := rt.s.store.Insert(ctx, sess); err != nil {
		// Roll back the tmux session so a failed insert doesn't leak an untracked
		// brain (the same guard handleSpawn / fireScheduleAgent apply).
		tctx, cancel := context.WithTimeout(context.Background(), brainTeardownTimeout)
		defer cancel()
		if terr := rt.s.life.Teardown(tctx, sess); terr != nil {
			return autopilot.BrainHandle{}, errors.New(err.Error() + " (rollback also failed: " + terr.Error() + ")")
		}
		return autopilot.BrainHandle{}, err
	}
	rt.s.notify()
	return autopilot.BrainHandle{AgentID: sess.ID, Backend: sess.Backend}, nil
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

// SpawnGuardian creates a cheap terminal-backed system session representing the
// daemon's guardian loop. The loop itself remains daemon-owned; the session makes
// its lifecycle inspectable without consuming an LLM backend.
func (rt autopilotRuntime) SpawnGuardian(ctx context.Context, runID, repo string) (string, error) {
	id := "guardian-" + strings.TrimPrefix(runID, "ap-")
	now := time.Now().UTC()
	if _, err := rt.s.store.Get(ctx, id); err == nil {
		if err := rt.s.store.Update(ctx, id, func(sess *store.Session) error {
			sess.Status, sess.Tags, sess.Repo, sess.Workdir = store.StatusIdle,
				[]string{guardianSystemTag, guardianRunPrefix + runID}, repo, repo
			return nil
		}); err != nil {
			return "", err
		}
		rt.s.notify()
		return id, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	sess := &store.Session{ID: id, Name: id, Repo: repo, Workdir: repo,
		Status: store.StatusIdle, Tags: []string{guardianSystemTag, guardianRunPrefix + runID},
		CreatedAt: now, UpdatedAt: now}
	if err := rt.s.store.Insert(ctx, sess); err != nil {
		return "", err
	}
	rt.s.notify()
	return id, nil
}

func (rt autopilotRuntime) TerminateGuardian(ctx context.Context, agentID string) error {
	sess, err := rt.s.store.Get(ctx, agentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := rt.s.life.Terminate(ctx, sess.TmuxSession); err != nil {
		return err
	}
	if err := rt.s.store.UpdateStatus(ctx, sess.ID, store.StatusDone); err != nil {
		return err
	}
	rt.s.notify()
	return nil
}

// ReconcileGuardians terminates leaked system guardians whose run disappeared,
// became terminal, or now points at a different guardian id.
func (rt autopilotRuntime) ReconcileGuardians(ctx context.Context, valid map[string]string) ([]string, error) {
	sessions, err := rt.s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var errs []error
	seen := make(map[string]bool)
	for _, sess := range sessions {
		if !containsTag(sess.Tags, guardianSystemTag) {
			continue
		}
		runID := ""
		for _, tag := range sess.Tags {
			if strings.HasPrefix(tag, guardianRunPrefix) {
				runID = strings.TrimPrefix(tag, guardianRunPrefix)
				break
			}
		}
		if runID == "" {
			continue // system visibility is not, by itself, guardian ownership
		}
		if valid[runID] != sess.ID {
			errs = append(errs, rt.TerminateGuardian(ctx, sess.ID))
		} else {
			seen[runID] = true
		}
	}
	missing := make([]string, 0)
	for runID := range valid {
		if !seen[runID] {
			missing = append(missing, runID)
		}
	}
	sort.Strings(missing)
	return missing, errors.Join(errs...)
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
func (rt autopilotRuntime) NudgeBrain(_ context.Context, agentID, msg string) error {
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
