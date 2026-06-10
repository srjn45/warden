package daemon

import "time"

type restartAction int

const (
	actionGiveUp restartAction = iota
	actionRestart
)

// decideRestart decides whether an errored, auto-restart-enabled agent should be
// restarted, and the counter to persist if so. A restart that happened >= reset
// ago (or never) means the prior run was sustained-healthy, so the counter resets
// to 0 — "a successful run resets the counter", defined as sustained health so a
// resume->instant-crash loop cannot evade the cap by briefly reaching working.
func decideRestart(count int, lastRestartAt, now time.Time, max int, reset time.Duration) (restartAction, int) {
	effective := count
	if lastRestartAt.IsZero() || now.Sub(lastRestartAt) >= reset {
		effective = 0
	}
	if effective >= max {
		return actionGiveUp, effective
	}
	return actionRestart, effective + 1
}
