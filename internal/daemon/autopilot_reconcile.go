package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
)

// managerSlotObservation is authenticated launch metadata obtained without
// changing the observed pane. Every field is required for adoption; keeping
// this seam read-only prevents a duplicate-session error from becoming an
// implicit permission to mutate an arbitrary tmux session.
type managerSlotObservation struct {
	TmuxSession    string
	RunID          string
	PlanID         string
	ProjectID      string
	Repo           string
	Workdir        string
	Role           string
	Backend        string
	AICLISessionID string
	LaunchID       string
}

type managerSlotInspector interface {
	InspectManagerSlot(context.Context, string) (managerSlotObservation, error)
}

var errManagerAdoptionRefused = errors.New("manager adoption refused")
var errManagerSlotMissing = errors.New("manager slot missing")

// ReconcileSessions migrates legacy autopilot sessions at daemon boot (WP12):
// adopt live agent-<hex> managers into slot ids and stamp worker back-ref fields
// from tags. Idempotent.
func (rt autopilotRuntime) ReconcileSessions(ctx context.Context, runs []autopilot.BootReconcileRun) error {
	if rt.s == nil || rt.s.store == nil {
		return nil
	}
	sessions, err := rt.s.store.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, spec := range runs {
		if _, err := rt.adoptVerifiedMissingManager(ctx, sessions, spec); err != nil && !errors.Is(err, errManagerAdoptionRefused) {
			errs = append(errs, err)
		}
		errs = append(errs, rt.migrateLegacyManager(ctx, sessions, spec))
		errs = append(errs, rt.reconcileWorkerBackRefs(ctx, sessions, spec))
	}
	rt.s.notify()
	return errors.Join(errs...)
}

// adoptVerifiedMissingManager restores only a record that was lost while its
// deterministic manager pane survived. It is intentionally all-or-nothing:
// evidence that is absent, stale, foreign, or ambiguous leaves the pane and
// all other sessions untouched. In particular it does not spawn, terminate,
// rename, or attach to tmux.
func (rt autopilotRuntime) adoptVerifiedMissingManager(ctx context.Context, sessions []*agentstore.Agent, spec autopilot.BootReconcileRun) (bool, error) {
	if rt.s.managerSlotInspector == nil || spec.ManagerSlotID == "" {
		return false, fmt.Errorf("%w: launch metadata verifier unavailable", errManagerAdoptionRefused)
	}
	if _, err := rt.s.store.Get(ctx, spec.ManagerSlotID); err == nil {
		return false, nil
	} else if !errors.Is(err, agentstore.ErrNotFound) {
		return false, err
	}

	// Bound an external metadata observation even if boot reconciliation was
	// called with a background context.
	verifyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	obs, err := rt.s.managerSlotInspector.InspectManagerSlot(verifyCtx, spec.ManagerSlotID)
	if err != nil {
		if errors.Is(err, errManagerSlotMissing) {
			return false, nil
		}
		slog.Warn("autopilot: manager adoption refused", "slot", spec.ManagerSlotID, "reason", "metadata unavailable", "err", err)
		return false, fmt.Errorf("%w: metadata unavailable: %v", errManagerAdoptionRefused, err)
	}
	if reason := verifyManagerSlotObservation(obs, spec, sessions); reason != "" {
		slog.Warn("autopilot: manager adoption refused", "slot", spec.ManagerSlotID, "reason", reason)
		return false, fmt.Errorf("%w: %s", errManagerAdoptionRefused, reason)
	}
	now := time.Now().UTC()
	sess := &agentstore.Agent{
		ID: obs.TmuxSession, Name: obs.TmuxSession, TmuxSession: obs.TmuxSession,
		AiCli: obs.Backend, AICLISessionID: obs.AICLISessionID,
		Repo: obs.Repo, Workdir: obs.Workdir, Role: autopilotBrainRole,
		PlanID: spec.PlanID, ProjectID: spec.ProjectID,
		AutopilotRunID: spec.RunID, AutopilotSlot: store.AutopilotSlotManager,
		Tags:   []string{"autopilot", "run:" + spec.RunID},
		Status: store.StatusWorking, CreatedAt: now, UpdatedAt: now,
		Runtime: &store.SessionRuntime{Kind: "tmux", LaunchID: obs.LaunchID},
	}
	if err := rt.s.store.Insert(ctx, sess); err != nil {
		if errors.Is(err, agentstore.ErrExists) { // another verifier won; never replace it.
			return false, nil
		}
		return false, fmt.Errorf("adopt verified manager slot %s: %w", spec.ManagerSlotID, err)
	}
	slog.Info("autopilot: adopted verified manager slot", "run", spec.RunID, "slot", spec.ManagerSlotID)
	return true, nil
}

