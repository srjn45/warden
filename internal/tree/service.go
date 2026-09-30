package tree

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/autopilotstore"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// DefaultMaxNodes is the default soft cap on total nodes before truncation (spec §13).
const DefaultMaxNodes = 2000

// Service computes the typed hierarchy from entity snapshots (spec §2).
// It is pure: no I/O, no store access, no disk reads.
type Service struct{}

// NewService returns a new pure tree Service instance.
func NewService() *Service {
	return &Service{}
}

// Build computes the typed hierarchy from inputs. When projectID != "", it scopes
// to that single project's subtree; an unknown projectID yields an empty roots slice.
// The synthetic "No project" bucket is only returned when projectID == "".
//
// Project children are always the five sections (Plans, Autopilots, Pipelines,
// Agents, Terminals). Each entity renders exactly once: live Autopilot managers
// and their worker children nest under Autopilots (not Agents); pipeline job
// agents nest under Pipelines; Plan task groups are never rendered inside
// Autopilot (task evidence lives on Plan detail). Headless brain agents are
// omitted unless Inputs.ShowSystem is true.
func (s *Service) Build(in Inputs, projectID string) *Tree {
	openByKey := map[string]string{}
	closedByKey := map[string]string{}
	projectByID := map[string]projectstore.Project{}
	isClosedProject := map[string]bool{}

	for _, p := range in.Projects {
		projectByID[p.ID] = p
		if projectstore.NormalizeStatus(p.Status) == projectstore.StatusClosed {
			isClosedProject[p.ID] = true
			closedByKey[p.ID] = p.ID
			if p.Path != "" {
				closedByKey[p.Path] = p.ID
			}
		} else {
			openByKey[p.ID] = p.ID
			if p.Path != "" {
				openByKey[p.Path] = p.ID
			}
		}
	}

	liveAutopilots := in.Autopilots
	legacyRuns := []autopilot.RunStatus{}
	if len(liveAutopilots) == 0 {
		legacyRuns = append(legacyRuns, in.Autopilot.Runs...)
	}

	// Index live Autopilots and claim their manager / brain / worker sessions
	// so they nest exactly once under Autopilots (not also under Agents).
	liveByID := make(map[string]*autopilotstore.Autopilot, len(liveAutopilots))
	managerOfLive := map[string]string{} // managerAgentID → autopilotID
	brainOfLive := map[string]string{}   // brainAgentID → autopilotID
	for _, a := range liveAutopilots {
		if a == nil || a.ID == "" {
			continue
		}
		liveByID[a.ID] = a
		if a.ManagerAgentID != "" {
			managerOfLive[a.ManagerAgentID] = a.ID
		}
		if a.BrainAgentID != "" {
			brainOfLive[a.BrainAgentID] = a.ID
		}
	}

	sessionByID := make(map[string]*store.Session, len(in.Sessions))
	for _, sess := range in.Sessions {
		sessionByID[sess.ID] = sess
	}

	isAutopilotSession := map[string]bool{}
	autopilotSessionsByRun := map[string][]*store.Session{}

	claimAutopilot := func(runID string, sess *store.Session) {
		if runID == "" || sess == nil || isAutopilotSession[sess.ID] {
			return
		}
		isAutopilotSession[sess.ID] = true
		autopilotSessionsByRun[runID] = append(autopilotSessionsByRun[runID], sess)
	}

	// Pass 1: claim by live Autopilot manager/brain IDs and AutopilotRunID / run tags.
	for _, sess := range in.Sessions {
		if aid := managerOfLive[sess.ID]; aid != "" {
			claimAutopilot(aid, sess)
			continue
		}
		if aid := brainOfLive[sess.ID]; aid != "" {
			claimAutopilot(aid, sess)
			continue
		}
		if rid := sessionRunID(sess); rid != "" {
			if liveByID[rid] != nil || len(liveAutopilots) == 0 {
				claimAutopilot(rid, sess)
			}
		}
	}

	// Pass 2: claim workers parented to a claimed manager (ParentID / ChildAgents).
	// Repeat until fixed point so nested worker forests stay under Autopilot.
	changed := true
	for changed {
		changed = false
		for _, sess := range in.Sessions {
			if isAutopilotSession[sess.ID] {
				continue
			}
			if sess.ParentID != "" && isAutopilotSession[sess.ParentID] {
				// Parent is under some Autopilot — inherit that run id.
				for rid, members := range autopilotSessionsByRun {
					for _, m := range members {
						if m.ID == sess.ParentID {
							claimAutopilot(rid, sess)
							changed = true
							break
						}
					}
					if isAutopilotSession[sess.ID] {
						break
					}
				}
			}
		}
		for _, parent := range in.Sessions {
			if !isAutopilotSession[parent.ID] {
				continue
			}
			var rid string
			for id, members := range autopilotSessionsByRun {
				for _, m := range members {
					if m.ID == parent.ID {
						rid = id
						break
					}
				}
				if rid != "" {
					break
				}
			}
			for _, cid := range parent.ChildAgents {
				if child := sessionByID[cid]; child != nil && !isAutopilotSession[cid] {
					claimAutopilot(rid, child)
					changed = true
				}
			}
		}
	}

	// Legacy guardian/brain designation from RunStatus when no live Autopilots.
	if len(liveAutopilots) == 0 {
		for i := range legacyRuns {
			r := &legacyRuns[i]
			if r.GuardianID != "" {
				if sess := sessionByID[r.GuardianID]; sess != nil {
					claimAutopilot(r.RunID, sess)
				}
			}
			if r.Brain != nil && r.Brain.AgentID != "" {
				if sess := sessionByID[r.Brain.AgentID]; sess != nil {
					claimAutopilot(r.RunID, sess)
				}
			}
		}
	}

	pipelineJobSessions := map[string]*store.Session{}
	isPipelineSession := map[string]bool{}
	for _, sess := range in.Sessions {
		if isAutopilotSession[sess.ID] {
			continue
		}
		if sess.PipelineID != "" && sess.JobID != "" {
			pipelineJobSessions[sess.PipelineID+"/"+sess.JobID] = sess
			isPipelineSession[sess.ID] = true
		}
	}

	var agents []*store.Session
	terminalsByGroup := map[string][]*store.Session{}
	for _, sess := range in.Sessions {
		if isAutopilotSession[sess.ID] || isPipelineSession[sess.ID] {
			continue
		}
		// Headless brain never appears as a free-floating agent.
		if isHeadlessBrainSession(sess) && !in.ShowSystem {
			continue
		}
		if sess.IsTerminal() {
			key := resolveMembershipKey(
				sess.ID, sess.ProjectID, sessionDir(sess),
				membershipTerminals, in.Projects, openByKey, closedByKey,
			)
			terminalsByGroup[key] = append(terminalsByGroup[key], sess)
		} else {
			agents = append(agents, sess)
		}
	}

	agentRoots, childrenByParent := agentForest(agents)
	agentRootsByGroup := map[string][]*store.Session{}
	for _, sess := range agentRoots {
		key := resolveMembershipKey(
			sess.ID, sess.ProjectID, sessionDir(sess),
			membershipAgents, in.Projects, openByKey, closedByKey,
		)
		agentRootsByGroup[key] = append(agentRootsByGroup[key], sess)
	}

	agentByID := make(map[string]*store.Session, len(agents))
	for _, s := range agents {
		agentByID[s.ID] = s
	}
	pipelineByID := make(map[string]*pipeline.Pipeline, len(in.Pipelines))
	for _, p := range in.Pipelines {
		pipelineByID[p.ID] = p
	}
	ownerOfPipeline := map[string]string{}
	forwardPipeClaim := make(map[string]string)
	for _, s := range agents {
		for _, pid := range s.ChildPipelines {
			if pid == "" || pipelineByID[pid] == nil {
				continue
			}
			if prev, ok := forwardPipeClaim[pid]; ok && prev <= s.ID {
				continue
			}
			forwardPipeClaim[pid] = s.ID
		}
	}
	claimedForwardPipe := make(map[string]bool, len(forwardPipeClaim))
	for pid, aid := range forwardPipeClaim {
		ownerOfPipeline[pid] = aid
		claimedForwardPipe[pid] = true
	}
	for _, p := range in.Pipelines {
		if claimedForwardPipe[p.ID] {
			continue
		}
		if p.ParentAgentID != "" && agentByID[p.ParentAgentID] != nil {
			ownerOfPipeline[p.ID] = p.ParentAgentID
		}
	}

	ownedPipelinesByAgent := map[string][]*pipeline.Pipeline{}
	pipelinesByGroup := map[string][]*pipeline.Pipeline{}
	seenOwned := map[string]bool{}
	for _, s := range agents {
		for _, pid := range s.ChildPipelines {
			if ownerOfPipeline[pid] != s.ID || seenOwned[pid] {
				continue
			}
			ownedPipelinesByAgent[s.ID] = append(ownedPipelinesByAgent[s.ID], pipelineByID[pid])
			seenOwned[pid] = true
		}
	}
	for _, p := range in.Pipelines {
		if owner, ok := ownerOfPipeline[p.ID]; ok {
			if !seenOwned[p.ID] {
				ownedPipelinesByAgent[owner] = append(ownedPipelinesByAgent[owner], p)
				seenOwned[p.ID] = true
			}
			continue
		}
		key := resolveMembershipKey(
			p.ID, p.ProjectID, canonicalDir(p.Repo),
			membershipPipelines, in.Projects, openByKey, closedByKey,
		)
		pipelinesByGroup[key] = append(pipelinesByGroup[key], p)
	}

	plansByGroup := map[string][]*planstore.Plan{}
	for _, p := range in.Plans {
		if p == nil {
			continue
		}
		key := resolveMembershipKey(
			p.ID, p.ProjectID, "",
			membershipPlans, in.Projects, openByKey, closedByKey,
		)
		plansByGroup[key] = append(plansByGroup[key], p)
	}

	autopilotsByGroup := map[string][]*autopilotstore.Autopilot{}
	for _, a := range liveAutopilots {
		if a == nil {
			continue
		}
		key := resolveMembershipKey(
			a.ID, a.ProjectID, canonicalDir(a.Diagnostics.Repo),
			membershipAutopilots, in.Projects, openByKey, closedByKey,
		)
		autopilotsByGroup[key] = append(autopilotsByGroup[key], a)
	}
	legacyRunsByGroup := map[string][]autopilot.RunStatus{}
	for i := range legacyRuns {
		r := legacyRuns[i]
		dir := canonicalDir(r.Repo)
		key := resolveGroupKey("", dir, openByKey, closedByKey)
		legacyRunsByGroup[key] = append(legacyRunsByGroup[key], r)
	}

	for key, roots := range agentRootsByGroup {
		if p, ok := projectByID[key]; ok && p.Agents != nil {
			agentRootsByGroup[key] = orderSessionsByList(roots, p.Agents)
		} else {
			sortAgents(roots)
		}
	}
	for key, terms := range terminalsByGroup {
		if p, ok := projectByID[key]; ok && p.Terminals != nil {
			terminalsByGroup[key] = orderSessionsByList(terms, p.Terminals)
		} else {
			sortTerminals(terms)
		}
	}
	for key, pipes := range pipelinesByGroup {
		if p, ok := projectByID[key]; ok && p.Pipelines != nil {
			pipelinesByGroup[key] = orderPipelinesByList(pipes, p.Pipelines)
		} else {
			sort.SliceStable(pipes, func(i, j int) bool {
				return pipes[i].Name < pipes[j].Name
			})
		}
	}
	for key, plans := range plansByGroup {
		if p, ok := projectByID[key]; ok && p.Plans != nil {
			plansByGroup[key] = orderPlansByList(plans, p.Plans)
		} else {
			sort.SliceStable(plans, func(i, j int) bool {
				return plans[i].Name < plans[j].Name
			})
		}
	}
	for key, aps := range autopilotsByGroup {
		if p, ok := projectByID[key]; ok && p.Autopilots != nil {
			autopilotsByGroup[key] = orderAutopilotsByList(aps, p.Autopilots)
		} else {
			sort.SliceStable(aps, func(i, j int) bool {
				return aps[i].ID < aps[j].ID
			})
		}
	}

	groupKeySet := map[string]bool{}
	for _, p := range in.Projects {
		groupKeySet[p.ID] = true
	}
	for key := range pipelinesByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}
	for key := range autopilotsByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}
	for key := range legacyRunsByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}
	for key := range agentRootsByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}
	for key := range terminalsByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}
	for key := range plansByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}

	hasSyntheticItems := len(pipelinesByGroup[""]) > 0 || len(autopilotsByGroup[""]) > 0 ||
		len(legacyRunsByGroup[""]) > 0 || len(agentRootsByGroup[""]) > 0 ||
		len(terminalsByGroup[""]) > 0 || len(plansByGroup[""]) > 0
	if hasSyntheticItems {
		groupKeySet[""] = true
	}

	var rootNodes []*Node
	for key := range groupKeySet {
		groupChildren := buildGroupChildren(
			key,
			plansByGroup[key],
			autopilotsByGroup[key],
			legacyRunsByGroup[key],
			autopilotSessionsByRun,
			pipelinesByGroup[key],
			pipelineJobSessions,
			agentRootsByGroup[key],
			childrenByParent,
			ownedPipelinesByAgent,
			terminalsByGroup[key],
			in.ShowSystem,
		)

		if key == "" {
			rootNodes = append(rootNodes, &Node{
				Type:     NodeTypeProject,
				ID:       "project:__none__",
				Label:    "No project",
				Status:   rollupNodes(groupChildren),
				Detail:   &Detail{Synthetic: true},
				Children: groupChildren,
			})
			continue
		}

		if p, ok := projectByID[key]; ok {
			name := p.Name
			if name == "" {
				name = filepath.Base(p.ID)
			}
			repo := p.Path
			if repo == "" {
				repo = p.ID
			}
			path := p.Path
			if path == "" {
				path = p.ID
			}
			detail := &Detail{
				Repo: repo,
				Path: path,
			}
			if isClosedProject[key] {
				detail.Closed = true
			}
			rootNodes = append(rootNodes, &Node{
				Type:     NodeTypeProject,
				ID:       "project:" + p.ID,
				Label:    name,
				Status:   rollupNodes(groupChildren),
				Detail:   detail,
				Children: groupChildren,
			})
		} else {
			rootNodes = append(rootNodes, &Node{
				Type:     NodeTypeProject,
				ID:       "project:" + key,
				Label:    filepath.Base(key),
				Status:   rollupNodes(groupChildren),
				Detail:   &Detail{Repo: key, Path: key},
				Children: groupChildren,
			})
		}
	}

	degradedSet := map[string]bool{}
	for _, id := range in.DegradedSubtrees {
		degradedSet[id] = true
	}

	var wholeTreeDegraded bool
	if in.PipelinesDegraded || in.AutopilotDegraded {
		wholeTreeDegraded = true
	}

	var markDegraded func(n *Node)
	markDegraded = func(n *Node) {
		if degradedSet[n.ID] {
			if n.Detail == nil {
				n.Detail = &Detail{}
			}
			n.Detail.Degraded = true
			n.Status = StatusUnknown
			n.Children = nil
			wholeTreeDegraded = true
		}
		if in.PipelinesDegraded && n.Type == NodeTypePipeline {
			if n.Detail == nil {
				n.Detail = &Detail{}
			}
			n.Detail.Degraded = true
			n.Status = StatusUnknown
			n.Children = nil
		}
		if in.AutopilotDegraded && n.Type == NodeTypeAutopilotRun {
			if n.Detail == nil {
				n.Detail = &Detail{}
			}
			n.Detail.Degraded = true
			n.Status = StatusUnknown
			n.Children = nil
		}
		if n.Detail != nil && n.Detail.Degraded {
			wholeTreeDegraded = true
		}
		for _, child := range n.Children {
			markDegraded(child)
		}
	}
	for _, root := range rootNodes {
		markDegraded(root)
	}

	sortRoots(rootNodes, projectByID, isClosedProject)

	if projectID != "" {
		for _, root := range rootNodes {
			if root.Detail != nil && root.Detail.Synthetic {
				continue
			}
			matched := root.ID == "project:"+projectID || root.ID == projectID
			if !matched {
				if p, ok := projectByID[projectID]; ok && (root.ID == "project:"+p.ID || root.ID == "project:"+p.Path) {
					matched = true
				}
			}
			if matched {
				return &Tree{
					Roots:    []*Node{root},
					Degraded: root.Detail != nil && root.Detail.Degraded,
				}
			}
		}
		return &Tree{
			Roots:    []*Node{},
			Degraded: false,
		}
	}

	if rootNodes == nil {
		rootNodes = []*Node{}
	}
	return &Tree{
		Roots:    rootNodes,
		Degraded: wholeTreeDegraded,
	}
}

