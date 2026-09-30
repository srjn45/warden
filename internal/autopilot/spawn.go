package autopilot

import (
	"strings"

	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/store"
)

// WorkerSpawnRole reports whether role names a delegated work unit spawned by an
// autopilot manager. Those sessions are parented via ParentID to the manager
// (plan-execution redesign); legacy AutopilotRunID back-refs remain readable.
func WorkerSpawnRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "worker", "implementer", "auto-merger", "reviewer":
		return true
	default:
		return false
	}
}

// SessionRunID returns the owning ap- run id from explicit back-ref fields,
// Plan-linked Autopilot identity tags, or legacy run: / autopilot-run: tags.
// AutopilotRunID is no longer required for new agents — tags + PlanID suffice.
func SessionRunID(s *agentstore.Agent) string {
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

// IsManagerRecord reports whether s is an autopilot manager session.
// Preferred identity: role=autopilot with PlanID (and/or ownership tags).
// Legacy AutopilotSlot=manager remains accepted during the compatibility window.
func IsManagerRecord(s *agentstore.Agent) bool {
	if s == nil {
		return false
	}
	if s.AutopilotSlot == store.AutopilotSlotManager {
		return true
	}
	if s.Role != "autopilot" {
		return false
	}
	if s.PlanID != "" {
		return true
	}
	return SessionRunID(s) != "" && s.HasTag("autopilot")
}

// IsWorkerRecord reports whether s is an autopilot worker/implementer session.
// Preferred identity: worker role + ParentID (manager) and ownership tags / PlanID.
// Legacy AutopilotSlot=worker remains accepted during the compatibility window.
func IsWorkerRecord(s *agentstore.Agent) bool {
	if s == nil {
		return false
	}
	if s.AutopilotSlot == store.AutopilotSlotWorker {
		return true
	}
	if !WorkerSpawnRole(s.Role) || !s.HasTag("autopilot") {
		return false
	}
	if s.ParentID != "" || s.PlanID != "" {
		return true
	}
	return SessionRunID(s) != ""
}

// IsHeadlessBrain reports whether s is an on-demand role=brain Agent that should
// be hidden from the normal agent tree (system:true / headless).
func IsHeadlessBrain(s *agentstore.Agent) bool {
	if s == nil {
		return false
	}
	return s.Role == "brain" && s.HasTag("system:true")
}
