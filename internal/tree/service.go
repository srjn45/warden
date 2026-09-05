package tree

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/pipeline"
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

	// Group pipelines by group key
	pipelinesByGroup := map[string][]*pipeline.Pipeline{}
	for _, p := range in.Pipelines {
		dir := canonicalDir(p.Repo)
		key := resolveGroupKey(p.ProjectID, dir, openByKey, closedByKey)
		pipelinesByGroup[key] = append(pipelinesByGroup[key], p)
	}

	// Group autopilot runs by group key
	runsByGroup := map[string][]autopilot.RunStatus{}
	for i := range in.Autopilot.Runs {
		r := in.Autopilot.Runs[i]
		dir := canonicalDir(r.Repo)
		key := resolveGroupKey("", dir, openByKey, closedByKey)
		runsByGroup[key] = append(runsByGroup[key], r)
	}

	// Partition sessions by exactly-once nesting precedence (spec §7):
	// 1. autopilot_run_id
	// 2. pipeline_id + job_id
	// 3. parent_id within same project (agent forest)
	// 4. resolved project
	autopilotSessionsByRun := map[string][]*store.Session{}
	isAutopilotSession := map[string]bool{}

	for _, sess := range in.Sessions {
		rid := sessionRunID(sess)
		if rid != "" {
			autopilotSessionsByRun[rid] = append(autopilotSessionsByRun[rid], sess)
			isAutopilotSession[sess.ID] = true
			continue
		}
		// Also match sessions explicitly designated as guardian or brain in run status
		for i := range in.Autopilot.Runs {
			r := &in.Autopilot.Runs[i]
			if (r.GuardianID != "" && sess.ID == r.GuardianID) || (r.Brain != nil && sess.ID == r.Brain.AgentID) {
				autopilotSessionsByRun[r.RunID] = append(autopilotSessionsByRun[r.RunID], sess)
				isAutopilotSession[sess.ID] = true
				break
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
		if sess.IsTerminal() {
			key := resolveGroupKey(sess.ProjectID, sessionDir(sess), openByKey, closedByKey)
			terminalsByGroup[key] = append(terminalsByGroup[key], sess)
		} else {
			agents = append(agents, sess)
		}
	}

	agentRoots, childrenByParent := agentForest(agents)
	agentRootsByGroup := map[string][]*store.Session{}
	for _, sess := range agentRoots {
		key := resolveGroupKey(sess.ProjectID, sessionDir(sess), openByKey, closedByKey)
		agentRootsByGroup[key] = append(agentRootsByGroup[key], sess)
	}

	// Identify all group keys to render
	groupKeySet := map[string]bool{}
	for _, p := range in.Projects {
		groupKeySet[p.ID] = true
	}
	for key := range pipelinesByGroup {
		if key != "" {
			groupKeySet[key] = true
		}
	}
	for key := range runsByGroup {
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

	// Include synthetic bucket "" if it contains any items
	hasSyntheticItems := len(pipelinesByGroup[""]) > 0 || len(runsByGroup[""]) > 0 ||
		len(agentRootsByGroup[""]) > 0 || len(terminalsByGroup[""]) > 0
	if hasSyntheticItems {
		groupKeySet[""] = true
	}

	// Build nodes for each group
	var rootNodes []*Node
	for key := range groupKeySet {
		groupChildren := buildGroupChildren(
			key,
			runsByGroup[key],
			autopilotSessionsByRun,
			pipelinesByGroup[key],
			pipelineJobSessions,
			agentRootsByGroup[key],
			childrenByParent,
			terminalsByGroup[key],
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
			// Loose directory group
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

	// Apply degradation marking (spec §12)
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

	// Canonical root ordering (spec §8):
	// Open projects (alpha by label) -> closed projects (alpha) -> loose dirs (alpha) -> "No project" last.
	sortRoots(rootNodes, projectByID, isClosedProject)

	// Scoping by projectID if specified (spec §2, §9)
	if projectID != "" {
		for _, root := range rootNodes {
			// Do not return synthetic bucket when projectID is scoped
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

// buildGroupChildren constructs the ordered children of a project/group node in canonical order:
// autopilot runs -> pipelines -> agent subtrees -> terminals (spec §8).
func buildGroupChildren(
	_ string,
	runs []autopilot.RunStatus,
	autopilotSessionsByRun map[string][]*store.Session,
	pipelines []*pipeline.Pipeline,
	pipelineJobSessions map[string]*store.Session,
	agentRoots []*store.Session,
	childrenByParent map[string][]*store.Session,
	terminals []*store.Session,
) []*Node {
	var children []*Node

	// 1. Autopilot runs
	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].RunID < runs[j].RunID
	})
	for i := range runs {
		r := &runs[i]
		children = append(children, buildAutopilotRunNode(r, autopilotSessionsByRun[r.RunID]))
	}

	// 2. Pipelines
	sort.SliceStable(pipelines, func(i, j int) bool {
		return pipelines[i].Name < pipelines[j].Name
	})
	for _, p := range pipelines {
		children = append(children, buildPipelineNode(p, pipelineJobSessions))
	}

	// 3. Agent subtrees
	sortAgents(agentRoots)
	for _, s := range agentRoots {
		children = append(children, buildAgentSubtree(s, childrenByParent))
	}

	// 4. Terminals
	sortTerminals(terminals)
	for _, t := range terminals {
		children = append(children, buildTerminalNode(t))
	}

	return children
}

// buildAutopilotRunNode creates an autopilot_run node with manager, guardian, and task children (spec §4, §8).
func buildAutopilotRunNode(r *autopilot.RunStatus, sessions []*store.Session) *Node {
	var managerSess *store.Session
	var guardianSess *store.Session
	var workerSessions []*store.Session

	for _, s := range sessions {
		switch runSessionSlot(s, r) {
		case store.AutopilotSlotManager:
			if managerSess == nil {
				managerSess = s
			}
		case store.AutopilotSlotGuardian:
			if guardianSess == nil {
				guardianSess = s
			}
		default:
			workerSessions = append(workerSessions, s)
		}
	}

	workersByTask := map[string][]*store.Session{}
	var looseWorkers []*store.Session
	for _, w := range workerSessions {
		if w.AutopilotTaskID != "" {
			workersByTask[w.AutopilotTaskID] = append(workersByTask[w.AutopilotTaskID], w)
		} else {
			looseWorkers = append(looseWorkers, w)
		}
	}

	// Order tasks: ledger order first, then remaining plan tasks, then worker task IDs (spec §8)
	seenTasks := map[string]bool{}
	var taskIDs []string
	for _, lt := range r.LedgerTasks {
		if lt.ID != "" && !seenTasks[lt.ID] {
			seenTasks[lt.ID] = true
			taskIDs = append(taskIDs, lt.ID)
		}
	}
	planTaskByID := map[string]autopilot.PlanTask{}
	for _, pt := range r.PlanTasks {
		planTaskByID[pt.ID] = pt
		if pt.ID != "" && !seenTasks[pt.ID] {
			seenTasks[pt.ID] = true
			taskIDs = append(taskIDs, pt.ID)
		}
	}
	for tid := range workersByTask {
		if !seenTasks[tid] {
			seenTasks[tid] = true
			taskIDs = append(taskIDs, tid)
		}
	}

	var runChildren []*Node

	// Manager
	if managerSess != nil {
		mLabel := managerSess.Name
		if mLabel == "" {
			mLabel = "manager"
		}
		runChildren = append(runChildren, &Node{
			Type:      NodeTypeManager,
			ID:        "session:" + managerSess.ID,
			Label:     mLabel,
			Status:    sessionStatus(managerSess.Status),
			SessionID: managerSess.ID,
			Detail:    &Detail{Kind: "agent", Slot: "autopilot"},
		})
	}

	// Guardian
	if guardianSess != nil {
		gLabel := guardianSess.Name
		if gLabel == "" {
			gLabel = "guardian"
		}
		runChildren = append(runChildren, &Node{
			Type:      NodeTypeGuardian,
			ID:        "session:" + guardianSess.ID,
			Label:     gLabel,
			Status:    sessionStatus(guardianSess.Status),
			SessionID: guardianSess.ID,
			Detail:    &Detail{Kind: "agent", Slot: "guardian"},
		})
	}

	// Tasks
	for _, tid := range taskIDs {
		pt, hasPlan := planTaskByID[tid]
		label := tid
		if hasPlan && pt.Prompt != "" {
			label = pt.Prompt
		}

		workers := workersByTask[tid]
		sortAgents(workers)

		var workerNodes []*Node
		for _, w := range workers {
			wLabel := w.Name
			if wLabel == "" {
				wLabel = w.ID
			}
			workerNodes = append(workerNodes, &Node{
				Type:      NodeTypeWorker,
				ID:        "session:" + w.ID,
				Label:     wLabel,
				Status:    sessionStatus(w.Status),
				SessionID: w.ID,
				Detail:    &Detail{Kind: "agent", Slot: "worker"},
			})
		}

		taskNode := &Node{
			Type:   NodeTypeTask,
			ID:     "run:" + r.RunID + "/task:" + tid,
			Label:  label,
			Status: taskStatus(workerNodes),
		}
		if len(workerNodes) > 0 {
			taskNode.Children = workerNodes
		}
		runChildren = append(runChildren, taskNode)
	}

	// Loose workers not attached to a task
	sortAgents(looseWorkers)
	for _, w := range looseWorkers {
		wLabel := w.Name
		if wLabel == "" {
			wLabel = w.ID
		}
		runChildren = append(runChildren, &Node{
			Type:      NodeTypeWorker,
			ID:        "session:" + w.ID,
			Label:     wLabel,
			Status:    sessionStatus(w.Status),
			SessionID: w.ID,
			Detail:    &Detail{Kind: "agent", Slot: "worker"},
		})
	}

	detail := &Detail{
		Repo: r.Repo,
		Gate: r.Gate,
	}

	runNode := &Node{
		Type:     NodeTypeAutopilotRun,
		ID:       "run:" + r.RunID,
		Label:    r.Name,
		Status:   runStatus(*r),
		Detail:   detail,
		Children: runChildren,
	}
	return runNode
}

// buildPipelineNode creates a pipeline node with job children ordered topologically (spec §4, §8).
func buildPipelineNode(p *pipeline.Pipeline, jobSessions map[string]*store.Session) *Node {
	orderedJobs := sortJobs(p.Jobs)
	var jobNodes []*Node
	for _, j := range orderedJobs {
		sessionID := j.SessionID
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
		Detail:   &Detail{Repo: p.Repo},
		Children: jobNodes,
	}
}

// sortJobs orders pipeline jobs topologically by depends_on, preserving declaration order (spec §8).
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

// buildAgentSubtree recursively builds an agent node and its same-project children (spec §7, §8).
func buildAgentSubtree(s *store.Session, childrenByParent map[string][]*store.Session) *Node {
	label := s.Name
	if label == "" {
		label = s.ID
	}
	node := &Node{
		Type:      NodeTypeAgent,
		ID:        "session:" + s.ID,
		Label:     label,
		Status:    sessionStatus(s.Status),
		SessionID: s.ID,
		Detail:    &Detail{Kind: "agent", Backend: backendOr(s)},
	}

	kids := childrenByParent[s.ID]
	if len(kids) > 0 {
		sortAgents(kids)
		for _, kid := range kids {
			node.Children = append(node.Children, buildAgentSubtree(kid, childrenByParent))
		}
	}
	return node
}

// buildTerminalNode builds a terminal node (spec §4).
func buildTerminalNode(t *store.Session) *Node {
	return &Node{
		Type:      NodeTypeTerminal,
		ID:        "session:" + t.ID,
		Label:     terminalDisplayName(t),
		Status:    sessionStatus(t.Status),
		SessionID: t.ID,
		Detail:    &Detail{Kind: "terminal"},
	}
}

// sortAgents sorts sibling agents: live first, then by creation time ascending, then by ID (spec §8).
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

// sortTerminals sorts terminals by creation time ascending, then by ID.
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

// sortRoots sorts root nodes into canonical order (spec §8):
// open registered projects -> closed registered projects -> loose dirs -> synthetic bucket last.
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