// buildGroupChildren constructs the five project sections in canonical order:
// Plans → Autopilots → Pipelines → Agents → Terminals.
func buildGroupChildren(
	groupKey string,
	plans []*planstore.Plan,
	liveAutopilots []*autopilotstore.Autopilot,
	legacyRuns []autopilot.RunStatus,
	autopilotSessionsByRun map[string][]*store.Session,
	pipelines []*pipeline.Pipeline,
	pipelineJobSessions map[string]*store.Session,
	agentRoots []*store.Session,
	childrenByParent map[string][]*store.Session,
	ownedPipelinesByAgent map[string][]*pipeline.Pipeline,
	terminals []*store.Session,
	showSystem bool,
) []*Node {
	secID := func(kind SectionKind) string {
		key := groupKey
		if key == "" {
			key = "__none__"
		}
		return "section:" + key + ":" + string(kind)
	}

	var planNodes []*Node
	for _, p := range plans {
		planNodes = append(planNodes, buildPlanNode(p))
	}

	var apNodes []*Node
	if len(liveAutopilots) > 0 {
		for _, a := range liveAutopilots {
			apNodes = append(apNodes, buildLiveAutopilotNode(a, autopilotSessionsByRun[a.ID], showSystem))
		}
	} else {
		sort.SliceStable(legacyRuns, func(i, j int) bool {
			return legacyRuns[i].RunID < legacyRuns[j].RunID
		})
		for i := range legacyRuns {
			r := &legacyRuns[i]
			apNodes = append(apNodes, buildLegacyAutopilotNode(r, autopilotSessionsByRun[r.RunID], showSystem))
		}
	}

	var pipeNodes []*Node
	for _, p := range pipelines {
		pipeNodes = append(pipeNodes, buildPipelineNode(p, pipelineJobSessions))
	}

	var agentNodes []*Node
	for _, s := range agentRoots {
		agentNodes = append(agentNodes, buildAgentSubtree(s, childrenByParent, ownedPipelinesByAgent, pipelineJobSessions))
	}

	var termNodes []*Node
	for _, t := range terminals {
		termNodes = append(termNodes, buildTerminalNode(t))
	}

	return []*Node{
		sectionNode(secID(SectionPlans), "Plans", SectionPlans, planNodes),
		sectionNode(secID(SectionAutopilots), "Autopilots", SectionAutopilots, apNodes),
		sectionNode(secID(SectionPipelines), "Pipelines", SectionPipelines, pipeNodes),
		sectionNode(secID(SectionAgents), "Agents", SectionAgents, agentNodes),
		sectionNode(secID(SectionTerminals), "Terminals", SectionTerminals, termNodes),
	}
}

