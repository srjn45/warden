package autopilot

import "fmt"

// claimRegistry tracks slot scopes and reserved manager/guardian slot ids so
// concurrent runs cannot steal each other's stable session ids.
type claimRegistry struct {
	scopeByRun  map[string]string // runID -> scope
	runByScope  map[string]string // scope -> runID
	runBySlotID map[string]string // slot session id -> owning runID
}

func newClaimRegistry() *claimRegistry {
	return &claimRegistry{
		scopeByRun:  map[string]string{},
		runByScope:  map[string]string{},
		runBySlotID: map[string]string{},
	}
}

func (cr *claimRegistry) scopeTaken(scope, selfRunID string) bool {
	if scope == "" {
		return false
	}
	if owner, ok := cr.runByScope[scope]; ok && owner != selfRunID {
		return true
	}
	if owner, ok := cr.runBySlotID[scope]; ok && owner != selfRunID {
		return true
	}
	return false
}

func (cr *claimRegistry) validateClaim(runID, scope string) error {
	if other, ok := cr.runByScope[scope]; ok && other != runID {
		return fmt.Errorf("%w: slot scope %q is owned by run %s", ErrRunConflict, scope, other)
	}
	if other, ok := cr.runBySlotID[scope]; ok && other != runID {
		return fmt.Errorf("%w: slot scope %q collides with session id owned by run %s", ErrRunConflict, scope, other)
	}
	for _, slotID := range []string{ManagerSlotID(scope), GuardianSlotID(scope)} {
		if other, ok := cr.runBySlotID[slotID]; ok && other != runID {
			return fmt.Errorf("%w: slot id %q is owned by run %s", ErrRunConflict, slotID, other)
		}
		if other, ok := cr.runByScope[slotID]; ok && other != runID {
			return fmt.Errorf("%w: slot id %q collides with scope owned by run %s", ErrRunConflict, slotID, other)
		}
	}
	return nil
}

func (cr *claimRegistry) claim(runID, scope string) error {
	if err := cr.validateClaim(runID, scope); err != nil {
		return err
	}
	if prev, ok := cr.scopeByRun[runID]; ok && prev == scope {
		return nil
	}
	cr.release(runID)
	cr.scopeByRun[runID] = scope
	cr.runByScope[scope] = runID
	cr.runBySlotID[ManagerSlotID(scope)] = runID
	cr.runBySlotID[GuardianSlotID(scope)] = runID
	return nil
}

func (cr *claimRegistry) release(runID string) {
	scope, ok := cr.scopeByRun[runID]
	if !ok {
		return
	}
	delete(cr.scopeByRun, runID)
	if cr.runByScope[scope] == runID {
		delete(cr.runByScope, scope)
	}
	for _, slotID := range []string{ManagerSlotID(scope), GuardianSlotID(scope)} {
		if cr.runBySlotID[slotID] == runID {
			delete(cr.runBySlotID, slotID)
		}
	}
}
