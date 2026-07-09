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

// selectBackend walks the cost-tier ladder — free → subscription → pay_per_use
// (the last only when allowPayPerUse) — and returns the first backend that is
// configured, not excluded, and not currently rate-limited in ts (autopilot.md
// §7). It drives both the brain's backend and the backend the brain hands its
// workers, so one selection policy governs the whole run.
//
// exclude lets the guardian rotate DOWN the ladder without re-picking a backend
// it just tried this heal cycle (nil ⇒ exclude nothing). A ts of nil treats every
// backend as available (no limit tracking).
//
// An entirely unconfigured ladder yields {Backend:"", Tier:free, OK:true} so the
// S1/S3 daemon-default path is preserved; once "" has been excluded (already
// tried), even that collapses to no selection.
func selectBackend(l BackendLadder, ts *tierState, allowPayPerUse bool, exclude map[string]bool) selection {
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
