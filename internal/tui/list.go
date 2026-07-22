package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/srjn45/warden/internal/approval"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/digest"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/store"
)

func age(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// sourceDir is the grouping key: the directory the warden command was
// triggered from. Repo (set for typed/worktree agents) wins; otherwise Workdir
// (the caller cwd for prompt agents); "—" when neither is known.
func sourceDir(s *store.Session) string {
	if s.Repo != "" {
		return s.Repo
	}
	if s.Workdir != "" {
		return s.Workdir
	}
	return "—"
}

// homeDir resolves the user's home directory once per process; abbrevHome runs
// on every render tick, so we avoid repeating the os.UserHomeDir() syscall.
var homeDir = sync.OnceValue(func() string {
	h, _ := os.UserHomeDir()
	return h
})

// abbrevHome replaces a leading $HOME with ~ for display (TUI runs locally).
func abbrevHome(path string) string {
	return abbrevHomeWith(path, homeDir())
}

// abbrevHomeWith is the pure core of abbrevHome with the home dir injected.
// An empty home (lookup failed) means no abbreviation.
func abbrevHomeWith(path, home string) string {
	if home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// groupSort returns sessions re-ordered so agents sharing a sourceDir are
// contiguous. Groups are ordered by their newest agent's CreatedAt (desc); within
// a group agents are ordered by CreatedAt (desc). The ordering keys on the
// immutable CreatedAt rather than UpdatedAt so an agent's row is fixed at creation
// and does not shuffle as it works (UpdatedAt bumps on every action, which made
// the list churn constantly). Pure: returns a new slice, leaves the input untouched.
func groupSort(sessions []*store.Session) []*store.Session {
	if len(sessions) < 2 {
		return sessions
	}
	type grp struct {
		max  time.Time
		seen int
		rank int
	}
	groups := map[string]*grp{}
	var keys []string
	for i, s := range sessions {
		k := sourceDir(s)
		g := groups[k]
		if g == nil {
			groups[k] = &grp{max: s.CreatedAt, seen: i}
			keys = append(keys, k)
			continue
		}
		if s.CreatedAt.After(g.max) {
			g.max = s.CreatedAt
		}
	}
	sort.SliceStable(keys, func(a, b int) bool {
		ga, gb := groups[keys[a]], groups[keys[b]]
		if ga.max.Equal(gb.max) {
			return ga.seen < gb.seen
		}
		return ga.max.After(gb.max)
	})
	for r, k := range keys {
		groups[k].rank = r
	}
	out := make([]*store.Session, len(sessions))
	copy(out, sessions)
	sort.SliceStable(out, func(a, b int) bool {
		ra, rb := groups[sourceDir(out[a])].rank, groups[sourceDir(out[b])].rank
		if ra != rb {
			return ra < rb
		}
		return out[a].CreatedAt.After(out[b].CreatedAt) // newest agent first within its group
	})
	return out
}

// flatSessions returns the sessions that belong in the flat agent list: those
// not owned by a pipeline, plus orphans whose owning pipeline no longer exists.
// The latter case keeps a deleted pipeline's leftover agents visible (and
// terminable) instead of hiding them — they have no pipeline header to render
// under, so without this they'd vanish from the list while still being counted.
func flatSessions(sessions []*store.Session, pipelines []*pipeline.Pipeline) []*store.Session {
	known := make(map[string]bool, len(pipelines))
	for _, p := range pipelines {
		known[p.ID] = true
	}
	out := make([]*store.Session, 0, len(sessions))
	for _, s := range sessions {
		if s.PipelineID == "" || !known[s.PipelineID] {
			out = append(out, s)
		}
	}
	return out
}

// item is one navigable row: a real agent (session != nil) or a placeholder for
// an opened directory that currently has no agents (session == nil). dir is the
// group directory and is always set.
type item struct {
	session   *store.Session
	dir       string
	approvals bool // synthetic top-of-list inbox row
	apprCount int  // number of waiting agents (inbox row only)

	pipeline  *pipeline.Pipeline // pipeline header row
	collapsed bool               // pipeline/agent header row: children hidden (▸ vs ▾)
	pjPipe    string             // pipelineJob row: owning pipeline id
	pjJob     *pipeline.Job      // pipelineJob row: the job
	pjSess    *store.Session     // pipelineJob row: linked live session (nil if none/terminal)

	// agent sub-tree rows (agent sub-tree grouping)
	depth       int  // nesting level under the root agent (0 = root)
	hasKids     bool // has ≥1 child agent → collapsible header (▸/▾)
	tombstone   bool // terminal parent: render header-only, no live badge/gauge
	runningKids int  // live descendants under a tombstone (the "N running" badge)
}

// dirKey is the placeholder identity for an opened dir. The NUL separator can't
// occur in a session ID, so a placeholder key never collides with an agent's.
func dirKey(dir string) string { return "dir\x00" + dir }

// itemKey is the stable identity used to re-pin the cursor across refreshes.
func itemKey(it item) string {
	if it.approvals {
		return "approvals\x00"
	}
	if it.pipeline != nil {
		return "pipe\x00" + it.pipeline.ID
	}
	if it.pjJob != nil {
		return "pjob\x00" + it.pjPipe + "\x00" + it.pjJob.ID
	}
	if it.session != nil {
		return it.session.ID
	}
	return dirKey(it.dir)
}

// liveStatus reports whether an agent status is non-terminal — still running or
// awaiting input. Mirrors the daemon's liveStatus (internal/daemon): a terminal
// parent with children is a tombstone, rendered header-only in the sub-tree.
func liveStatus(s store.Status) bool {
	switch s {
	case store.StatusSpawning, store.StatusWorking, store.StatusWaitingForInput, store.StatusIdle:
		return true
	}
	return false // done/errored/orphaned/rate_limited
}

// buildItems flattens grouped sessions plus opened directories into the list the
// cursor walks, nesting agent-spawned children under their parent (agent sub-tree
// grouping). Dir grouping is over ROOT agents only — a child nests under its
// root's dir regardless of its own sourceDir. Groups are ordered by creation (an
// agent group's key is its newest root's CreatedAt; an empty opened dir's key is
// when it was opened — so a freshly-opened dir floats to the top). An opened dir
// that has agents emits its sub-trees and no placeholder; an opened dir with none
// emits a single placeholder. A node listed in `collapsed` hides its whole
// sub-tree. Pure: returns a new slice, leaves inputs untouched. Callers pass
// sessions already grouped by groupSort; within-group root order is preserved.
func buildItems(sessions []*store.Session, opened map[string]time.Time, collapsed map[string]bool) []item {
	// Index for parent lookup + child grouping. A child whose parent id is absent
	// from the set is an orphan → promoted to a root so it never vanishes.
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	childrenByParent := map[string][]*store.Session{}
	var roots []*store.Session
	for _, s := range sessions {
		if s.ParentID != "" && byID[s.ParentID] != nil {
			childrenByParent[s.ParentID] = append(childrenByParent[s.ParentID], s)
		} else {
			roots = append(roots, s)
		}
	}

	type grp struct {
		max  time.Time
		seen int
	}
	groups := map[string]*grp{}
	var order []string
	note := func(dir string, t time.Time) {
		g := groups[dir]
		if g == nil {
			groups[dir] = &grp{max: t, seen: len(order)}
			order = append(order, dir)
			return
		}
		if t.After(g.max) {
			g.max = t
		}
	}
	for _, s := range roots {
		note(sourceDir(s), s.CreatedAt)
	}
	for dir, at := range opened {
		note(dir, at)
	}
	sort.SliceStable(order, func(a, b int) bool {
		ga, gb := groups[order[a]], groups[order[b]]
		if ga.max.Equal(gb.max) {
			return ga.seen < gb.seen
		}
		return ga.max.After(gb.max)
	})
	byDir := map[string][]*store.Session{}
	for _, s := range roots {
		byDir[sourceDir(s)] = append(byDir[sourceDir(s)], s)
	}
	var items []item
	seen := map[string]bool{} // cycle guard across the whole forest
	for _, dir := range order {
		rs := byDir[dir]
		if len(rs) == 0 {
			items = append(items, item{dir: dir}) // empty opened dir → placeholder
			continue
		}
		for _, s := range rs {
			items = appendSubtree(items, s, dir, 0, childrenByParent, collapsed, seen)
		}
	}
	return items
}

// appendSubtree emits s then, unless it is collapsed, its descendants in DFS
// pre-order, assigning tree depth. A node with children renders as a collapsible
// header (▸/▾); a terminal (non-live) parent is a tombstone — header-only, no
// live badge/gauge — carrying the count of live descendants still under it. seen
// guards against a malformed parent cycle.
func appendSubtree(items []item, s *store.Session, dir string, depth int, childrenByParent map[string][]*store.Session, collapsed, seen map[string]bool) []item {
	if seen[s.ID] {
		return items
	}
	seen[s.ID] = true

	kids := childrenByParent[s.ID]
	it := item{session: s, dir: dir, depth: depth, hasKids: len(kids) > 0}
	if it.hasKids {
		it.collapsed = collapsed[s.ID]
		if !liveStatus(s.Status) {
			it.tombstone = true
			it.runningKids = liveDescendants(s.ID, childrenByParent, map[string]bool{})
		}
	}
	items = append(items, it)
	if it.hasKids && !it.collapsed {
		for _, c := range kids {
			items = appendSubtree(items, c, dir, depth+1, childrenByParent, collapsed, seen)
		}
	}
	return items
}

// liveDescendants counts the live (non-terminal) agents anywhere under id — the
// figure the tombstone header reports as "N running". seen guards against cycles.
func liveDescendants(id string, childrenByParent map[string][]*store.Session, seen map[string]bool) int {
	if seen[id] {
		return 0
	}
	seen[id] = true
	n := 0
	for _, c := range childrenByParent[id] {
		if liveStatus(c.Status) {
			n++
		}
		n += liveDescendants(c.ID, childrenByParent, seen)
	}
	return n
}

// pipelineItems flattens pipelines into a header row per pipeline followed by an
// indented row per job. Each job row holds a distinct *Job pointer plus, when the
// job has spawned an agent, the matching live session (for the state badge, token
// gauge, and worktree). A pipeline whose id is marked in `collapsed` emits only its
// header row (jobs hidden).
func pipelineItems(ps []*pipeline.Pipeline, sessions []*store.Session, collapsed map[string]bool) []item {
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	var out []item
	for _, p := range ps {
		c := collapsed[p.ID]
		out = append(out, item{pipeline: p, collapsed: c})
		if c {
			continue
		}
		for i := range p.Jobs {
			j := p.Jobs[i] // fresh var each iteration → distinct pointer
			out = append(out, item{pjPipe: p.ID, pjJob: &j, pjSess: byID[j.SessionID]})
		}
	}
	return out
}

// listRow is one rendered line: a group header (header != "") or a body row that
// points at items[idx]. buildRows assumes items are in grouped order (buildItems),
// so a group's rows are contiguous.
type listRow struct {
	header string
	idx    int
}

func buildRows(items []item) []listRow {
	var rows []listRow
	prev := ""
	started := false
	for i := range items {
		// Pinned/synthetic rows (approvals, pipeline header, pipeline job) have no
		// dir group — emit them bare, never under a dir header.
		if items[i].approvals || items[i].pipeline != nil || items[i].pjJob != nil {
			rows = append(rows, listRow{idx: i})
			continue
		}
		dir := items[i].dir
		if !started || dir != prev {
			count := 0
			for j := i; j < len(items) && !items[j].approvals && items[j].pipeline == nil && items[j].pjJob == nil && items[j].dir == dir; j++ {
				if items[j].session != nil {
					count++
				}
			}
			rows = append(rows, listRow{header: fmt.Sprintf("%s (%d)", abbrevHome(dir), count)})
			prev = dir
			started = true
		}
		rows = append(rows, listRow{idx: i})
	}
	return rows
}

func cursorRowIndex(rows []listRow, cursor int) int {
	for i, r := range rows {
		if r.header == "" && r.idx == cursor {
			return i
		}
	}
	return 0
}

func headerAbove(rows []listRow, at int) string {
	for i := at; i >= 0; i-- {
		if rows[i].header != "" {
			return rows[i].header
		}
	}
	return ""
}

func countRows(rows []listRow) int {
	n := 0
	for _, r := range rows {
		if r.header == "" {
			n++
		}
	}
	return n
}

// renderList renders the item list windowed to exactly height lines and width
// columns. Items are grouped by dir with dimmed header rows; an empty opened dir
// shows a selectable "(no agents …)" placeholder. The cursor item is always kept
// visible; a sticky header shows when the window starts mid-group.
func renderList(items []item, cursor, width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(items) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	rows := buildRows(items)
	n := len(rows)
	cursorRow := cursorRowIndex(rows, cursor)

	visible := height
	hidden := n > height
	if hidden {
		if visible = height - 1; visible < 1 {
			visible = 1
		}
	}

	top := listWindow(n, cursorRow, visible)
	sticky := rows[top].header == "" && visible > 1 && headerAbove(rows, top) != ""
	bodyCap := visible
	if sticky {
		top = listWindow(n, cursorRow, visible-1)
		sticky = rows[top].header == ""
		if sticky {
			bodyCap = visible - 1
		}
	}

	var b strings.Builder
	used := 0
	if sticky {
		b.WriteString(stMuted.Render(headerAbove(rows, top)) + "\n")
		used++
	}
	end := top
	for i := top; i < top+bodyCap && i < n; i++ {
		r := rows[i]
		if r.header != "" {
			b.WriteString(stMuted.Render(r.header) + "\n")
		} else {
			b.WriteString(renderItemLine(items[r.idx], r.idx == cursor, width) + "\n")
		}
		used++
		end = i + 1
	}

	if hidden && height > 1 {
		var parts []string
		if a := countRows(rows[:top]); a > 0 {
			parts = append(parts, fmt.Sprintf("▲ %d more", a))
		}
		if a := countRows(rows[end:]); a > 0 {
			parts = append(parts, fmt.Sprintf("▼ %d more", a))
		}
		b.WriteString(stMuted.Render(strings.Join(parts, "   ")) + "\n")
		used++
	}
	for ; used < height; used++ {
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// jobBadge maps a pipeline job status to its status glyph + color style. The
// glyph and (by caller convention) the status word are rendered in this color.
func jobBadge(s pipeline.JobStatus) (string, lipgloss.Style) {
	switch s {
	case pipeline.JobDone:
		return "●", stBusy // green
	case pipeline.JobRunning:
		return "◐", stRunning // cyan
	case pipeline.JobFailed:
		return "✗", stError // red
	case pipeline.JobNeedsAttention:
		return "⚠", stAttention // amber
	case pipeline.JobSkipped:
		return "⊘", stMuted // grey
	default: // pending
		return "○", stMuted // grey
	}
}

// pipelineDisplayStatus maps a pipeline to its display label, color style, and
// glyph. It derives a "partial" state (amber ◑) when the pipeline has reached a
// terminal status (done/stalled/canceled) but ≥1 job failed or needs attention —
// so a pipeline that "finished" with casualties is not shown plain green/grey.
// Otherwise it maps the real status. By caller convention the glyph and label
// are both rendered in the returned style.
func pipelineDisplayStatus(p *pipeline.Pipeline) (string, lipgloss.Style, string) {
	if pipelineIsTerminal(p.Status) && pipelineHasFailure(p) {
		return "partial", stAttention, "◑" // amber
	}
	switch p.Status {
	case pipeline.StatusRunning:
		return "running", stRunning, "◐" // cyan
	case pipeline.StatusPaused:
		return "paused", stAttention, "⏸" // amber
	case pipeline.StatusDone:
		return "done", stBusy, "●" // green
	case pipeline.StatusStalled:
		return "stalled", stAttention, "⚠" // amber
	case pipeline.StatusCanceled:
		return "canceled", stMuted, "⊘" // grey
	default: // pending
		return "pending", stMuted, "○" // grey
	}
}

// pipelineIsTerminal reports whether a pipeline status is a stopped/final state.
func pipelineIsTerminal(s pipeline.Status) bool {
	return s == pipeline.StatusDone || s == pipeline.StatusStalled || s == pipeline.StatusCanceled
}

// pipelineIsCompleted reports whether a pipeline finished cleanly enough to be
// collapsed by default in the list (done or canceled). Stalled is excluded: it
// signals casualties the user should see, so it stays expanded.
func pipelineIsCompleted(s pipeline.Status) bool {
	return s == pipeline.StatusDone || s == pipeline.StatusCanceled
}

// pipelineHasFailure reports whether any job failed or needs attention.
func pipelineHasFailure(p *pipeline.Pipeline) bool {
	for i := range p.Jobs {
		if p.Jobs[i].Status == pipeline.JobFailed || p.Jobs[i].Status == pipeline.JobNeedsAttention {
			return true
		}
	}
	return false
}

// contextLabel renders an agent's context gauge as a short figure ("210k") plus
// the lipgloss style for its state band. An unknown gauge (no model turn yet)
// renders "" so a just-spawned agent shows nothing rather than a green 0k.
func contextLabel(tokens int, state string) (string, lipgloss.Style) {
	if tokens == 0 && state == "" {
		return "", stMuted
	}
	label := fmt.Sprintf("%dk", tokens/1000)
	switch state {
	case store.ContextWarning:
		return label, stCtxWarn
	case store.ContextCritical:
		return label, stCtxCrit
	default:
		return label, stCtxOK
	}
}

// treePrefix renders the sub-tree indentation + collapse glyph for an agent row:
// two spaces per depth level, then ▾/▸ for a node with children (expanded vs
// collapsed), or two aligning spaces for a leaf so it lines up under siblings
// that carry a glyph. A childless root (depth 0) gets no prefix — the flat list
// renders exactly as before.
func treePrefix(it item) string {
	if it.depth == 0 && !it.hasKids {
		return ""
	}
	p := strings.Repeat("  ", it.depth)
	switch {
	case it.hasKids && it.collapsed:
		return p + "▸ "
	case it.hasKids:
		return p + "▾ "
	default:
		return p + "  "
	}
}

// renderItemLine renders one body row: an agent's columns, or the placeholder
// line for an empty opened dir. The cursor row gets the "› " caret + cursor style.
func renderItemLine(it item, selected bool, width int) string {
	var line string
	switch {
	case it.approvals:
		txt := "⏳ Approvals (" + strconv.Itoa(it.apprCount) + ")"
		if it.apprCount == 0 {
			line = stMuted.Render(txt)
		} else {
			line = stStatus.Render(txt)
		}
	case it.pipeline != nil:
		exp := "▾" // expanded
		if it.collapsed {
			exp = "▸" // collapsed
		}
		label, st, glyph := pipelineDisplayStatus(it.pipeline)
		line = exp + " " + stPaneTitle.Render(it.pipeline.ID) + "  " + st.Render(glyph+" "+label)
	case it.pjJob != nil:
		deps := ""
		if len(it.pjJob.DependsOn) > 0 {
			deps = stMuted.Render("  (deps: " + strings.Join(it.pjJob.DependsOn, ",") + ")")
		}
		glyph, st := jobBadge(it.pjJob.Status)
		statusWord := fmt.Sprintf("%-13s", string(it.pjJob.Status))
		// When the job has a live session, surface the agent's execution badge and
		// context-token gauge — a "running" job whose agent "needs-input" matters.
		agentCol, ctxCol := fmt.Sprintf("%-11s", ""), fmt.Sprintf("%-6s", "")
		if s := it.pjSess; s != nil {
			label, ast := badge(s.Status, s.ExitCode)
			agentCol = ast.Render(fmt.Sprintf("%-11s", label))
			if cl, cst := contextLabel(s.ContextTokens, s.ContextState); cl != "" {
				ctxCol = cst.Render(fmt.Sprintf("%-6s", cl))
			}
		}
		// Branch/worktree: prefer the job's branch, else the session's worktree name.
		branchInfo := it.pjJob.Branch
		if branchInfo == "" && it.pjSess != nil && it.pjSess.Worktree != "" {
			branchInfo = filepath.Base(it.pjSess.Worktree)
		}
		if branchInfo != "" {
			branchInfo = stMuted.Render(" [" + trunc(branchInfo, 20) + "]")
		}
		line = fmt.Sprintf("    %s %-12s %s %s %s%s",
			st.Render(glyph), trunc(it.pjJob.ID, 12), st.Render(statusWord), agentCol, ctxCol, branchInfo) + deps
	case it.session == nil:
		line = stMuted.Render("(no agents — n to spawn here)")
	case it.tombstone:
		// A deleted-or-done parent anchoring live children: header-only, muted,
		// with the running-descendant count. No state badge, gauge, or worktree —
		// there is no live pane to attach to.
		s := it.session
		nameStr := s.Name
		if nameStr == "" {
			nameStr = "—"
		} else {
			nameStr = trunc(nameStr, 15)
		}
		line = treePrefix(it) + stMuted.Render(fmt.Sprintf("%-16s %-14s (terminated · %d running)", nameStr, s.ID, it.runningKids))
	default:
		s := it.session
		label, st := badge(s.Status, s.ExitCode)
		cl, cst := contextLabel(s.ContextTokens, s.ContextState)
		// Add branch/worktree info: prefer worktree name (if exists), otherwise branch name
		branchInfo := ""
		if s.Worktree != "" {
			// Extract just the worktree directory name (last component of path)
			branchInfo = filepath.Base(s.Worktree)
		} else if s.Branch != "" {
			branchInfo = s.Branch
		}
		if branchInfo != "" {
			branchInfo = stMuted.Render(" [" + trunc(branchInfo, 20) + "]")
		}
		// Display name as first column if present. When the name is blank, use a
		// plain "—" on the selected row: styling it here would embed an ANSI reset
		// at the very start of the line, which cuts the cursor highlight applied to
		// the whole row below — that reset is what left unnamed agents un-highlighted.
		nameStr := s.Name
		switch {
		case nameStr != "":
			nameStr = trunc(nameStr, 15)
		case selected:
			nameStr = "—"
		default:
			nameStr = stMuted.Render("—")
		}
		line = treePrefix(it) + fmt.Sprintf("%-16s %-14s %-11s %-6s %-5s %s%s",
			nameStr, s.ID, st.Render(label),
			cst.Render(fmt.Sprintf("%-6s", cl)), age(s.UpdatedAt),
			stMuted.Render(fmt.Sprintf("%-7s", trunc(backendOr(s), 7))), branchInfo)
	}
	cur := "  "
	if selected {
		cur = stCursor.Render("› ")
		if it.session != nil || it.approvals || it.pipeline != nil || it.pjJob != nil {
			line = stCursor.Render(line)
		}
	}
	return cur + line
}

// recognizedApprovals returns the subset of views that are answerable menus
// (Recognized) — the ones the cockpit can present option keys for. Unrecognized
// prompts must be attached to, not answered here.
func recognizedApprovals(views []approval.View) []approval.View {
	out := make([]approval.View, 0, len(views))
	for _, v := range views {
		if v.Recognized {
			out = append(out, v)
		}
	}
	return out
}

// detailBody renders the modeDetails overlay: every populated store.Session
// field for the selected agent, grouped (header, summary, location, refs, mode,
// plumbing). Empty fields/sections are omitted so the view stays tight. Pure
// over the session — no fetch. Reuses badge/contextLabel/age/abbrevHome/trunc.
func detailBody(s *store.Session, width int) string {
	if s == nil {
		return stMuted.Render("(no agent selected)")
	}
	var b strings.Builder
	label, st := badge(s.Status, s.ExitCode)
	permMode := s.PermissionMode
	if permMode == "" {
		permMode = "default"
	}
	// Show name in title if present
	title := s.ID
	if s.Name != "" {
		title = s.ID + " (" + s.Name + ")"
	}
	b.WriteString(stPaneTitle.Render(title) + "  " + st.Render(label) + " · " + permMode + "\n\n")

	// summary block
	nameStr := s.Name
	if nameStr == "" {
		nameStr = "—"
	}
	b.WriteString(stMuted.Render("name      ") + nameStr + "\n")
	if s.Subject != "" {
		b.WriteString(stMuted.Render("subject   ") + s.Subject + "\n")
	}
	b.WriteString(stMuted.Render("type      ") + typeOr(s) + "   " + stMuted.Render("age ") + age(s.UpdatedAt) + "\n")
	b.WriteString(stMuted.Render("backend   ") + backendOr(s) + "\n")
	if cl, _ := contextLabel(s.ContextTokens, s.ContextState); cl != "" {
		ctxLine := stMuted.Render("context   ") + cl
		if s.ContextState != "" {
			ctxLine += " (" + s.ContextState + ")"
		}
		b.WriteString(ctxLine + "\n")
	}

	// location
	var loc []string
	dir := s.Repo
	if dir == "" {
		dir = s.Workdir
	}
	if dir != "" {
		loc = append(loc, stMuted.Render("  dir       ")+abbrevHome(dir))
	}
	// Show worktree first (if exists), then branch - makes it easier to see the working context
	if s.Worktree != "" {
		wtName := filepath.Base(s.Worktree)
		loc = append(loc, stMuted.Render("  worktree  ")+wtName+" "+stMuted.Render("("+abbrevHome(s.Worktree)+")"))
	}
	if s.Branch != "" {
		loc = append(loc, stMuted.Render("  branch    ")+s.Branch)
	}
	if len(loc) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("location") + "\n" + strings.Join(loc, "\n") + "\n")
	}

	// refs
	var refs []string
	if s.Ticket != "" {
		refs = append(refs, stMuted.Render("  ticket    ")+s.Ticket)
	}
	if s.PR != "" {
		refs = append(refs, stMuted.Render("  pr        ")+s.PR)
	}
	if s.PipelineID != "" {
		line := stMuted.Render("  pipeline  ") + s.PipelineID
		if s.JobID != "" {
			line += "  " + stMuted.Render("job ") + s.JobID
		}
		refs = append(refs, line)
	}
	if len(refs) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("refs") + "\n" + strings.Join(refs, "\n") + "\n")
	}

	// mode
	if s.AutoRestart {
		b.WriteString("\n" + stPaneTitle.Render("mode") + "\n")
		b.WriteString(fmt.Sprintf("  %s · auto-restart ×%d\n", permMode, s.RestartCount))
	}

	// plumbing
	b.WriteString("\n" + stPaneTitle.Render("plumbing") + "\n")
	b.WriteString(fmt.Sprintf("  pid %d · tmux %s\n", s.PID, s.TmuxSession))
	if s.ClaudeSessionID != "" {
		b.WriteString(stMuted.Render("  claude    ") + trunc(s.ClaudeSessionID, 12) + "\n")
	}
	if s.Prompt != "" {
		b.WriteString(stMuted.Render("  prompt    ") + "\"" + trunc(s.Prompt, max(0, width-14)) + "\"\n")
	}
	return b.String()
}