func verifyManagerSlotObservation(obs managerSlotObservation, spec autopilot.BootReconcileRun, sessions []*agentstore.Agent) string {
	if obs.TmuxSession != spec.ManagerSlotID {
		return "tmux slot identity mismatch"
	}
	if obs.RunID != spec.RunID {
		return "foreign run ownership"
	}
	if obs.PlanID == "" || obs.PlanID != spec.PlanID {
		return "plan ownership missing or conflicting"
	}
	if obs.ProjectID == "" || obs.ProjectID != spec.ProjectID {
		return "project ownership missing or conflicting"
	}
	if obs.Repo == "" || obs.Repo != spec.Repo || obs.Workdir != spec.Repo {
		return "repository or workdir ownership missing or conflicting"
	}
	if obs.Role != autopilotBrainRole {
		return "manager role missing or conflicting"
	}
	if obs.Backend == "" || obs.LaunchID == "" {
		return "backend or session identity missing"
	}
	for _, s := range sessions {
		if s == nil || !guardianSessionLive(s.Status) {
			continue
		}
		if s.ID == spec.ManagerSlotID || s.TmuxSession == spec.ManagerSlotID {
			return "competing manager record"
		}
		if s.AutopilotRunID == spec.RunID && s.AutopilotSlot == store.AutopilotSlotManager {
			return "competing manager record for run"
		}
	}
	return ""
}

