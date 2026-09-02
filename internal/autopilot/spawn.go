package autopilot

import (
	"strings"

	"github.com/srjn45/warden/internal/store"
)

// WorkerSpawnRole reports whether role names a delegated work unit spawned by an
// autopilot manager. Those sessions carry run back-refs instead of parent_id.
func WorkerSpawnRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "worker", "implementer", "auto-merger", "reviewer":
		return true
	default:
		return false
	}
}

// SessionRunID returns the owning ap- run id from explicit back-ref fields or
// legacy run: / autopilot-run: tags.
func SessionRunID(s *store.Session) string {
	if s == nil {
		return ""
	}
	if id := strings.TrimSpace(s.AutopilotRunID); id != "" {
		return id
	}
	for _, t := range s.Tags {
		if strings.HasPrefix(t, "run:") {
			return strings.TrimPrefix(t, "run:")
		}
		if strings.HasPrefix(t, "autopilot-run:") {
			return strings.TrimPrefix(t, "autopilot-run:")
		}
	}
	return ""
}

// IsManagerRecord reports whether s is an autopilot manager session (slot
// back-ref or legacy role=autopilot with run ownership tags).
func IsManagerRecord(s *store.Session) bool {
	if s == nil {
		return false
	}
	if s.AutopilotSlot == store.AutopilotSlotManager {
		return true
	}
	return s.Role == "autopilot" && SessionRunID(s) != "" && s.HasTag("autopilot")
}

// IsWorkerRecord reports whether s is an autopilot worker/implementer session.
func IsWorkerRecord(s *store.Session) bool {
	if s == nil {
		return false
	}
	if s.AutopilotSlot == store.AutopilotSlotWorker {
		return true
	}
	return WorkerSpawnRole(s.Role) && SessionRunID(s) != "" && s.HasTag("autopilot")
}