// digestBody renders a completion digest for the modeDigest overlay: task,
// metadata line, changed files with +/- counts, and the narrative summary.
func digestBody(d *digest.Digest, width int) string {
	if d == nil {
		return stMuted.Render("(no digest)")
	}
	var b strings.Builder
	if d.Task != "" {
		b.WriteString(stMuted.Render("Task: ") + d.Task + "\n")
	}
	meta := []string{}
	if d.Status != "" {
		meta = append(meta, "status "+d.Status)
	}
	if d.Branch != "" {
		meta = append(meta, "branch "+d.Branch)
	}
	meta = append(meta, fmt.Sprintf("%d turns", d.Turns))
	b.WriteString(stMuted.Render(strings.Join(meta, " · ")) + "\n\n")
	if len(d.Files) > 0 {
		b.WriteString(stPaneTitle.Render("Files") + "\n")
		for _, f := range d.Files {
			b.WriteString(fmt.Sprintf("  %s  (+%d/-%d)\n", f.Path, f.Added, f.Removed))
		}
		b.WriteString("\n")
	}
	b.WriteString(stPaneTitle.Render("Summary") + "\n")
	if strings.TrimSpace(d.Summary) == "" {
		b.WriteString(stMuted.Render("(no summary)") + "\n")
	} else {
		b.WriteString(d.Summary + "\n")
	}
	return b.String()
}