func sectionNode(id, label string, kind SectionKind, children []*Node) *Node {
	n := &Node{
		Type:   NodeTypeSection,
		ID:     id,
		Label:  label,
		Status: rollupNodes(children),
		Detail: &Detail{Section: string(kind)},
	}
	if len(children) > 0 {
		n.Children = children
	}
	return n
}

func buildPlanNode(p *planstore.Plan) *Node {
	detail := &Detail{PlanID: p.ID}
	if p.ExecutionMode != "" {
		detail.Kind = string(p.ExecutionMode)
	}
	if p.Revision > 0 {
		detail.Revision = p.Revision
	}
	if exec := planstore.ExecutorID(p); exec != "" {
		detail.ExecutorID = exec
	}
	ts := planstore.ComputeTaskSummary(p)
	if ts.Total > 0 {
		detail.TaskSummary = ts.String()
	}
	detail.ExportStatus = string(planstore.ComputeExportStatus(p))
	if !p.UpdatedAt.IsZero() {
		detail.UpdatedAt = p.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return &Node{
		Type:   NodeTypePlan,
		ID:     "plan:" + p.ID,
		Label:  p.Name,
		Status: planNodeStatus(p.Status),
		Detail: detail,
	}
}

func planNodeStatus(s planstore.PlanStatus) string {
	switch s {
	case planstore.PlanStatusInProgress:
		return StatusActive
	case planstore.PlanStatusPending:
		return StatusBlocked
	case planstore.PlanStatusCompleted, planstore.PlanStatusArchived:
		return StatusDone
	default:
		return StatusUnknown
	}
}

// buildLiveAutopilotNode renders Autopilot → manager → workers. Plan task groups
// are never nested here. Headless brain is omitted unless showSystem.
func buildLiveAutopilotNode(a *autopilotstore.Autopilot, sessions []*store.Session, showSystem bool) *Node {
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}

	var manager *store.Session
	if a.ManagerAgentID != "" {
		manager = byID[a.ManagerAgentID]
	}
	if manager == nil {
		for _, s := range sessions {
			if s.Role == "autopilot" || s.AutopilotSlot == store.AutopilotSlotManager {
				manager = s
				break
			}
		}
	}

	var runChildren []*Node
	claimed := map[string]bool{}

	if manager != nil {
		claimed[manager.ID] = true
		mLabel := manager.Name
		if mLabel == "" {
			mLabel = "manager"
		}
		managerNode := &Node{
			Type:      NodeTypeManager,
			ID:        "session:" + manager.ID,
			Label:     mLabel,
			Status:    sessionStatus(manager.Status, manager.ExitCode),
			SessionID: manager.ID,
			Detail:    &Detail{Kind: "agent", Slot: "autopilot", PlanID: firstNonEmpty(manager.PlanID, a.PlanID)},
		}
		// Workers are direct children of the manager (ParentID / ChildAgents).
		var workers []*store.Session
		for _, s := range sessions {
			if s.ID == manager.ID {
				continue
			}
			if isHeadlessBrainSession(s) && !showSystem {
				claimed[s.ID] = true
				continue
			}
			if s.ParentID == manager.ID || containsID(manager.ChildAgents, s.ID) {
				workers = append(workers, s)
			}
		}
		sortAgents(workers)
		for _, w := range workers {
			claimed[w.ID] = true
			wLabel := w.Name
			if wLabel == "" {
				wLabel = w.ID
			}
			managerNode.Children = append(managerNode.Children, &Node{
				Type:      NodeTypeWorker,
				ID:        "session:" + w.ID,
				Label:     wLabel,
				Status:    sessionStatus(w.Status, w.ExitCode),
				SessionID: w.ID,
				Detail:    &Detail{Kind: "agent", Slot: "worker", PlanID: firstNonEmpty(w.PlanID, a.PlanID)},
			})
		}
		runChildren = append(runChildren, managerNode)
	}

	if showSystem && a.BrainAgentID != "" {
		if brain := byID[a.BrainAgentID]; brain != nil && !claimed[brain.ID] {
			claimed[brain.ID] = true
			bLabel := brain.Name
			if bLabel == "" {
				bLabel = "brain"
			}
			runChildren = append(runChildren, &Node{
				Type:      NodeTypeWorker,
				ID:        "session:" + brain.ID,
				Label:     bLabel,
				Status:    sessionStatus(brain.Status, brain.ExitCode),
				SessionID: brain.ID,
				Detail:    &Detail{Kind: "agent", Slot: "brain", PlanID: firstNonEmpty(brain.PlanID, a.PlanID)},
			})
		}
	}

	// Any remaining claimed sessions (loose workers without ParentID) sit under the run.
	var loose []*store.Session
	for _, s := range sessions {
		if claimed[s.ID] {
			continue
		}
		if isHeadlessBrainSession(s) && !showSystem {
			continue
		}
		loose = append(loose, s)
	}
	sortAgents(loose)
	for _, w := range loose {
		wLabel := w.Name
		if wLabel == "" {
			wLabel = w.ID
		}
		runChildren = append(runChildren, &Node{
			Type:      NodeTypeWorker,
			ID:        "session:" + w.ID,
			Label:     wLabel,
			Status:    sessionStatus(w.Status, w.ExitCode),
			SessionID: w.ID,
			Detail:    &Detail{Kind: "agent", Slot: "worker", PlanID: firstNonEmpty(w.PlanID, a.PlanID)},
		})
	}

	label := a.Name
	if label == "" {
		label = autopilotstore.DisplayName("")
	}
	detail := &Detail{
		Repo:   a.Diagnostics.Repo,
		Gate:   a.Diagnostics.Gate,
		PlanID: a.PlanID,
	}
	status := a.Diagnostics.State
	if status == "" {
		status = StatusUnknown
	}
	return &Node{
		Type:     NodeTypeAutopilotRun,
		ID:       "run:" + a.ID,
		Label:    label,
		Status:   status,
		Detail:   detail,
		Children: runChildren,
	}
}

