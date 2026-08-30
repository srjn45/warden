// Package ctxtokens provides context-window occupancy tracking from agent transcripts
// and classifies it against warn/critical thresholds.
package ctxtokens

// State is an agent's context-fill band.
type State string

const (
	StateOK       State = "ok"
	StateWarning  State = "warning"
	StateCritical State = "critical"
)

// Classify maps a token count to a state. warn and crit are inclusive lower
// bounds: tokens >= crit is critical, tokens >= warn (but < crit) is warning.
func Classify(tokens, warn, crit int) State {
	switch {
	case tokens >= crit:
		return StateCritical
	case tokens >= warn:
		return StateWarning
	default:
		return StateOK
	}
}
