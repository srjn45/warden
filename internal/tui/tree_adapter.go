package tui

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/autopilot"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
	"github.com/srjn45/warden/internal/tree"
)

// treeViewOpts carries the view-layer inputs the adapter applies on top of a
// tree.Tree: collapse/cursor identity (composite node ids), project-group labels,
// opened-dir placeholders, and entity lookups render functions still need.
type treeViewOpts struct {
	collapsed      map[string]bool
	groupByProject map[string]string
	opened         map[string]time.Time
	projects       []projectstore.Project
	sessions       []*store.Session
	pipelines      []*pipeline.Pipeline
	runs           []client.AutopilotRunStatus
	skipTerminals  bool // Projects tab: terminals live on the Terminals tab
}

// buildProjectItems is the N6 entry point: tree.Service.Build for structure, then
// the view adapter for collapse / home-abbrev / backlink labels → []item.
func buildProjectItems(
	projects []projectstore.Project,
	groups []projectstore.ProjectGroup,
	sessions []*store.Session,
	pipelines []*pipeline.Pipeline,
	ap client.AutopilotStatus,
	opened map[string]time.Time,
	collapsed map[string]bool,
	showSystem bool,
) []item {
	filtered := filterTreeSessions(sessions, showSystem)
	tr := tree.NewService().Build(tree.Inputs{
		Sessions:  filtered,
		Projects:  projects,
		Pipelines: pipelines,
		Autopilot: autopilotFromClient(ap),
		Groups:    groups,
	}, "")
	return adaptTree(tr, treeViewOpts{
		collapsed:      collapsed,
		groupByProject: groupLabels(groups),
		opened:         opened,
		projects:       projects,
		sessions:       filtered,
		pipelines:      pipelines,
		runs:           ap.Runs,
		skipTerminals:  true,
	})
}