// buildLegacyAutopilotNode renders grandfathered RunStatus as Autopilot → manager
// → workers without Plan task groups or guardian lanes.
func buildLegacyAutopilotNode(r *autopilot.RunStatus, sessions []*store.Session, showSystem bool) *Node {
	var managerSess *store.Session
	var workerSessions []*store.Session

	for _, s := range sessions {
		if isHeadlessBrainSession(s) && !showSystem {
			continue
		}
		switch runSessionSlot(s, r) {
		case store.AutopilotSlotManager:
			if managerSess == nil {
				managerSess = s
			}
		case store.AutopilotSlotGuardian:
			// Guardian sessions are dropped from the tree (drop-guardian-session).
			continue
		default:
			workerSessions = append(workerSessions, s)
		}
	}

	var runChildren []*Node
	if managerSess != nil {
		mLabel := managerSess.Name
		if mLabel == "" {
			mLabel = "manager"
		}
		managerNode := &Node{
			Type:      NodeTypeManager,
			ID:        "session:" + managerSess.ID,
			Label:     mLabel,
			Status:    sessionStatus(managerSess.Status, managerSess.ExitCode),
			SessionID: managerSess.ID,
			Detail:    &Detail{Kind: "agent", Slot: "autopilot", PlanID: managerSess.PlanID},
		}
		sortAgents(workerSessions)
		for _, w := range workerSessions {
			wLabel := w.Name
			if wLabel == "" {
				wLabel = w.ID
			}
			managerNode.Children = append(managerNode.Children, &Node{
				Type:      NodeTypeWorker,
				ID:        "session:" + w.ID,
				Label:     wLabel,
				Status:    sessionStatus(w.Status, w.ExitCode),
				SessionID: w.ID,
				Detail:    &Detail{Kind: "agent", Slot: "worker", PlanID: w.PlanID},
			})
		}
		runChildren = append(runChildren, managerNode)
	} else {
		sortAgents(workerSessions)
		for _, w := range workerSessions {
			wLabel := w.Name
			if wLabel == "" {
				wLabel = w.ID
			}
			runChildren = append(runChildren, &Node{
				Type:      NodeTypeWorker,
				ID:        "session:" + w.ID,
				Label:     wLabel,
				Status:    sessionStatus(w.Status, w.ExitCode),
				SessionID: w.ID,
				Detail:    &Detail{Kind: "agent", Slot: "worker", PlanID: w.PlanID},
			})
		}
	}

	return &Node{
		Type:     NodeTypeAutopilotRun,
		ID:       "run:" + r.RunID,
		Label:    r.Name,
		Status:   runStatus(*r),
		Detail:   &Detail{Repo: r.Repo, Gate: r.Gate},
		Children: runChildren,
	}
}

