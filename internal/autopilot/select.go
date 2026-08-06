package autopilot

// Tier names, the cost order the selection loop walks (autopilot.md §7). They are
// the values reported in BrainStatus.Tier.
const (
	tierFree         = "free"
	tierSubscription = "subscription"
	tierPayPerUse    = "pay_per_use"
)

// selection is the outcome of walking the cost-tier ladder (autopilot.md §7).
type selection struct {
	// Backend is the chosen backend. It is "" only in the unconfigured-ladder case
	// (OK is still true), meaning "let the daemon pick its default".
	Backend string
	// Tier is the cost tier Backend came from (free | subscription | pay_per_use).
	Tier string
	// OK reports that a backend was selected.
	OK bool
	// GateOnly is set when nothing is selectable EXCEPT a pay_per_use backend the
	// gate excludes — the distinct "flip allow_pay_per_use" signal (§7). It is
	// meaningful only when OK is false.
	GateOnly bool
}

// TierLadderSource yields autopilot's cost-tier backend ladder plus the
// paid-autopilot gate. Per the backend-registry unification (docs/specs/
// 2026-08-06-backend-registry.md §8) the registry store is the source of truth, so
// the daemon injects a store-backed implementation (ControllerConfig.LadderSource)
// and a user's live tier / gate edits govern the next selection with no restart.
// When no source is injected the Controller wraps the (now-deprecated) config
// ladder in a static source, so pre-registry daemons and unit tests keep working.
type TierLadderSource interface {
	// TierLadder returns the free / subscription / pay_per_use backend-id lists in
	// preference order and whether pay_per_use (paid) autopilot is permitted
	// (store Settings.AllowPaidAutopilot). A non-nil error makes selection degrade
	// to the daemon default rather than risk a wrong-tier pick.
	TierLadder() (ladder BackendLadder, allowPaid bool, err error)
}

// staticLadder wraps a fixed BackendLadder + gate as a TierLadderSource. It backs
// the config-ladder fallback (no store injected) and unit tests.
type staticLadder struct {
	ladder    BackendLadder
	allowPaid bool
}

func (s staticLadder) TierLadder() (BackendLadder, bool, error) {
	return s.ladder, s.allowPaid, nil
}

// selectBackend walks the cost-tier ladder — free → subscription → pay_per_use
// (the last only when the source permits paid autopilot) — and returns the first
// backend that is configured, not excluded, and not currently rate-limited in ts
// (autopilot.md §7). The tier lists and the paid gate come from src, the backend
// registry store (docs/specs/2026-08-06-backend-registry.md §8) — no longer the
// autopilot.brain.backends config. It drives both the brain's backend and the
// backend the brain hands its workers, so one selection policy governs the run.
//
// exclude lets the guardian rotate DOWN the ladder without re-picking a backend
// it just tried this heal cycle (nil ⇒ exclude nothing). A ts of nil treats every
// backend as available (no limit tracking). The rate-limit (ts) and exclude
// handling is unchanged by the store move — only the tier source changed.
//
// A src error, or an entirely unconfigured ladder, yields {Backend:"", Tier:free,
// OK:true} so the daemon-default path is preserved; once "" has been excluded
// (already tried), even that collapses to no selection.
func selectBackend(src TierLadderSource, ts *tierState, exclude map[string]bool) selection {
	l, allowPayPerUse, err := src.TierLadder()
	if err != nil {
		// Registry read failed: degrade to the daemon default (identical to an
		// unconfigured ladder) rather than risk a wrong-tier or paid pick.
		if exclude[""] {
			return selection{}
		}
		return selection{Backend: "", Tier: tierFree, OK: true}
	}
	if len(l.all()) == 0 {
		if exclude[""] {
			return selection{}
		}
		return selection{Backend: "", Tier: tierFree, OK: true}
	}

	tiers := []struct {
		name string
		list []string
	}{
		{tierFree, l.Free},
		{tierSubscription, l.Subscription},
	}
	if allowPayPerUse {
		tiers = append(tiers, struct {
			name string
			list []string
		}{tierPayPerUse, l.PayPerUse})
	}

	for _, tr := range tiers {
		for _, b := range tr.list {
			if !selectable(b, ts, exclude) {
				continue
			}
			return selection{Backend: b, Tier: tr.name, OK: true}
		}
	}

	// Nothing selectable. Distinguish "everything is rate-limited" from "the only
	// thing left is behind the pay_per_use gate" (§7): the latter is actionable by
	// the owner flipping allow_pay_per_use, so it earns a distinct notification.
	gateOnly := false
	if !allowPayPerUse {
		for _, b := range l.PayPerUse {
			if selectable(b, ts, exclude) {
				gateOnly = true
				break
			}
		}
	}
	return selection{GateOnly: gateOnly}
}

// selectable reports whether backend b may be picked: non-blank, not excluded, and
// not currently rate-limited in ts.
func selectable(b string, ts *tierState, exclude map[string]bool) bool {
	if b == "" || exclude[b] {
		return false
	}
	if ts != nil && !ts.available(b) {
		return false
	}
	return true
}

// tierOrDefault reports a run's selected tier, defaulting to free for a run whose
// tier was never recorded (an inert/daemon-default spawn).
func tierOrDefault(tier string) string {
	if tier == "" {
		return tierFree
	}
	return tier
}
