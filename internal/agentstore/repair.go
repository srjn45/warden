package agentstore

import (
	"errors"
)

// RepairAvailable reports whether warden can rebuild a corrupt agent store.
// It is false until ScrivaDB ships the Verify/Repair primitives listed in
// docs/specs/2026-10-06-agent-store-integrity-contract.md §2; warden does not
// emulate them. Every operator surface reads this one flag so it flips in one
// place when the dependency lands.
const RepairAvailable = false

// ErrRepairUnavailable matches (errors.Is) *RepairUnavailableError.
var ErrRepairUnavailable = errors.New("agent store repair unavailable")

// RepairUnavailableError explains that the repair primitive is absent from the
// pinned ScrivaDB, and what the operator can safely do meanwhile.
type RepairUnavailableError struct{}

func (*RepairUnavailableError) Error() string {
	return "agent store repair is not available yet: it depends on ScrivaDB Verify/Repair primitives " +
		"(srjn45/scriva#107) that the pinned release does not export; " + SafeNextStep
}

func (*RepairUnavailableError) Unwrap() error { return ErrRepairUnavailable }

// SafeNextStep is the operator guidance shown on every degraded/owned surface.
// It never suggests deleting files or the lock.
const SafeNextStep = "running agents are unaffected (tmux sessions are never touched); " +
	"stop the daemon and back up the data directory before any manual action — see " +
	"the 'Agent store integrity' guide (daemon-offline procedure)"

// ProbeOwnership reports whether the offline-repair precondition holds for
// dataDir: the agent-store lock can be taken (nobody owns the store). It takes
// and immediately releases the lock and never opens, imports or writes the
// store. It returns *OwnershipError when the store is owned.
func ProbeOwnership(dataDir string) error {
	l, _, err := acquireOwnership(dataDir)
	if err != nil {
		return err
	}
	return l.release()
}