func buildPipelineNode(p *pipeline.Pipeline, jobSessions map[string]*store.Session) *Node {
	orderedJobs := sortJobs(p.Jobs)
	var jobNodes []*Node
	for _, j := range orderedJobs {
		sessionID := j.AgentRef()
		if sess, ok := jobSessions[p.ID+"/"+j.ID]; ok && sessionID == "" {
			sessionID = sess.ID
		}

		jobDetail := &Detail{}
		if j.DependsOn != nil {
			jobDetail.DependsOn = j.DependsOn
		} else {
			jobDetail.DependsOn = []string{}
		}

		jobNodes = append(jobNodes, &Node{
			Type:      NodeTypeJob,
			ID:        "pipeline:" + p.ID + "/job:" + j.ID,
			Label:     j.ID,
			Status:    jobStatus(j.Status),
			SessionID: sessionID,
			Detail:    jobDetail,
		})
	}

	return &Node{
		Type:     NodeTypePipeline,
		ID:       "pipeline:" + p.ID,
		Label:    p.Name,
		Status:   pipelineStatus(p.Status),
		Detail:   &Detail{Repo: p.Repo, PlanID: p.PlanID},
		Children: jobNodes,
	}
}

func sortJobs(jobs []pipeline.Job) []pipeline.Job {
	if len(jobs) <= 1 {
		return jobs
	}
	declOrder := make(map[string]int, len(jobs))
	jobMap := make(map[string]pipeline.Job, len(jobs))
	for i, j := range jobs {
		declOrder[j.ID] = i
		jobMap[j.ID] = j
	}

	inDegree := make(map[string]int, len(jobs))
	dependents := make(map[string][]string, len(jobs))
	for _, j := range jobs {
		depCount := 0
		for _, dep := range j.DependsOn {
			if _, ok := jobMap[dep]; ok {
				depCount++
				dependents[dep] = append(dependents[dep], j.ID)
			}
		}
		inDegree[j.ID] = depCount
	}

	ready := make([]string, 0, len(jobs))
	for _, j := range jobs {
		if inDegree[j.ID] == 0 {
			ready = append(ready, j.ID)
		}
	}

	var ordered []pipeline.Job
	for len(ready) > 0 {
		sort.SliceStable(ready, func(i, j int) bool {
			return declOrder[ready[i]] < declOrder[ready[j]]
		})
		currID := ready[0]
		ready = ready[1:]
		ordered = append(ordered, jobMap[currID])

		for _, depID := range dependents[currID] {
			inDegree[depID]--
			if inDegree[depID] == 0 {
				ready = append(ready, depID)
			}
		}
	}

	if len(ordered) < len(jobs) {
		visited := make(map[string]bool, len(ordered))
		for _, j := range ordered {
			visited[j.ID] = true
		}
		for _, j := range jobs {
			if !visited[j.ID] {
				ordered = append(ordered, j)
			}
		}
	}

	return ordered
}

