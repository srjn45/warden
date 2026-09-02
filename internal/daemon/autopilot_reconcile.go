package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/store"
)

// ReconcileSessions migrates legacy autopilot sessions at daemon boot (WP12):
// adopt live agent-<hex> managers into slot ids, retire legacy guardian-<hash>
// sessions, and stamp worker back-ref fields from tags. Idempotent.
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
		errs = append(errs, rt.migrateLegacyManager(ctx, sessions, spec))
		errs = append(errs, rt.retireLegacyGuardian(ctx, sessions, spec))
		errs = append(errs, rt.reconcileWorkerBackRefs(ctx, sessions, spec))
	}
	rt.s.notify()
	return errors.Join(errs...)
}

func (rt autopilotRuntime) migrateLegacyManager(ctx context.Context, sessions []*store.Session, spec autopilot.BootReconcileRun) error {
	slotID := spec.ManagerSlotID
	legacyID := spec.LegacyBrainID
	if legacyID == "" || legacyID == slotID {
		legacyID = findLegacyManagerID(sessions, spec.RunID)
	}
	if legacyID == "" || legacyID == slotID {
		return rt.stampManagerBackRefs(ctx, slotID, spec.RunID)
	}
	legacy, err := rt.s.store.Get(ctx, legacyID)
	if errors.Is(err, store.ErrNotFound) {
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
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sess.AutopilotRunID == runID && sess.AutopilotSlot == store.AutopilotSlotManager {
		return nil
	}
	return rt.s.store.Update(ctx, slotID, func(s *store.Session) error {
		if s.AutopilotRunID == "" {
			s.AutopilotRunID = runID
		}
		if s.AutopilotSlot == "" {
			s.AutopilotSlot = store.AutopilotSlotManager
		}
		return nil
	})
}

func (rt autopilotRuntime) retireLegacyGuardian(ctx context.Context, sessions []*store.Session, spec autopilot.BootReconcileRun) error {
	var errs []error
	for _, sess := range sessions {
		if sess == nil || !containsTag(sess.Tags, guardianSystemTag) {
			continue
		}
		runID := guardianRunIDFromTags(sess.Tags)
		if runID != spec.RunID {
			continue
		}
		if sess.ID == spec.GuardianSlotID {
			continue
		}
		errs = append(errs, rt.TerminateGuardian(ctx, sess.ID))
	}
	return errors.Join(errs...)
}

func (rt autopilotRuntime) reconcileWorkerBackRefs(ctx context.Context, sessions []*store.Session, spec autopilot.BootReconcileRun) error {
	runTag := "run:" + spec.RunID
	var errs []error
	for _, sess := range sessions {
		if sess == nil || containsTag(sess.Tags, guardianSystemTag) {
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
			if errors.Is(err, store.ErrNotFound) || !guardianSessionLive(parent.Status) {
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
		errs = append(errs, rt.s.store.Update(ctx, id, func(s *store.Session) error {
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

func sessionOwnsRun(sess *store.Session, runID, runTag string) bool {
	if autopilot.SessionRunID(sess) == runID {
		return true
	}
	return sess.HasTag(runTag) || sess.HasTag("autopilot-run:"+runID)
}

func findLegacyManagerID(sessions []*store.Session, runID string) string {
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

func guardianRunIDFromTags(tags []string) string {
	for _, tag := range tags {
		if strings.HasPrefix(tag, guardianRunPrefix) {
			return strings.TrimPrefix(tag, guardianRunPrefix)
		}
	}
	return ""
}

func (rt autopilotRuntime) sessionAlive(ctx context.Context, sess *store.Session) bool {
	if sess == nil || !guardianSessionLive(sess.Status) {
		return false
	}
	if rt.s.poller != nil && sess.TmuxSession != "" && !rt.s.poller.SessionAlive(ctx, sess.TmuxSession) {
		return false
	}
	return true
}

func (rt autopilotRuntime) rekeyAutopilotSession(ctx context.Context, old *store.Session, newID, runID, slot string) error {
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
		if errors.Is(err, store.ErrExists) {
			return nil
		}
		return err
	}
	return nil
}
