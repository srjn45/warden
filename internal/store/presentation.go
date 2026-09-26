package store

// PresentedStatus returns the seven-state agent UX vocabulary. It does not
// change persisted or wire statuses. An exit code is affirmative exit evidence;
// without it an errored record remains an orphan available for recovery.
func PresentedStatus(status Status, exitCode *int) string {
	switch status.Canonical() {
	case StatusSpawning:
		return "pending"
	case StatusWorking:
		return "busy"
	case StatusWaitingForInput:
		return "need-input"
	case StatusIdle:
		return "idle"
	case StatusDone:
		return "done"
	case StatusOrphaned:
		return "orphaned"
	case StatusRateLimited:
		return "rate_limited"
	case StatusErrored:
		if exitCode != nil {
			return "done"
		}
		return "orphaned"
	default:
		return "pending"
	}
}
