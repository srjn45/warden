package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/lifecycle"
	"github.com/srjn45/warden/internal/recovery"
	"github.com/srjn45/warden/internal/store"
)

const (
	recoveryRefreshing  = "refreshing_usage"
	recoverySwitching   = "switching"
	recoveryStabilizing = "stabilizing"
	recoveryWaiting     = "waiting_for_capacity"
)

// BackendRecoveryCoordinator is the sole owner of automatic hard-limit
// switching. State is persisted on the session; the maps below contain only
// process-local serialization and reconstructable timers.
type BackendRecoveryCoordinator struct {
	store    store.Store
	backends *backendstore.Store
	usage    *backendusage.Service
	life     backendRecoveryLife
	now      func() time.Time
	// notifyFn is called after each phase-transition event to wake SSE subscribers.
	// May be nil (tests that don't need SSE leave it unset).
	notifyFn func()

	mu                  sync.Mutex
	locks               map[string]*sync.Mutex
	timers              map[string]*time.Timer
	stabilizationWindow time.Duration
}

type backendRecoveryLife interface {
	Restore(context.Context, *store.Session) error
	SendKeys(context.Context, string, string) error
	HotSwap(context.Context, *store.Session, lifecycle.SwapRequest) (*lifecycle.SwapResult, error)
}

func NewBackendRecoveryCoordinator(st store.Store, backends *backendstore.Store, usage *backendusage.Service, life backendRecoveryLife) *BackendRecoveryCoordinator {
	return &BackendRecoveryCoordinator{store: st, backends: backends, usage: usage, life: life, now: time.Now, locks: make(map[string]*sync.Mutex), timers: make(map[string]*time.Timer), stabilizationWindow: 10 * time.Second}
}

// SetNotify wires an SSE publish callback. It is called once per recovery
// phase transition so remote clients (web, Android, MCP) see state changes
// without polling. Call before the coordinator handles any hard-limit events.
func (c *BackendRecoveryCoordinator) SetNotify(fn func()) { c.notifyFn = fn }

// WithStabilizationWindow overrides the stabilization observation window (how
// long an agent must stay in a live non-rate-limited status before the candidate
// is declared stable and recovery clears). Defaults to 10s when not set.
func (c *BackendRecoveryCoordinator) WithStabilizationWindow(d time.Duration) *BackendRecoveryCoordinator {
	if d > 0 {
		c.stabilizationWindow = d
	}
	return c
}

func (c *BackendRecoveryCoordinator) sessionLock(id string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[id] == nil {
		c.locks[id] = &sync.Mutex{}
	}
	return c.locks[id]
}

