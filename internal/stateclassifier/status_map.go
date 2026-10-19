package stateclassifier

import (
	"github.com/srjn45/warden/internal/ptyevent"
	"github.com/srjn45/warden/internal/store"
)

// ToStoreStatus maps the normalized ptyevent.State and WaitingReason to the
// existing store.Status enum per contract spec §8.4.
func ToStoreStatus(s ptyevent.State, r ptyevent.WaitingReason) (store.Status, bool) {
	switch s {
	case ptyevent.StateWorking:
		return store.StatusWorking, true
	case ptyevent.StateIdle:
		return store.StatusIdle, true
	case ptyevent.StateWaiting:
		switch r {
		case ptyevent.ReasonStartup:
			return store.StatusSpawning, true
		case ptyevent.ReasonRateLimit, ptyevent.ReasonQuota:
			return store.StatusRateLimited, true
		case ptyevent.ReasonSubprocess:
			// A tool child process running without terminal output is executing work.
			return store.StatusWorking, true
		default:
			return store.StatusWaitingForInput, true
		}
	case ptyevent.StateWaitingForPermission, ptyevent.StateWaitingForDecision, ptyevent.StateNeedsAttention:
		return store.StatusWaitingForInput, true
	case ptyevent.StateExited:
		return store.StatusDone, true
	default:
		return "", false
	}
}
