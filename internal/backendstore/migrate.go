package backendstore

import (
	"errors"
	"os"
	"time"
)

// AutopilotLadderMarker names the sentinel written (last) once the one-time
// autopilot cost-tier ladder import has completed. Its presence means the store is
// authoritative for backend tiers + the paid-autopilot gate and the import never
// re-runs — so a user's later tier / gate edits in the store survive every
// subsequent boot. The daemon joins it onto the data dir to form sentinelPath. See
// MigrateAutopilotLadder.
const AutopilotLadderMarker = ".autopilot-ladder-migrated"

// MigrateAutopilotLadder folds the deprecated autopilot.brain.backends cost-tier
// ladder and allow_pay_per_use gate into the registry ONCE, on the first boot
// after upgrade (docs/specs/2026-08-06-backend-registry.md §8). It makes the store
// the single source of truth: each listed backend id is tiered to its config tier
// (free / subscription / pay_per_use) and Settings.AllowPaidAutopilot is seeded
// from allowPaid.
//
// The import is guarded by a sentinel file at sentinelPath, written LAST — so a
// crash mid-import re-imports the same (idempotent) config values next boot, while
// a completed import never runs again. That guard is what keeps LATER user edits in
// the store authoritative: once the sentinel exists, config no longer clobbers the
// store on boot. Returns whether the import ran (false ⇒ already migrated).
//
// A listed id with no store row (a backend not installed / not detected on this
// machine) is skipped, not created: the registry only tiers backends it knows. The
// reserved local / terminal ids are never user-tierable and are skipped too. On any
// per-backend or settings write error the import aborts WITHOUT writing the
// sentinel, so the next boot retries.
func MigrateAutopilotLadder(s *Store, sentinelPath string, free, subscription, payPerUse []string, allowPaid bool) (bool, error) {
	migrated, err := autopilotLadderMigrated(sentinelPath)
	if err != nil {
		return false, err
	}
	if migrated {
		return false, nil
	}

	tiers := []struct {
		tier string
		ids  []string
	}{
		{TierFree, free},
		{TierSubscription, subscription},
		{TierPayPerUse, payPerUse},
	}
	for _, t := range tiers {
		for _, id := range t.ids {
			if id == "" || id == idLocal || id == idTerminal {
				continue
			}
			// A config-listed backend that isn't installed/detected here has no store
			// row yet — skip it rather than materialise a phantom row. A rescan that
			// later detects it leaves it unclassified, and the user tiers it directly.
			if err := s.SetTier(id, t.tier); err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return false, err
			}
		}
	}
	if err := s.SetAllowPaidAutopilot(allowPaid); err != nil {
		return false, err
	}

	// Sentinel LAST: only now is the store authoritative and the import complete.
	if err := os.WriteFile(sentinelPath, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// autopilotLadderMigrated reports whether the ladder-import sentinel exists,
// distinguishing a genuine stat error from a plain not-exist (which means the
// import has not run).
func autopilotLadderMigrated(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}
