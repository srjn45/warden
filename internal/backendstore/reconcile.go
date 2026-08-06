package backendstore

import (
	"errors"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
)

// Reconcile folds a detection sweep into the store, preserving the user's
// preferences (docs/specs/2026-08-06-backend-registry.md §4). It writes ONLY the
// detection fields — Installed / BinaryPath / DetectedAt — and never touches
// Tier / Default / Enabled, so those survive an uninstall/reinstall. A backend
// seen for the first time starts unclassified + enabled.
//
// Two rows are special:
//   - terminal is forced Installed=true (it is the host shell, always present and
//     a non-AI backend), so it is listed for completeness but never becomes an
//     internal-thinking candidate or an auto-default.
//   - the reserved local row (the local model, not a PATH-detected CLI) is
//     reconciled separately from localInstalled: IsLocal=true, Tier=local, its
//     LimitedUntil forced to zero (it can never be limited), and it can never be
//     a user-agent default.
func Reconcile(store *Store, det []agentbackend.Detected, localInstalled bool, now time.Time) error {
	for _, d := range det {
		b, err := store.Get(d.ID)
		if errors.Is(err, ErrNotFound) {
			b = Backend{ID: d.ID, Tier: TierUnclassified, Enabled: true} // first sight
		} else if err != nil {
			return err
		}
		b.Installed = d.Installed
		b.BinaryPath = d.Path
		b.DetectedAt = now
		if d.ID == idTerminal {
			b.Installed = true // host shell is always present
		}
		if err := store.Upsert(b); err != nil {
			return err
		}
	}
	return reconcileLocal(store, localInstalled, now)
}

// reconcileLocal seeds/updates the reserved local-model row. Detection for local
// is "is the local endpoint configured/reachable" (localInstalled), not a PATH
// lookup; the row always carries the system-set local invariants (IsLocal, tier
// local, never limited, never default). Enabled is preserved across reconciles.
func reconcileLocal(store *Store, installed bool, now time.Time) error {
	b, err := store.Get(idLocal)
	if errors.Is(err, ErrNotFound) {
		b = Backend{ID: idLocal, Enabled: true} // first sight
	} else if err != nil {
		return err
	}
	b.IsLocal = true
	b.Tier = TierLocal
	b.Installed = installed
	b.BinaryPath = ""
	b.LimitedUntil = time.Time{} // local can never be limited
	b.Default = false            // local can never be a user-agent default
	b.DetectedAt = now
	return store.Upsert(b)
}