func buildAgentSubtree(
	s *store.Session,
	childrenByParent map[string][]*store.Session,
	ownedPipelinesByAgent map[string][]*pipeline.Pipeline,
	pipelineJobSessions map[string]*store.Session,
) *Node {
	label := s.Name
	if label == "" {
		label = s.ID
	}
	node := &Node{
		Type:      NodeTypeAgent,
		ID:        "session:" + s.ID,
		Label:     label,
		Status:    sessionStatus(s.Status, s.ExitCode),
		SessionID: s.ID,
		Detail:    &Detail{Kind: "agent", AiCli: backendOr(s), PlanID: s.PlanID},
	}

	for _, kid := range childrenByParent[s.ID] {
		node.Children = append(node.Children, buildAgentSubtree(kid, childrenByParent, ownedPipelinesByAgent, pipelineJobSessions))
	}
	for _, p := range ownedPipelinesByAgent[s.ID] {
		node.Children = append(node.Children, buildPipelineNode(p, pipelineJobSessions))
	}
	return node
}

func buildTerminalNode(t *store.Session) *Node {
	return &Node{
		Type:      NodeTypeTerminal,
		ID:        "session:" + t.ID,
		Label:     terminalDisplayName(t),
		Status:    terminalStatus(t.Status, t.ExitCode),
		SessionID: t.ID,
		Detail:    &Detail{Kind: "terminal"},
	}
}