// OnHardLimit claims or advances one recovery generation. Returning true tells
// RateLimitScheduler that this coordinator owns switching/waiting and its legacy
// resume timer must not also run.
func (c *BackendRecoveryCoordinator) OnHardLimit(sess *store.Session, fallbackAt time.Time) bool {
	if c == nil || c.store == nil || c.backends == nil || c.usage == nil || c.life == nil || sess == nil {
		return false
	}
	settings, err := c.backends.GetHandoverSettings()
	if err == nil && !settings.Enabled {
		return false
	}
	lock := c.sessionLock(sess.ID)
	lock.Lock()
	defer lock.Unlock()
	current, err := c.store.Get(context.Background(), sess.ID)
	if err != nil {
		return false
	}
	now := c.now().UTC()
	generation := current.BackendRecoveryGeneration + 1
	if current.BackendRecovery != nil {
		generation = current.BackendRecovery.Generation
		if current.BackendRecovery.Phase == recoveryStabilizing && current.BackendRecovery.Current != nil {
			attempt := store.RecoveryAttempt{Candidate: *current.BackendRecovery.Current, Round: current.BackendRecovery.Round, StartedAt: now, Outcome: "immediate_hard_limit"}
			_ = c.store.Update(context.Background(), current.ID, func(s *store.Session) error {
				if s.BackendRecovery == nil || s.BackendRecovery.Generation != generation {
					return nil
				}
				s.BackendRecovery.Attempts = append(s.BackendRecovery.Attempts, attempt)
				s.BackendRecovery.Phase = recoveryRefreshing
				s.BackendRecovery.Current = nil
				s.BackendRecovery.UpdatedAt = now
				return nil
			})
			c.event(current.ID, "backend_recovery_attempt_failed", fmt.Sprintf("generation=%d candidate=%s/%s outcome=immediate_hard_limit", generation, attempt.Candidate.BackendID, attempt.Candidate.ModelID))
			go c.advance(current.ID, generation, fallbackAt)
			return true
		}
		// Duplicate transition while this generation is already refreshing,
		// switching, or waiting: it is already owned.
		return true
	}
	original := store.BackendCandidate{BackendID: current.Backend, ModelID: current.Model}
	err = c.store.Update(context.Background(), current.ID, func(s *store.Session) error {
		s.BackendRecoveryGeneration = generation
		s.BackendRecovery = &store.BackendRecovery{
			Generation: generation, Phase: recoveryRefreshing, Original: original,
			Attempts: []store.RecoveryAttempt{{Candidate: original, Round: 0, StartedAt: now, Outcome: "hard_limit"}}, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return false
	}
	_ = c.store.SetRateLimit(context.Background(), current.ID, fallbackAt, 0)
	c.event(current.ID, "backend_recovery_started", fmt.Sprintf("generation=%d limited=%s/%s", generation, original.BackendID, original.ModelID))
	go c.advance(current.ID, generation, fallbackAt)
	return true
}

func (c *BackendRecoveryCoordinator) advance(id string, generation uint64, fallbackAt time.Time) {
	lock := c.sessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	ctx := context.Background()
	sess, err := c.store.Get(ctx, id)
	if err != nil || sess.BackendRecovery == nil || sess.BackendRecovery.Generation != generation {
		return
	}
	snap, snapErr := c.usage.Snapshot(ctx, true)
	if snapErr != nil {
		snap = backendusage.Snapshot{}
	} // unknown remains trial-eligible
	c.event(id, "backend_usage_refreshed", fmt.Sprintf("generation=%d status=%s", generation, errorClass(snapErr)))

	candidates := c.policyCandidates(sess)
	ranked := recovery.Rank(candidates, snap)
	attempted := make(map[string]bool, len(sess.BackendRecovery.Attempts))
	for _, a := range sess.BackendRecovery.Attempts {
		if a.Round == sess.BackendRecovery.Round {
			attempted[candidateKey(a.Candidate.BackendID, a.Candidate.ModelID)] = true
		}
	}
	// Persisted active recoveries are the shared exact-pool limit registry. This
	// avoids broad backend cooldowns while preventing another agent from racing
	// into a backend/model pool that has just confirmed a hard limit.
	if sessions, listErr := c.store.List(ctx); listErr == nil {
		for _, other := range sessions {
			if other.ID == id || other.BackendRecovery == nil {
				continue
			}
			for _, attempt := range other.BackendRecovery.Attempts {
				if attempt.Outcome == "hard_limit" || attempt.Outcome == "immediate_hard_limit" {
					pool := attempt.Candidate
					attempted[candidateKey(pool.BackendID, pool.ModelID)] = true
				}
			}
		}
	}
	var selected *recovery.Candidate
	var resets []store.RecoveryReset
	for i := range ranked {
		for _, reset := range ranked[i].Resets {
			resets = append(resets, store.RecoveryReset{BackendID: ranked[i].BackendID, LimitID: reset.LimitID, Scope: reset.Scope, ResetsAt: reset.ResetsAt})
		}
		if selected == nil && !attempted[candidateKey(ranked[i].BackendID, ranked[i].ModelID)] && (ranked[i].Headroom == nil || *ranked[i].Headroom > 0) {
			selected = &ranked[i]
		}
	}
	if selected == nil {
		c.waitLocked(sess, generation, resets, fallbackAt)
		return
	}
	target := store.BackendCandidate{BackendID: selected.BackendID, ModelID: selected.ModelID}
	now := c.now().UTC()
	_ = c.store.Update(ctx, id, func(s *store.Session) error {
		if s.BackendRecovery == nil || s.BackendRecovery.Generation != generation {
			return nil
		}
		s.BackendRecovery.Phase = recoverySwitching
		s.BackendRecovery.Current = &target
		s.BackendRecovery.Resets = resets
		s.BackendRecovery.UpdatedAt = now
		return nil
	})
	c.event(id, "backend_recovery_candidate_selected", fmt.Sprintf("generation=%d candidate=%s/%s", generation, target.BackendID, target.ModelID))

	// Only the exact original pool resumes in place. Every other model/backend
	// uses the existing handoff lifecycle.
	if target == sess.BackendRecovery.Original {
		err = c.life.Restore(ctx, sess)
		if errors.Is(err, lifecycle.ErrAlreadyRunning) {
			err = c.life.SendKeys(ctx, sess.TmuxSession, "Enter")
		}
	} else {
		_, err = c.life.HotSwap(ctx, sess, lifecycle.SwapRequest{Backend: target.BackendID, Model: target.ModelID, Role: sess.Role, Reason: lifecycle.SwapReasonQuota})
	}
	if err != nil {
		attempt := store.RecoveryAttempt{Candidate: target, Round: sess.BackendRecovery.Round, StartedAt: now, Outcome: "launch_failed"}
		_ = c.store.Update(ctx, id, func(s *store.Session) error {
			if s.BackendRecovery == nil || s.BackendRecovery.Generation != generation {
				return nil
			}
			s.BackendRecovery.Attempts = append(s.BackendRecovery.Attempts, attempt)
			s.BackendRecovery.Phase = recoveryRefreshing
			s.BackendRecovery.Current = nil
			return nil
		})
		c.event(id, "backend_recovery_attempt_failed", fmt.Sprintf("generation=%d candidate=%s/%s outcome=launch_failed", generation, target.BackendID, target.ModelID))
		go c.advance(id, generation, fallbackAt)
		return
	}
	_ = c.store.Update(ctx, id, func(s *store.Session) error {
		if s.BackendRecovery == nil || s.BackendRecovery.Generation != generation {
			return nil
		}
		s.BackendRecovery.Phase = recoveryStabilizing
		s.BackendRecovery.StableSince = nil
		s.BackendRecovery.UpdatedAt = c.now().UTC()
		return nil
	})
	_, _ = c.store.UpdateStatusIf(ctx, id, store.StatusRateLimited, store.StatusSpawning)
	c.event(id, "backend_recovery_stabilizing", fmt.Sprintf("generation=%d candidate=%s/%s", generation, target.BackendID, target.ModelID))
}

func (c *BackendRecoveryCoordinator) policyCandidates(sess *store.Session) []recovery.Candidate {
	tier := backendstore.Tier2
	if sess.Role != "" {
		if t, err := c.backends.GetRoleTier(sess.Role); err == nil && t.Valid() {
			tier = t
		}
	}
	tiers := []backendstore.ModelTier{tier}
	switch tier {
	case backendstore.Tier1:
		tiers = append(tiers, backendstore.Tier2, backendstore.Tier3)
	case backendstore.Tier2:
		tiers = append(tiers, backendstore.Tier3)
	}
	tierPriority := make(map[backendstore.ModelTier]int, len(tiers))
	var models []backendstore.ModelEntry
	for priority, candidateTier := range tiers {
		tierPriority[candidateTier] = priority
		rows, _ := c.backends.ListModels(candidateTier)
		models = append(models, rows...)
	}
	backends, _ := c.backends.List()
	bm := make(map[string]backendstore.Backend, len(backends))
	for _, b := range backends {
		bm[b.ID] = b
	}
	var out []recovery.Candidate
	for _, m := range models {
		b, ok := bm[m.BackendID]
		if !ok || !m.Enabled || !m.AutoAssign || !b.Enabled || !b.Installed || b.IsLocal || (b.Tier != backendstore.TierSubscription && b.Tier != backendstore.TierFree) {
			continue
		}
		out = append(out, recovery.Candidate{BackendID: m.BackendID, ModelID: m.ModelID, Priority: tierPriority[m.Tier]})
	}
	return out
}

func (c *BackendRecoveryCoordinator) waitLocked(sess *store.Session, generation uint64, resets []store.RecoveryReset, fallbackAt time.Time) {
	now := c.now().UTC()
	next := fallbackAt
	for _, r := range resets {
		if r.ResetsAt != nil && r.ResetsAt.After(now) && (next.IsZero() || r.ResetsAt.Before(next)) {
			next = *r.ResetsAt
		}
	}
	if !next.After(now) {
		next = now.Add(30 * time.Minute)
	}
	_ = c.store.Update(context.Background(), sess.ID, func(s *store.Session) error {
		if s.BackendRecovery == nil || s.BackendRecovery.Generation != generation {
			return nil
		}
		s.BackendRecovery.Phase = recoveryWaiting
		s.BackendRecovery.Resets = deterministicResets(resets)
		s.BackendRecovery.NextRetryAt = &next
		s.BackendRecovery.UpdatedAt = now
		return nil
	})
	_ = c.store.SetRateLimit(context.Background(), sess.ID, next, int(sess.BackendRecovery.Round))
	c.event(sess.ID, "backend_recovery_waiting_for_capacity", fmt.Sprintf("generation=%d next_retry=%s", generation, next.Format(time.RFC3339)))
	c.schedule(sess.ID, generation, next)
}

func (c *BackendRecoveryCoordinator) schedule(id string, generation uint64, at time.Time) {
	c.mu.Lock()
	if old := c.timers[id]; old != nil {
		old.Stop()
	}
	d := time.Until(at)
	if d < 0 {
		d = 0
	}
	c.timers[id] = time.AfterFunc(d, func() { c.retry(id, generation) })
	c.mu.Unlock()
}

func (c *BackendRecoveryCoordinator) retry(id string, generation uint64) {
	ctx := context.Background()
	sess, err := c.store.Get(ctx, id)
	if err != nil || sess.BackendRecovery == nil || sess.BackendRecovery.Generation != generation || sess.BackendRecovery.Phase != recoveryWaiting {
		return
	}
	_ = c.store.Update(ctx, id, func(s *store.Session) error {
		s.BackendRecovery.Round++
		s.BackendRecovery.Phase = recoveryRefreshing
		s.BackendRecovery.NextRetryAt = nil
		return nil
	})
	c.advance(id, generation, c.now().Add(30*time.Minute))
}

// OnTransition starts a bounded stabilization observation after a later live
// poll. Process launch or one live observation alone never clears recovery.
func (c *BackendRecoveryCoordinator) OnTransition(sess *store.Session, _, to store.Status) {
	if c == nil || sess == nil || (to != store.StatusWorking && to != store.StatusIdle && to != store.StatusWaitingForInput) {
		return
	}
	lock := c.sessionLock(sess.ID)
	lock.Lock()
	defer lock.Unlock()
	current, err := c.store.Get(context.Background(), sess.ID)
	if err != nil || current.BackendRecovery == nil || current.BackendRecovery.Phase != recoveryStabilizing {
		return
	}
	generation := current.BackendRecovery.Generation
	target := current.BackendRecovery.Current
	if target == nil || current.Backend != target.BackendID || current.Model != target.ModelID {
		return
	}
	now := c.now().UTC()
	_ = c.store.Update(context.Background(), sess.ID, func(s *store.Session) error {
		if s.BackendRecovery != nil && s.BackendRecovery.Generation == generation {
			s.BackendRecovery.StableSince = &now
		}
		return nil
	})
	c.scheduleStabilization(sess.ID, generation, *target, now.Add(c.stabilizationWindow))
}

func (c *BackendRecoveryCoordinator) scheduleStabilization(id string, generation uint64, target store.BackendCandidate, at time.Time) {
	c.mu.Lock()
	if old := c.timers[id]; old != nil {
		old.Stop()
	}
	d := time.Until(at)
	if d < 0 {
		d = 0
	}
	c.timers[id] = time.AfterFunc(d, func() { c.verifyStable(id, generation, target) })
	c.mu.Unlock()
}

func (c *BackendRecoveryCoordinator) verifyStable(id string, generation uint64, target store.BackendCandidate) {
	lock := c.sessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	ctx := context.Background()
	sess, err := c.store.Get(ctx, id)
	if err != nil || sess.BackendRecovery == nil || sess.BackendRecovery.Generation != generation || sess.BackendRecovery.Phase != recoveryStabilizing || sess.BackendRecovery.StableSince == nil || sess.Backend != target.BackendID || sess.Model != target.ModelID || (sess.Status != store.StatusWorking && sess.Status != store.StatusIdle && sess.Status != store.StatusWaitingForInput) {
		return
	}
	_ = c.store.Update(ctx, id, func(s *store.Session) error {
		if s.BackendRecovery != nil && s.BackendRecovery.Generation == generation {
			s.BackendRecovery = nil
		}
		return nil
	})
	_ = c.store.ClearRateLimit(ctx, id)
	c.event(id, "backend_recovery_stabilized", fmt.Sprintf("generation=%d candidate=%s/%s", generation, target.BackendID, target.ModelID))
}

// Supersede invalidates automatic work before a manual switch/stop/delete.
func (c *BackendRecoveryCoordinator) Supersede(ctx context.Context, id, action string) {
	if c == nil {
		return
	}
	lock := c.sessionLock(id)
	lock.Lock()
	defer lock.Unlock()
	sess, err := c.store.Get(ctx, id)
	if err != nil || sess.BackendRecovery == nil {
		return
	}
	generation := sess.BackendRecovery.Generation
	_ = c.store.Update(ctx, id, func(s *store.Session) error { s.BackendRecovery = nil; return nil })
	c.mu.Lock()
	if timer := c.timers[id]; timer != nil {
		timer.Stop()
		delete(c.timers, id)
	}
	c.mu.Unlock()
	c.event(id, "backend_recovery_superseded", fmt.Sprintf("generation=%d action=%s", generation, action))
}

func (c *BackendRecoveryCoordinator) Reconstruct(ctx context.Context) error {
	sessions, err := c.store.List(ctx)
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if sess.BackendRecovery == nil || sess.BackendRecovery.Phase != recoveryWaiting || sess.BackendRecovery.NextRetryAt == nil {
			continue
		}
		c.schedule(sess.ID, sess.BackendRecovery.Generation, *sess.BackendRecovery.NextRetryAt)
	}
	return nil
}

func (c *BackendRecoveryCoordinator) event(id, typ, detail string) {
	_ = c.store.AppendEvent(context.Background(), id, store.Event{TS: c.now().UTC(), Type: typ, Detail: detail})
	// Wake SSE subscribers on every phase-transition event. The spec (§8) says
	// "SSE/store notifications fire on phase changes, not every stabilization poll",
	// and every call to event() in this coordinator corresponds to a phase change.
	if c.notifyFn != nil {
		c.notifyFn()
	}
}

func candidateKey(backend, model string) string { return backend + "\x00" + model }
func errorClass(err error) string {
	if err != nil {
		return "unavailable"
	}
	return "ok"
}

// deterministicResets is kept small and testable when providers return the same
// scopes in different orders.
func deterministicResets(in []store.RecoveryReset) []store.RecoveryReset {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].BackendID != in[j].BackendID {
			return in[i].BackendID < in[j].BackendID
		}
		if in[i].LimitID != in[j].LimitID {
			return in[i].LimitID < in[j].LimitID
		}
		return in[i].Scope < in[j].Scope
	})
	return in
}