// approvalsBody renders the modeApprovals overlay: the focused prompt's question
// and numbered options, with a "N of M waiting" indicator when several are queued.
func approvalsBody(rec []approval.View, cursor, width int) string {
	if len(rec) == 0 {
		return stMuted.Render("(no pending approvals)")
	}
	if cursor < 0 || cursor >= len(rec) {
		cursor = 0
	}
	v := rec[cursor]
	var b strings.Builder
	if len(rec) > 1 {
		b.WriteString(stMuted.Render(fmt.Sprintf("%d of %d waiting (tab for next)", cursor+1, len(rec))) + "\n")
	}
	b.WriteString(stPaneTitle.Render(v.ID) + "\n")
	if v.Question != "" {
		b.WriteString(v.Question + "\n")
	}
	for i, opt := range v.Options {
		b.WriteString(fmt.Sprintf("  %d. %s\n", i+1, opt))
	}
	return b.String()
}

// padTo pads s with blank lines to exactly height lines.
func padTo(s string, height int) string {
	for cur := strings.Count(s, "\n") + 1; cur < height; cur++ {
		s += "\n"
	}
	return s
}

func typeOr(s *store.Session) string {
	if s.Type == "" {
		return "classifying"
	}
	return string(s.Type)
}

// backendOr returns the agent's AI backend id (claude, aider, …), defaulting to
// "claude" when empty. Backend is json `omitempty`, so agents spawned before
// backends were recorded carry no value — treat the registry default as claude
// everywhere rather than rendering a blank.
func backendOr(s *store.Session) string {
	if s.Backend == "" {
		return "claude"
	}
	return s.Backend
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// itemAt returns the item at cursor, clamped to the slice bounds. Returns a zero
// item (nil session, "" dir) when items is empty.
func itemAt(items []item, cursor int) item {
	if len(items) == 0 {
		return item{}
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(items) {
		cursor = len(items) - 1
	}
	return items[cursor]
}

// activeDir is the directory a new agent should launch in: the cursor item's
// group dir, or fallback when there is no item or the group dir is unknown ("—").
func activeDir(items []item, cursor int, fallback string) string {
	d := itemAt(items, cursor).dir
	if d == "" || d == "—" {
		return fallback
	}
	return d
}

// listWindow returns the index of the first row to render so a window of
// `visible` rows always contains the cursor. Stateless — derived from cursor +
// height each render, so no scroll state is kept on the Model.
func listWindow(n, cursor, visible int) int {
	if visible < 1 || n <= visible {
		return 0
	}
	top := 0
	if cursor >= visible {
		top = cursor - visible + 1
	}
	if maxTop := n - visible; top > maxTop {
		top = maxTop
	}
	if top < 0 {
		top = 0
	}
	return top
}

// expandPath expands a leading ~, resolves relative paths against the cwd via
// filepath.Abs, and normalizes the result. home is injected (empty = no expansion).
func expandPath(p, home string) string {
	if home != "" {
		if p == "~" {
			p = home
		} else if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return filepath.Clean(p)
}

// dirCompletionTarget splits a typed path into the directory to list and the
// leaf prefix to match. A trailing slash means "list this dir's children" (empty
// leaf); otherwise the last segment is the prefix to complete within its parent.
func dirCompletionTarget(typed string) (listDir, leaf string) {
	if typed == "" {
		return ".", ""
	}
	if strings.HasSuffix(typed, "/") {
		d := strings.TrimRight(typed, "/")
		if d == "" {
			d = "/"
		}
		return d, ""
	}
	return filepath.Dir(typed), filepath.Base(typed)
}

// longestCommonPrefix returns the longest string that prefixes every input.
func longestCommonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	pre := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, pre) {
			pre = pre[:len(pre)-1]
			if pre == "" {
				return ""
			}
		}
	}
	return pre
}

// completeDir, given a listing of the directory that contains `typed`'s leaf,
// returns `typed` completed to the longest common prefix of the entries that
// match the leaf, plus the matching entry names (for display). When nothing
// matches, returns typed unchanged and nil candidates.
func completeDir(listing client.DirListing, typed string) (completed string, candidates []string) {
	listDir, leaf := dirCompletionTarget(typed)
	for _, e := range listing.Entries {
		if strings.HasPrefix(e.Name, leaf) {
			candidates = append(candidates, e.Name)
		}
	}
	if len(candidates) == 0 {
		return typed, nil
	}
	lcp := longestCommonPrefix(candidates)
	if lcp == leaf {
		return typed, candidates // already at the common prefix; just show candidates
	}
	return filepath.Join(listDir, lcp), candidates
}
