package tui

import (
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// Presentation-only state: never persisted or used to infer executor health.
const autopilotStatusUnavailable = "status unavailable"

// autopilotWithSessionFallback preserves visibility while independent status and
// session requests arrive out of order or fail. The tree claims run-tagged
// sessions before building run nodes, so a missing run would otherwise swallow
// its entire agent subtree. Only non-terminal session evidence creates a row;
// historical done/orphaned/error records must not resurrect completed runs.
func autopilotWithSessionFallback(st client.AutopilotStatus, sessions []*store.Session, projects []projectstore.Project, plans map[string][]*planstore.Plan) client.AutopilotStatus {
	out := client.AutopilotStatus{Runs: append([]client.AutopilotRunStatus(nil), st.Runs...)}
	known := make(map[string]bool, len(st.Runs))
	for _, r := range st.Runs {
		known[r.RunID] = true
	}
	// Prefer manager metadata, then stable agent IDs, independent of poll order.
	ordered := append([]*store.Session(nil), sessions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		am, bm := a.Role == "autopilot" || a.AutopilotSlot == store.AutopilotSlotManager, b.Role == "autopilot" || b.AutopilotSlot == store.AutopilotSlotManager
		if am != bm {
			return am
		}
		return a.ID < b.ID
	})
	for _, s := range ordered {
		id := sessionRunID(s)
		if id == "" || known[id] || (!liveStatus(s.Status) && s.Status != store.StatusRateLimited) {
			continue
		}
		r := client.AutopilotRunStatus{RunID: id, Name: id, State: autopilotStatusUnavailable, Repo: s.Repo}
		for _, p := range projects {
			if p.ID == s.ProjectID && p.Path != "" {
				r.Repo = p.Path
				break
			}
		}
		if r.Repo == "" {
			r.Repo = s.Workdir
		}
		if s.Role == "autopilot" || s.AutopilotSlot == store.AutopilotSlotManager {
			r.ManagerSlotID = s.ID
			if s.Name != "" {
				r.Name = s.Name
			}
		}
		// Use only explicit plan/run links, never names or directory heuristics.
		for _, ps := range plans {
			for _, p := range ps {
				if p != nil && p.AutopilotRunID == id {
					r.PlanID, r.Name = p.ID, p.Name
				}
			}
		}
		out.Runs = append(out.Runs, r)
		known[id] = true
	}
	return out
}

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