func (rt autopilotRuntime) migrateLegacyManager(ctx context.Context, sessions []*agentstore.Agent, spec autopilot.BootReconcileRun) error {
	slotID := spec.ManagerSlotID
	legacyID := spec.LegacyBrainID
	if legacyID == "" || legacyID == slotID {
		legacyID = findLegacyManagerID(sessions, spec.RunID)
	}
	if legacyID == "" || legacyID == slotID {
		return rt.stampManagerBackRefs(ctx, slotID, spec.RunID)
	}
	legacy, err := rt.s.store.Get(ctx, legacyID)
	if errors.Is(err, agentstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !rt.sessionAlive(ctx, legacy) {
		return rt.clearDeadSlotSession(ctx, legacy)
	}
	slot, slotErr := rt.s.store.Get(ctx, slotID)
	switch {
	case slotErr == nil && rt.sessionAlive(ctx, slot):
		return rt.clearDeadSlotSession(ctx, legacy)
	case slotErr == nil:
		_ = rt.s.store.Archive(ctx, slotID)
	}
	return rt.rekeyAutopilotSession(ctx, legacy, slotID, spec.RunID, store.AutopilotSlotManager)
}

func (rt autopilotRuntime) stampManagerBackRefs(ctx context.Context, slotID, runID string) error {
	sess, err := rt.s.store.Get(ctx, slotID)
	if errors.Is(err, agentstore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sess.AutopilotRunID == runID && sess.AutopilotSlot == store.AutopilotSlotManager {
		return nil
	}
	return rt.s.store.Update(ctx, slotID, func(s *agentstore.Agent) error {
		if s.AutopilotRunID == "" {
			s.AutopilotRunID = runID
		}
		if s.AutopilotSlot == "" {
			s.AutopilotSlot = store.AutopilotSlotManager
		}
		return nil
	})
}

func (rt autopilotRuntime) reconcileWorkerBackRefs(ctx context.Context, sessions []*agentstore.Agent, spec autopilot.BootReconcileRun) error {
	runTag := "run:" + spec.RunID
	var errs []error
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		if !sessionOwnsRun(sess, spec.RunID, runTag) {
			continue
		}
		if autopilot.IsManagerRecord(sess) {
			continue
		}
		if !autopilot.WorkerSpawnRole(sess.Role) && sess.AutopilotSlot != store.AutopilotSlotWorker {
			continue
		}
		parentDead := false
		if pid := strings.TrimSpace(sess.ParentID); pid != "" {
			parent, err := rt.s.store.Get(ctx, pid)
			if errors.Is(err, agentstore.ErrNotFound) || !guardianSessionLive(parent.Status) {
				parentDead = true
			}
		}
		needsUpdate := parentDead ||
			sess.AutopilotRunID == "" ||
			(autopilot.WorkerSpawnRole(sess.Role) && sess.AutopilotSlot != store.AutopilotSlotWorker)
		if !needsUpdate {
			continue
		}
		id := sess.ID
		errs = append(errs, rt.s.store.Update(ctx, id, func(s *agentstore.Agent) error {
			if s.AutopilotRunID == "" {
				s.AutopilotRunID = spec.RunID
			}
			if autopilot.WorkerSpawnRole(s.Role) && s.AutopilotSlot != store.AutopilotSlotWorker {
				s.AutopilotSlot = store.AutopilotSlotWorker
			}
			if s.AutopilotTaskID == "" {
				s.AutopilotTaskID = strings.TrimSpace(s.Task)
			}
			if parentDead {
				s.ParentID = ""
			}
			return nil
		}))
	}
	return errors.Join(errs...)
}

func sessionOwnsRun(sess *agentstore.Agent, runID, runTag string) bool {
	if autopilot.SessionRunID(sess) == runID {
		return true
	}
	return sess.HasTag(runTag) || sess.HasTag("autopilot-run:"+runID)
}

func findLegacyManagerID(sessions []*agentstore.Agent, runID string) string {
	runTag := "run:" + runID
	for _, sess := range sessions {
		if sess == nil || sess.ID == "" {
			continue
		}
		if !strings.HasPrefix(sess.ID, "agent-") {
			continue
		}
		if sess.Role == autopilotBrainRole && (sess.HasTag(runTag) || autopilot.SessionRunID(sess) == runID) {
			return sess.ID
		}
	}
	return ""
}

func (rt autopilotRuntime) sessionAlive(ctx context.Context, sess *agentstore.Agent) bool {
	if sess == nil || !guardianSessionLive(sess.Status) {
		return false
	}
	if rt.s.poller != nil && sess.TmuxSession != "" && !rt.s.poller.SessionAlive(ctx, sess.TmuxSession) {
		return false
	}
	return true
}

func (rt autopilotRuntime) rekeyAutopilotSession(ctx context.Context, old *agentstore.Agent, newID, runID, slot string) error {
	if old == nil || newID == "" || old.ID == newID {
		return nil
	}
	tmux := old.TmuxSession
	if tmux == "" {
		tmux = old.ID
	}
	now := time.Now().UTC()
	newSess := *old
	newSess.ID = newID
	newSess.Name = newID
	newSess.TmuxSession = tmux // keep the live tmux pane name; store id is the slot
	newSess.AutopilotRunID = runID
	newSess.AutopilotSlot = slot
	newSess.UpdatedAt = now
	if err := rt.s.store.Archive(ctx, old.ID); err != nil {
		return err
	}
	if err := rt.s.store.Insert(ctx, &newSess); err != nil {
		if errors.Is(err, agentstore.ErrExists) {
			return nil
		}
		return err
	}
	return nil
}
