package agentstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/srjn45/scriva/engine"
	"github.com/srjn45/warden/internal/store"
)

// RepairAvailable reports whether warden can rebuild a corrupt agent store.
// It is false until ScrivaDB ships the Verify/Repair primitives listed in
// docs/specs/2026-10-06-agent-store-integrity-contract.md §2; warden does not
// emulate them. Every operator surface reads this one flag so it flips in one
// place when the dependency lands.
const RepairAvailable = true

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
	"stop the daemon, run `warden repair agents --dry-run`, then repair from its verified backup — see " +
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

// VerifyAgentStore is the read-only, offline ScrivaDB verification pass.  The
// engine owns the format-specific checks; Warden deliberately does not infer
// recovery from segment bytes itself.
func VerifyAgentStore(ctx context.Context, dataDir string) (*engine.IntegrityReport, error) {
	return engine.VerifyDir(ctx, filepath.Join(dataDir, "agents-db"), engine.VerifyOptions{Mode: engine.VerifyFull})
}

// RepairAgentStore invokes ScrivaDB's offline repair primitive for the agent
// collections. It refuses a held engine lock, takes/verifies a backup before a
// mutation, journals interrupted work, and preserves conflicts in its report.
func RepairAgentStore(ctx context.Context, dataDir, backup string, salvage bool, conflict engine.ConflictPolicy) (*engine.RepairReport, error) {
	if err := CheckRepairAuthority(dataDir); err != nil {
		return nil, err
	}
	if err := ProbeOwnership(dataDir); err != nil {
		return nil, err
	}
	opts := engine.RepairOptions{Collections: []string{"agents", "closed"}, BackupDir: backup, Salvage: salvage, OnConflict: conflict}
	return engine.Repair(ctx, filepath.Join(dataDir, "agents-db"), opts)
}

// ReportFailures turns a verification report into the existing typed degraded
// boundary. This is used for pre-open verification so a missing persisted index
// cannot be presented as a complete fleet.
func ReportFailures(rep *engine.IntegrityReport) []store.ScanFailure {
	if rep == nil || rep.MaxSeverity() == "" || rep.MaxSeverity() == engine.SeverityInfo {
		return nil
	}
	all := rep.AllFindings()
	out := make([]store.ScanFailure, 0, len(all))
	for _, f := range all {
		if f.Severity == engine.SeverityInfo {
			continue
		}
		out = append(out, store.ScanFailure{Collection: f.Collection, Key: f.Location.Field, Class: store.DegradeIntegrity, Detail: fmt.Sprintf("%s: %s", f.Code, f.Message)})
	}
	return out
}