// filterTreeSessions drops system-tagged agents the operator has hidden, but
// keeps autopilot-owned sessions (guardians are tagged system:true and must
// still appear under their run).
func filterTreeSessions(sessions []*store.Session, showSystem bool) []*store.Session {
	if showSystem {
		return sessions
	}
	out := make([]*store.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.HasTag("system:true") && !isAutopilotOwned(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// autopilotFromClient bridges the hand-written client wire types onto the
// autopilot package types tree.Service expects. They share JSON shape (oapi
// aliases AutopilotStatus = autopilot.Status) but are not Go type aliases.
func autopilotFromClient(c client.AutopilotStatus) autopilot.Status {
	b, err := json.Marshal(c)
	if err != nil {
		return autopilot.Status{}
	}
	var out autopilot.Status
	if err := json.Unmarshal(b, &out); err != nil {
		return autopilot.Status{}
	}
	return out
}

// adaptTree walks Tree.Roots, applies view state, and flattens to []item for the
// existing renderList / buildRows path. Render functions are untouched.
func adaptTree(tr *tree.Tree, opts treeViewOpts) []item {
	if tr == nil {
		return nil
	}
	ctx := newAdaptCtx(opts)
	var items []item
	seenProjects := map[string]bool{}
	var closedChildren []*tree.Node // re-homed when the TUI hides closed projects
	var synthetic *tree.Node
	for _, root := range tr.Roots {
		if root == nil || root.Type != tree.NodeTypeProject {
			continue
		}
		if root.Detail != nil && root.Detail.Synthetic {
			synthetic = root
			continue
		}
		if root.Detail != nil && root.Detail.Closed {
			// Spec D4: service keeps closed projects; TUI hides them. Park their
			// visible children in the No-project bucket (prior Ungrouped behavior).
			closedChildren = append(closedChildren, ctx.visibleChildren(root.Children)...)
			continue
		}
		seenProjects[root.ID] = true
		items = append(items, ctx.adaptProject(root)...)
	}
	if synthetic != nil || len(closedChildren) > 0 {
		syn := synthetic
		if syn == nil {
			syn = &tree.Node{
				Type:   tree.NodeTypeProject,
				ID:     "project:__none__",
				Label:  "No project",
				Detail: &tree.Detail{Synthetic: true},
			}
		}
		if len(closedChildren) > 0 {
			merged := append([]*tree.Node{}, ctx.visibleChildren(syn.Children)...)
			merged = append(merged, closedChildren...)
			syn = &tree.Node{
				Type:     syn.Type,
				ID:       syn.ID,
				Label:    syn.Label,
				Status:   syn.Status,
				Detail:   syn.Detail,
				Children: merged,
			}
		}
		seenProjects[syn.ID] = true
		items = append(items, ctx.adaptProject(syn)...)
	}
	items = ctx.injectOpenedDirs(items, seenProjects)
	return items
}

type adaptCtx struct {
	collapsed      map[string]bool
	groupByProject map[string]string
	opened         map[string]time.Time
	openMeta       map[string]projectstore.Project // open project id → project
	sessionsByID   map[string]*store.Session
	pipelinesByID  map[string]*pipeline.Pipeline
	runsByID       map[string]*client.AutopilotRunStatus
	jobsByKey      map[string]*pipeline.Job // "pipeID/jobID"
	skipTerminals  bool
}

func newAdaptCtx(opts treeViewOpts) *adaptCtx {
	ctx := &adaptCtx{
		collapsed:      opts.collapsed,
		groupByProject: opts.groupByProject,
		opened:         opts.opened,
		openMeta:       map[string]projectstore.Project{},
		sessionsByID:   make(map[string]*store.Session, len(opts.sessions)),
		pipelinesByID:  make(map[string]*pipeline.Pipeline, len(opts.pipelines)),
		runsByID:       make(map[string]*client.AutopilotRunStatus, len(opts.runs)),
		jobsByKey:      map[string]*pipeline.Job{},
		skipTerminals:  opts.skipTerminals,
	}
	if ctx.collapsed == nil {
		ctx.collapsed = map[string]bool{}
	}
	if ctx.groupByProject == nil {
		ctx.groupByProject = map[string]string{}
	}
	for _, p := range opts.projects {
		if projectstore.NormalizeStatus(p.Status) == projectstore.StatusClosed {
			continue
		}
		ctx.openMeta[p.ID] = p
	}
	for _, s := range opts.sessions {
		ctx.sessionsByID[s.ID] = s
	}
	for _, p := range opts.pipelines {
		ctx.pipelinesByID[p.ID] = p
		for i := range p.Jobs {
			j := &p.Jobs[i]
			ctx.jobsByKey[p.ID+"/"+j.ID] = j
		}
	}
	for i := range opts.runs {
		r := &opts.runs[i]
		ctx.runsByID[r.RunID] = r
	}
	return ctx
}

func (ctx *adaptCtx) adaptProject(n *tree.Node) []item {
	rawID := strings.TrimPrefix(n.ID, "project:")
	synthetic := n.Detail != nil && n.Detail.Synthetic

	hdr := &projectHeader{id: rawID, name: n.Label, group: ctx.groupByProject[rawID]}
	switch {
	case synthetic:
		hdr.id = ""
		hdr.isProject = false
	default:
		if p, ok := ctx.openMeta[rawID]; ok {
			hdr.isProject = true
			hdr.path = p.Path
			if hdr.path == "" {
				hdr.path = p.ID
			}
			if hdr.name == "" {
				hdr.name = filepath.Base(p.ID)
			}
		} else {
			hdr.path = rawID
			hdr.isProject = false
			if hdr.name == "" {
				hdr.name = filepath.Base(rawID)
			}
		}
	}

	children := ctx.visibleChildren(n.Children)
	hdr.agentCount, hdr.liveAgents = countAgents(children, ctx.sessionsByID)

	collapsed := ctx.collapsed[n.ID]
	items := []item{{projHdr: hdr, dir: hdr.path, collapsed: collapsed}}
	if collapsed {
		return items
	}
	if len(children) == 0 {
		items = append(items, item{dir: hdr.path, underProject: true})
		return items
	}
	for _, ch := range children {
		items = append(items, ctx.adaptNode(ch, 0)...)
	}
	return items
}

func (ctx *adaptCtx) visibleChildren(kids []*tree.Node) []*tree.Node {
	if !ctx.skipTerminals {
		return kids
	}
	out := make([]*tree.Node, 0, len(kids))
	for _, ch := range kids {
		if ch != nil && ch.Type == tree.NodeTypeTerminal {
			continue
		}
		out = append(out, ch)
	}
	return out
}

func countAgents(nodes []*tree.Node, sessions map[string]*store.Session) (total, live int) {
	var walk func([]*tree.Node)
	walk = func(ns []*tree.Node) {
		for _, n := range ns {
			if n == nil {
				continue
			}
			switch n.Type {
			case tree.NodeTypeAgent, tree.NodeTypeManager, tree.NodeTypeGuardian, tree.NodeTypeWorker:
				total++
				if s := sessions[n.SessionID]; s != nil && liveStatus(s.Status) {
					live++
				}
			}
			walk(n.Children)
		}
	}
	walk(nodes)
	return total, live
}

func (ctx *adaptCtx) adaptNode(n *tree.Node, depth int) []item {
	if n == nil {
		return nil
	}
	switch n.Type {
	case tree.NodeTypeAutopilotRun:
		return ctx.adaptRun(n)
	case tree.NodeTypePipeline:
		return ctx.adaptPipeline(n)
	case tree.NodeTypeAgent:
		return ctx.adaptAgent(n, depth)
	case tree.NodeTypeManager, tree.NodeTypeGuardian, tree.NodeTypeWorker:
		return ctx.adaptSlotSession(n, depth)
	case tree.NodeTypeTask:
		return ctx.adaptTask(n)
	case tree.NodeTypeTerminal:
		if ctx.skipTerminals {
			return nil
		}
		return ctx.adaptTerminal(n)
	case tree.NodeTypeJob:
		return ctx.adaptJob(n)
	default:
		var items []item
		for _, ch := range n.Children {
			items = append(items, ctx.adaptNode(ch, depth)...)
		}
		return items
	}
}

func (ctx *adaptCtx) adaptRun(n *tree.Node) []item {
	runID := strings.TrimPrefix(n.ID, "run:")
	r := ctx.runsByID[runID]
	if r == nil {
		// Synthesize a minimal run so the row still renders if status lagged.
		r = &client.AutopilotRunStatus{RunID: runID, Name: n.Label, State: n.Status}
		if n.Detail != nil {
			r.Repo = n.Detail.Repo
			r.Gate = n.Detail.Gate
		}
	}
	collapsed := ctx.collapsed[n.ID]
	items := []item{{apRun: r, collapsed: collapsed, underProject: true}}
	if collapsed {
		return items
	}
	for _, ch := range n.Children {
		items = append(items, ctx.adaptNode(ch, 1)...)
	}
	return items
}

func (ctx *adaptCtx) adaptTask(n *tree.Node) []item {
	// run:<runID>/task:<taskID>
	runID, taskID := splitRunTaskID(n.ID)
	pt := &client.AutopilotPlanTask{ID: taskID, Prompt: n.Label, Status: n.Status}
	if r := ctx.runsByID[runID]; r != nil {
		for i := range r.PlanTasks {
			if r.PlanTasks[i].ID == taskID {
				pt = &r.PlanTasks[i]
				break
			}
		}
		for _, lt := range r.LedgerTasks {
			if lt.ID == taskID && pt.Status == "" {
				pt.Status = lt.State
			}
		}
	}
	collapsed := ctx.collapsed[n.ID]
	hasKids := len(n.Children) > 0
	items := []item{{
		apTask:       pt,
		apTaskRun:    runID,
		hasKids:      hasKids,
		collapsed:    collapsed,
		underProject: true,
	}}
	// Also set apWorkerGroup so Left on a task collapses via existing handlers
	// that key off worker-group/task identity — itemKey prefers apTask composite id.
	if collapsed {
		return items
	}
	for _, ch := range n.Children {
		items = append(items, ctx.adaptNode(ch, 2)...)
	}
	return items
}

func splitRunTaskID(id string) (runID, taskID string) {
	// "run:<runID>/task:<taskID>"
	rest := strings.TrimPrefix(id, "run:")
	parts := strings.SplitN(rest, "/task:", 2)
	if len(parts) != 2 {
		return rest, ""
	}
	return parts[0], parts[1]
}

func (ctx *adaptCtx) adaptPipeline(n *tree.Node) []item {
	pipeID := strings.TrimPrefix(n.ID, "pipeline:")
	p := ctx.pipelinesByID[pipeID]
	if p == nil {
		p = &pipeline.Pipeline{ID: pipeID, Name: n.Label, Status: pipeline.Status(n.Status)}
		if n.Detail != nil {
			p.Repo = n.Detail.Repo
		}
	}
	collapsed := ctx.collapsed[n.ID]
	items := []item{{pipeline: p, collapsed: collapsed, underProject: true}}
	if collapsed {
		return items
	}
	for _, ch := range n.Children {
		items = append(items, ctx.adaptNode(ch, 0)...)
	}
	return items
}

func (ctx *adaptCtx) adaptJob(n *tree.Node) []item {
	// pipeline:<pipeID>/job:<jobID>
	pipeID, jobID := splitPipeJobID(n.ID)
	j := ctx.jobsByKey[pipeID+"/"+jobID]
	if j == nil {
		j = &pipeline.Job{ID: jobID, Status: pipeline.JobStatus(n.Status), SessionID: n.SessionID}
		if n.Detail != nil {
			j.DependsOn = n.Detail.DependsOn
		}
	}
	var sess *store.Session
	if n.SessionID != "" {
		sess = ctx.sessionsByID[n.SessionID]
	}
	return []item{{pjPipe: pipeID, pjJob: j, pjSess: sess, underProject: true}}
}

func splitPipeJobID(id string) (pipeID, jobID string) {
	rest := strings.TrimPrefix(id, "pipeline:")
	parts := strings.SplitN(rest, "/job:", 2)
	if len(parts) != 2 {
		return rest, ""
	}
	return parts[0], parts[1]
}

func (ctx *adaptCtx) adaptAgent(n *tree.Node, depth int) []item {
	s := ctx.sessionsByID[n.SessionID]
	if s == nil {
		return nil
	}
	kids := n.Children
	collapsed := ctx.collapsed[n.ID]
	it := item{
		session:      s,
		dir:          sourceDir(s),
		depth:        depth,
		hasKids:      len(kids) > 0,
		collapsed:    collapsed,
		underProject: true,
		fromParent:   ctx.backlink(s),
	}
	if it.hasKids && !liveStatus(s.Status) {
		it.tombstone = true
		it.runningKids = countLiveUnder(kids, ctx.sessionsByID)
	}
	items := []item{it}
	if it.hasKids && !collapsed {
		for _, ch := range kids {
			items = append(items, ctx.adaptNode(ch, depth+1)...)
		}
	}
	return items
}

func (ctx *adaptCtx) adaptSlotSession(n *tree.Node, depth int) []item {
	s := ctx.sessionsByID[n.SessionID]
	if s == nil {
		return nil
	}
	slot := ""
	if n.Detail != nil {
		slot = n.Detail.Slot
	}
	return []item{{
		session:      s,
		dir:          sourceDir(s),
		depth:        depth,
		underProject: true,
		apSlot:       slot,
	}}
}

func (ctx *adaptCtx) adaptTerminal(n *tree.Node) []item {
	s := ctx.sessionsByID[n.SessionID]
	if s == nil {
		return nil
	}
	return []item{{session: s, dir: sourceDir(s), termName: n.Label, underProject: true}}
}

// backlink reconstructs the §4.1 "↳ from <parent>" label for a cross-project
// child. The tree keeps the structural edge (child is a root under its own
// project); the label is view-only from session.ParentID.
func (ctx *adaptCtx) backlink(s *store.Session) string {
	if s.ParentID == "" {
		return ""
	}
	parent := ctx.sessionsByID[s.ParentID]
	if parent == nil {
		return ""
	}
	if sourceDir(s) == sourceDir(parent) {
		return "" // same-project children nest structurally; no backlink
	}
	return parentLabel(parent)
}

func countLiveUnder(nodes []*tree.Node, sessions map[string]*store.Session) int {
	n := 0
	var walk func([]*tree.Node)
	walk = func(ns []*tree.Node) {
		for _, node := range ns {
			if node == nil {
				continue
			}
			if s := sessions[node.SessionID]; s != nil && liveStatus(s.Status) {
				n++
			}
			walk(node.Children)
		}
	}
	walk(nodes)
	return n
}

// injectOpenedDirs adds empty placeholders for directories the operator opened
// (o → Local/Remote/New) that are not already represented as tree roots.
func (ctx *adaptCtx) injectOpenedDirs(items []item, seenProjects map[string]bool) []item {
	if len(ctx.opened) == 0 {
		return items
	}
	var extra []item
	for dir := range ctx.opened {
		id := "project:" + dir
		if seenProjects[id] {
			continue
		}
		// Skip if a project header for this path already exists.
		already := false
		for _, it := range items {
			if it.projHdr != nil && (it.projHdr.id == dir || it.projHdr.path == dir) {
				already = true
				break
			}
		}
		if already {
			continue
		}
		hdr := &projectHeader{id: dir, name: filepath.Base(dir), path: dir}
		collapsed := ctx.collapsed[id]
		extra = append(extra, item{projHdr: hdr, dir: dir, collapsed: collapsed})
		if !collapsed {
			extra = append(extra, item{dir: dir, underProject: true})
		}
	}
	if len(extra) == 0 {
		return items
	}
	// Insert before the synthetic "No project" bucket when present.
	for i, it := range items {
		if it.projHdr != nil && it.projHdr.id == "" {
			out := make([]item, 0, len(items)+len(extra))
			out = append(out, items[:i]...)
			out = append(out, extra...)
			out = append(out, items[i:]...)
			return out
		}
	}
	return append(items, extra...)
}
