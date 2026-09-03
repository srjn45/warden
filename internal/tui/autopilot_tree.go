package tui

import (
	"strings"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/store"
)

func apRunKey(runID string) string { return "aprun\x00" + runID }

func apPlanKey(runID string) string { return "applan\x00" + runID }

func apWorkersKey(runID string) string { return "apworkers\x00" + runID }

func apWorkerGroupKey(runID, taskID string) string {
	return "apwtask\x00" + runID + "\x00" + taskID
}

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

func sessionBelongsToRun(s *store.Session, runID string) bool {
	return sessionRunID(s) == runID
}

func runSessionSlot(s *store.Session, r *client.AutopilotRunStatus) string {
	if s.AutopilotSlot != "" {
		return s.AutopilotSlot
	}
	if r.GuardianID != "" && s.ID == r.GuardianID {
		return store.AutopilotSlotGuardian
	}
	if r.Brain != nil && s.ID == r.Brain.AgentID {
		return store.AutopilotSlotManager
	}
	if s.Role == "autopilot" {
		return store.AutopilotSlotManager
	}
	return store.AutopilotSlotWorker
}

type workerGroup struct {
	taskID   string
	state    string
	sessions []*store.Session
}

func partitionRunSessions(sessions []*store.Session, r *client.AutopilotRunStatus) (managers, guardians, workers []*store.Session) {
	for _, s := range sessions {
		if !sessionBelongsToRun(s, r.RunID) && s.ID != r.GuardianID {
			continue
		}
		switch runSessionSlot(s, r) {
		case store.AutopilotSlotManager:
			managers = append(managers, s)
		case store.AutopilotSlotGuardian:
			guardians = append(guardians, s)
		default:
			workers = append(workers, s)
		}
	}
	return managers, guardians, workers
}

func groupWorkersByLedger(workers []*store.Session, r *client.AutopilotRunStatus) []workerGroup {
	stateByTask := map[string]string{}
	for _, t := range r.LedgerTasks {
		if t.ID == "" {
			continue
		}
		stateByTask[t.ID] = t.State
	}
	byTask := map[string][]*store.Session{}
	var seen []string
	for _, s := range workers {
		tid := s.AutopilotTaskID
		if _, ok := byTask[tid]; !ok {
			seen = append(seen, tid)
		}
		byTask[tid] = append(byTask[tid], s)
	}

	used := map[string]bool{}
	var groups []workerGroup
	appendGroup := func(id, state string) {
		if used[id] {
			return
		}
		sess := byTask[id]
		if len(sess) == 0 && id != "" {
			return
		}
		if len(sess) == 0 {
			return
		}
		used[id] = true
		groups = append(groups, workerGroup{taskID: id, state: state, sessions: sess})
	}

	for _, st := range autopilot.CanonicalLedgerStates {
		want := string(st)
		for _, t := range r.LedgerTasks {
			if t.State == want {
				appendGroup(t.ID, want)
			}
		}
		for _, id := range seen {
			if stateByTask[id] == want {
				appendGroup(id, want)
			}
		}
	}
	for _, t := range r.PlanTasks {
		appendGroup(t.ID, stateByTask[t.ID])
	}
	for _, id := range seen {
		appendGroup(id, stateByTask[id])
	}
	return groups
}

func appendAutopilotRunItems(items []item, r *client.AutopilotRunStatus, sessions []*store.Session, collapsed map[string]bool) []item {
	runKey := apRunKey(r.RunID)
	items = append(items, item{apRun: r, collapsed: collapsed[runKey], underProject: true})
	if collapsed[runKey] {
		return items
	}

	managers, guardians, workers := partitionRunSessions(sessions, r)

	planKey := apPlanKey(r.RunID)
	planCollapsed := collapsed[planKey]
	items = append(items, item{apPlan: true, apPlanRun: r.RunID, collapsed: planCollapsed, hasKids: len(r.PlanTasks) > 0, underProject: true})
	if !planCollapsed {
		for j := range r.PlanTasks {
			items = append(items, item{apTask: &r.PlanTasks[j], apTaskRun: r.RunID, underProject: true})
		}
	}

	for _, s := range guardians {
		items = append(items, item{session: s, dir: sourceDir(s), depth: 1, underProject: true, apSlot: store.AutopilotSlotGuardian})
	}
	for _, s := range managers {
		items = append(items, item{session: s, dir: sourceDir(s), depth: 1, underProject: true, apSlot: store.AutopilotSlotManager})
	}

	wKey := apWorkersKey(r.RunID)
	wCollapsed := collapsed[wKey]
	items = append(items, item{apWorkers: true, apWorkersRun: r.RunID, collapsed: wCollapsed, hasKids: len(workers) > 0, underProject: true})
	if wCollapsed {
		return items
	}
	for _, g := range groupWorkersByLedger(workers, r) {
		depth := 2
		if g.taskID != "" {
			items = append(items, item{apWorkerGroup: g.taskID, apTaskRun: r.RunID, apLedgerState: g.state, underProject: true})
			depth = 3
		}
		for _, s := range g.sessions {
			items = append(items, item{session: s, dir: sourceDir(s), depth: depth, underProject: true, apSlot: store.AutopilotSlotWorker})
		}
	}
	return items
}