func orderSessionsByList(sessions []*store.Session, ids []string) []*store.Session {
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	out := make([]*store.Session, 0, len(sessions))
	seen := make(map[string]bool, len(sessions))
	for _, id := range ids {
		if s := byID[id]; s != nil && !seen[id] {
			out = append(out, s)
			seen[id] = true
		}
	}
	var rest []*store.Session
	for _, s := range sessions {
		if !seen[s.ID] {
			rest = append(rest, s)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].ID < rest[j].ID })
	return append(out, rest...)
}

func orderPipelinesByList(pipes []*pipeline.Pipeline, ids []string) []*pipeline.Pipeline {
	byID := make(map[string]*pipeline.Pipeline, len(pipes))
	for _, p := range pipes {
		byID[p.ID] = p
	}
	out := make([]*pipeline.Pipeline, 0, len(pipes))
	seen := make(map[string]bool, len(pipes))
	for _, id := range ids {
		if p := byID[id]; p != nil && !seen[id] {
			out = append(out, p)
			seen[id] = true
		}
	}
	var rest []*pipeline.Pipeline
	for _, p := range pipes {
		if !seen[p.ID] {
			rest = append(rest, p)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append(out, rest...)
}

func orderPlansByList(plans []*planstore.Plan, ids []string) []*planstore.Plan {
	byID := make(map[string]*planstore.Plan, len(plans))
	for _, p := range plans {
		byID[p.ID] = p
	}
	out := make([]*planstore.Plan, 0, len(plans))
	seen := make(map[string]bool, len(plans))
	for _, id := range ids {
		if p := byID[id]; p != nil && !seen[id] {
			out = append(out, p)
			seen[id] = true
		}
	}
	var rest []*planstore.Plan
	for _, p := range plans {
		if !seen[p.ID] {
			rest = append(rest, p)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Name < rest[j].Name })
	return append(out, rest...)
}

func orderAutopilotsByList(aps []*autopilotstore.Autopilot, ids []string) []*autopilotstore.Autopilot {
	byID := make(map[string]*autopilotstore.Autopilot, len(aps))
	for _, a := range aps {
		byID[a.ID] = a
	}
	out := make([]*autopilotstore.Autopilot, 0, len(aps))
	seen := make(map[string]bool, len(aps))
	for _, id := range ids {
		if a := byID[id]; a != nil && !seen[id] {
			out = append(out, a)
			seen[id] = true
		}
	}
	var rest []*autopilotstore.Autopilot
	for _, a := range aps {
		if !seen[a.ID] {
			rest = append(rest, a)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].ID < rest[j].ID })
	return append(out, rest...)
}

func sortAgents(sessions []*store.Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		aLive := isLive(a.Status)
		bLive := isLive(b.Status)
		if aLive != bLive {
			return aLive
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

func sortTerminals(terminals []*store.Session) {
	sort.SliceStable(terminals, func(i, j int) bool {
		a, b := terminals[i], terminals[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID < b.ID
	})
}

type rootCategory int

const (
	catOpenProject rootCategory = iota
	catClosedProject
	catLooseDir
	catSynthetic
)

func sortRoots(roots []*Node, projectByID map[string]projectstore.Project, isClosedProject map[string]bool) {
	getCategory := func(n *Node) rootCategory {
		if n.Detail != nil && n.Detail.Synthetic {
			return catSynthetic
		}
		rawID := strings.TrimPrefix(n.ID, "project:")
		if _, ok := projectByID[rawID]; ok {
			if isClosedProject[rawID] {
				return catClosedProject
			}
			return catOpenProject
		}
		if n.Detail != nil && n.Detail.Closed {
			return catClosedProject
		}
		return catLooseDir
	}

	sort.SliceStable(roots, func(i, j int) bool {
		catI := getCategory(roots[i])
		catJ := getCategory(roots[j])
		if catI != catJ {
			return catI < catJ
		}
		labelI := strings.ToLower(roots[i].Label)
		labelJ := strings.ToLower(roots[j].Label)
		if labelI != labelJ {
			return labelI < labelJ
		}
		return roots[i].ID < roots[j].ID
	})
}

func isHeadlessBrainSession(s *store.Session) bool {
	if s == nil {
		return false
	}
	return s.Role == "brain" && s.HasTag("system:true")
}

func containsID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
