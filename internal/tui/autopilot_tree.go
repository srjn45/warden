package tui

import (
	"strings"

	"github.com/srjn45/warden/internal/store"
)

func apRunKey(runID string) string { return "aprun\x00" + runID }

func apPlanKey(runID string) string { return "applan\x00" + runID }

func apWorkersKey(runID string) string { return "apworkers\x00" + runID }

// sessionRunID prefers the WP3 back-ref field, then the dual-read tag window
// (`run:<id>` / `autopilot-run:<id>`). Empty means the session is not owned by
// an autopilot run.
func sessionRunID(s *store.Session) string {
	if s == nil {
		return ""
	}
	if s.AutopilotRunID != "" {
		return s.AutopilotRunID
	}
	return sessionRunIDFromTags(s)
}

func sessionRunIDFromTags(s *store.Session) string {
	if s == nil {
		return ""
	}
	for _, tag := range s.Tags {
		if strings.HasPrefix(tag, "run:") {
			return strings.TrimPrefix(tag, "run:")
		}
		if strings.HasPrefix(tag, "autopilot-run:") {
			return strings.TrimPrefix(tag, "autopilot-run:")
		}
	}
	return ""
}

// isAutopilotOwned reports whether a session belongs in an autopilot run tree
// rather than the flat agent grid. Dual-read: back-ref fields or ownership tags.
func isAutopilotOwned(s *store.Session) bool {
	if s == nil {
		return false
	}
	if s.AutopilotRunID != "" || s.AutopilotSlot != "" {
		return true
	}
	return sessionRunIDFromTags(s) != ""
}
