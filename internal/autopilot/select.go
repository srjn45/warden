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

// tierOrDefault reports a run's selected tier, defaulting to free for a run whose
// tier was never recorded (an inert/daemon-default spawn).
func tierOrDefault(tier string) string {
	if tier == "" {
		return tierFree
	}
	return tier
}
