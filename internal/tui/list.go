package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// dirGroupLabel renders a project-node header as "<project> [<~path>]": the
// project name (the representative directory's basename) followed by its
// home-abbreviated path, so a group reads as a named project rather than a bare
// path. Worktrees of one repo collapse to a single node (buildItems keys by
// project), and this labels it by that node's representative directory.
func dirGroupLabel(dir string) string {
	return fmt.Sprintf("%s [%s]", filepath.Base(dir), abbrevHome(dir))
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
// contiguous. Directory groups are ordered chronologically by their FIRST (oldest)
// agent's CreatedAt (asc), so a directory holds its place in the list as new agents
// are created under it — creating an agent never re-sorts its directory to the top.
// Within a group agents are ordered by CreatedAt (asc, oldest first), so a new
// agent is appended to the bottom of its directory rather than shuffling siblings.
// The ordering keys on the immutable CreatedAt rather than UpdatedAt so an agent's
// row is fixed at creation and does not move as it works (UpdatedAt bumps on every
// action, which made the list churn constantly). Pure: returns a new slice, leaves
// the input untouched.
func groupSort(sessions []*store.Session) []*store.Session {
	if len(sessions) < 2 {
		return sessions
	}
	type grp struct {
		first time.Time
		seen  int
		rank  int
	}
	groups := map[string]*grp{}
	var keys []string
	for i, s := range sessions {
		k := sourceDir(s)
		g := groups[k]
		if g == nil {
			groups[k] = &grp{first: s.CreatedAt, seen: i}
			keys = append(keys, k)
			continue
		}
		if s.CreatedAt.Before(g.first) {
			g.first = s.CreatedAt
		}
	}
	sort.SliceStable(keys, func(a, b int) bool {
		ga, gb := groups[keys[a]], groups[keys[b]]
		if ga.first.Equal(gb.first) {
			return ga.seen < gb.seen
		}
		return ga.first.Before(gb.first)
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
		return out[a].CreatedAt.Before(out[b].CreatedAt) // oldest agent first within its group
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

// The top-level sections of the control-pane navigator, in render order. Each is
// a bordered/titled inner frame inside the control frame (renderFrames), which
// the user can collapse to fold its whole sub-tree away. The Agents frame is now
// a Projects frame: agents are grouped under their project node (project key from
// the B2 normalizer), so worktrees of one repo collapse to a single node.
// (Pipelines still renders as a top-level frame here; C3 folds it into Projects.)
const (
	secPipelines = "Pipelines"
	secProjects  = "Projects"
	secTerminals = "Terminals"
)

// secKey is the collapse-map / cursor-pin identity for a top-level section
// header. The NUL separator keeps it distinct from any pipeline/agent id.
func secKey(name string) string { return "sec\x00" + name }

// item is one navigable row: a real agent (session != nil) or a placeholder for
// an opened directory that currently has no agents (session == nil). dir is the
// group directory and is always set. A terminal-kind session (session.IsTerminal)
// renders under the Terminals section with its §7 display name in termName.
type item struct {
	session *store.Session
	dir     string

	section  string // non-empty ⇒ a top-level section header row (secApprovals…)
	secCount int    // count badge shown on a section header

	// opened marks the row whose session is currently shown in a cockpit pane:
	// the openedAgent in the agent pane (an Agents-section agent or a Pipelines
	// job row) or the openedTerminal in the terminal pane. It gets a distinct
	// marker + tint (stOpened) so you can see what's docked even when the cursor
	// has moved away — and it follows the §8 Alt+a/p/t rotation, which re-points
	// openedAgent/openedTerminal without moving the cursor.
	opened bool

	termName string // §7 display name for a Terminals-section row

	pipeline  *pipeline.Pipeline // pipeline header row
	collapsed bool               // pipeline/agent/section header row: children hidden (▸ vs ▾)
	pjPipe    string             // pipelineJob row: owning pipeline id
	pjJob     *pipeline.Job      // pipelineJob row: the job
	pjSess    *store.Session     // pipelineJob row: linked live session (nil if none/terminal)

	// agent sub-tree rows (agent sub-tree grouping)
	depth       int    // nesting level under the root agent (0 = root)
	hasKids     bool   // has ≥1 child agent → collapsible header (▸/▾)
	tombstone   bool   // terminal parent: render header-only, no live badge/gauge
	runningKids int    // live descendants under a tombstone (the "N running" badge)
	fromParent  string // §4.1 cross-project child surfaced as a root: "↳ from <parent>" backlink
}

// dirKey is the placeholder identity for an opened dir. The NUL separator can't
// occur in a session ID, so a placeholder key never collides with an agent's.
func dirKey(dir string) string { return "dir\x00" + dir }

// itemKey is the stable identity used to re-pin the cursor across refreshes.
func itemKey(it item) string {
	if it.section != "" {
		return secKey(it.section)
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

// noDirGroup reports whether a row must render bare, never under a project-node
// header: the section headers, pipeline rows, and terminal rows all live outside
// the Projects grouping. Only Projects-frame agent rows carry a project group.
func (it item) noDirGroup() bool {
	return it.section != "" || it.pipeline != nil || it.pjJob != nil ||
		(it.session != nil && it.session.IsTerminal())
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
// grouping). Grouping is over ROOT agents only — a child nests under its root's
// project regardless of its own project. Agents group by PROJECT KEY (projKey,
// the B2 normalizer): worktrees of one repo share a key and collapse to a single
// project node, whose header/spawn dir is the first root's dir. Project groups are
// ordered chronologically by their FIRST (oldest) root's CreatedAt (asc), so a
// project keeps its place as new agents are created under it; an empty opened dir
// sorts in by when it was opened (newest → bottom). An opened dir that shares a
// project with agents emits no placeholder; one alone emits a single placeholder.
// A node listed in `collapsed` hides its whole sub-tree. Pure: returns a new
// slice, leaves inputs untouched. Callers pass sessions already grouped by
// groupSort; within-project root order is preserved.
func buildItems(sessions []*store.Session, opened map[string]time.Time, collapsed map[string]bool, projKey func(dir string) string) []item {
	// key returns a session's project key (worktrees collapse to one key).
	key := func(s *store.Session) string { return projKey(sourceDir(s)) }
	// Index for parent lookup + child grouping. A child whose parent id is absent
	// from the set is an orphan → promoted to a root so it never vanishes.
	byID := make(map[string]*store.Session, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
	}
	// §4.1 render rule: a child nests under its parent only when they share a
	// project (same project key). A cross-project child surfaces under ITS OWN
	// project as a normal root, keeping a "↳ from <parent>" lineage backlink, so
	// the project grouping stays truthful and cross-project work isn't force-nested.
	childrenByParent := map[string][]*store.Session{}
	crossParent := map[string]string{} // childID → parent label for surfaced-as-root children
	var roots []*store.Session
	for _, s := range sessions {
		if parent := byID[s.ParentID]; s.ParentID != "" && parent != nil {
			if key(s) == key(parent) {
				childrenByParent[s.ParentID] = append(childrenByParent[s.ParentID], s)
				continue
			}
			crossParent[s.ID] = parentLabel(parent) // cross-project → root with backlink
		}
		roots = append(roots, s)
	}

	type grp struct {
		first time.Time
		seen  int
		dir   string // representative directory (first root's / the opened dir)
	}
	groups := map[string]*grp{}
	var order []string
	note := func(k, dir string, t time.Time) {
		g := groups[k]
		if g == nil {
			groups[k] = &grp{first: t, seen: len(order), dir: dir}
			order = append(order, k)
			return
		}
		if t.Before(g.first) {
			g.first = t
		}
	}
	for _, s := range roots {
		note(key(s), sourceDir(s), s.CreatedAt)
	}
	for dir, at := range opened {
		note(projKey(dir), dir, at)
	}
	sort.SliceStable(order, func(a, b int) bool {
		ga, gb := groups[order[a]], groups[order[b]]
		if ga.first.Equal(gb.first) {
			return ga.seen < gb.seen
		}
		return ga.first.Before(gb.first)
	})
	byKey := map[string][]*store.Session{}
	for _, s := range roots {
		byKey[key(s)] = append(byKey[key(s)], s)
	}
	var items []item
	seen := map[string]bool{} // cycle guard across the whole forest
	for _, k := range order {
		dir := groups[k].dir // every row in the project renders under this one dir header
		rs := byKey[k]
		if len(rs) == 0 {
			items = append(items, item{dir: dir}) // empty opened project → placeholder
			continue
		}
		for _, s := range rs {
			items = appendSubtree(items, s, dir, 0, childrenByParent, crossParent, collapsed, seen)
		}
	}
	return items
}

// parentLabel is the lineage backlink text for a §4.1 cross-project child: the
// parent's name when set, else its id.
func parentLabel(p *store.Session) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// splitByKind partitions sessions into agents and terminals (spec §3). Terminals
// leave the Agents tree entirely — they render only under the Terminals section —
// so the agents slice feeds pipelineItems/buildItems and terminals feeds
// terminalItems.
func splitByKind(sessions []*store.Session) (agents, terminals []*store.Session) {
	for _, s := range sessions {
		if s.IsTerminal() {
			terminals = append(terminals, s)
		} else {
			agents = append(agents, s)
		}
	}
	return agents, terminals
}

// terminalItems renders the Terminals section (spec §7). Terminals are sorted by
// CreatedAt so their 1-based ordinal is stable within a cockpit session, and each
// row carries its formatted display name. Names prefer the live cwd/branch polled
// from the running pane (info, keyed by session id — §7); a terminal with no live
// reading yet (info absent) falls back to the session's stored Workdir/Repo/Branch.
func terminalItems(terminals []*store.Session, info map[string]terminalLiveInfo) []item {
	sorted := make([]*store.Session, len(terminals))
	copy(sorted, terminals)
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].CreatedAt.Before(sorted[b].CreatedAt) })
	out := make([]item, 0, len(sorted))
	for i, t := range sorted {
		cwd, repoRoot, branch := terminalNameParts(t, info)
		out = append(out, item{session: t, termName: terminalDisplayName(i+1, cwd, repoRoot, branch)})
	}
	return out
}

// terminalNameParts picks the cwd/repo-root/branch a terminal's §7 name is built
// from: the live pane reading when present, else the session's stored fields.
func terminalNameParts(t *store.Session, info map[string]terminalLiveInfo) (cwd, repoRoot, branch string) {
	if li, ok := info[t.ID]; ok && li.cwd != "" {
		return li.cwd, li.repoRoot, li.branch
	}
	return terminalCwd(t), t.Repo, t.Branch
}

// terminalCwd is the terminal's working directory for naming: its Workdir, else
// its Repo.
func terminalCwd(t *store.Session) string {
	if t.Workdir != "" {
		return t.Workdir
	}
	return t.Repo
}

// terminalDisplayName formats a terminal's Terminals-section label per spec §7:
// "<index>. <repo>:<rel>/ (<branch>)" — e.g. "2. warden:site/ (main)". An empty
// rel renders as the repo root (just "<repo>"). Outside any git repo it falls
// back to "<index>. <home-abbreviated path>". All git/cwd resolution is done by
// the caller and passed in, keeping this pure and testable.
func terminalDisplayName(index int, cwd, repoRoot, branch string) string {
	if repoRoot == "" {
		return fmt.Sprintf("%d. %s", index, abbrevHome(cwd))
	}
	label := filepath.Base(repoRoot)
	if rel, err := filepath.Rel(repoRoot, cwd); err == nil && rel != "." && rel != "" {
		label += ":" + rel + "/"
	}
	if branch != "" {
		label += " (" + branch + ")"
	}
	return fmt.Sprintf("%d. %s", index, label)
}

// appendSubtree emits s then, unless it is collapsed, its descendants in DFS
// pre-order, assigning tree depth. A node with children renders as a collapsible
// header (▸/▾); a terminal (non-live) parent is a tombstone — header-only, no
// live badge/gauge — carrying the count of live descendants still under it. seen
// guards against a malformed parent cycle.
func appendSubtree(items []item, s *store.Session, dir string, depth int, childrenByParent map[string][]*store.Session, crossParent map[string]string, collapsed, seen map[string]bool) []item {
	if seen[s.ID] {
		return items
	}
	seen[s.ID] = true

	kids := childrenByParent[s.ID]
	it := item{session: s, dir: dir, depth: depth, hasKids: len(kids) > 0, fromParent: crossParent[s.ID]}
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
			items = appendSubtree(items, c, dir, depth+1, childrenByParent, crossParent, collapsed, seen)
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
		// Section headers, approval rows, pipeline rows, and terminal rows have no
		// dir group — emit them bare, never under a dir header.
		if items[i].noDirGroup() {
			rows = append(rows, listRow{idx: i})
			continue
		}
		dir := items[i].dir
		if !started || dir != prev {
			count := 0
			for j := i; j < len(items) && !items[j].noDirGroup() && items[j].dir == dir; j++ {
				if items[j].session != nil {
					count++
				}
			}
			// Indent the dir-group header one level under its section so the tree
			// reads section → project → agents (the agent rows indent one level
			// further, in treePrefix). Header rows carry no cursor gutter, so the
			// four leading spaces put the label at the same column an agent row's
			// gutter+base reaches.
			rows = append(rows, listRow{header: "    " + dirGroupLabel(dir) + fmt.Sprintf(" (%d)", count)})
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

// dirChildIndent is the base indent every dir-grouped body row (agent rows and
// the empty-dir placeholder) carries so it nests one level under its dir-group
// header. It matches the header's own leading indent in buildRows.
const dirChildIndent = "    "

// treePrefix renders the sub-tree indentation + collapse glyph for an agent row.
// Every agent row starts with a base indent (dirChildIndent) so it sits one level
// under its dir-group header — which itself sits one level under the section — so
// the control tree reads section → project → agents. On top of the base it adds
// two spaces per sub-tree depth level, then ▾/▸ for a node with children (expanded
// vs collapsed), or two aligning spaces for a leaf so it lines up under siblings
// that carry a glyph.
func treePrefix(it item) string {
	if it.depth == 0 && !it.hasKids {
		return dirChildIndent
	}
	p := dirChildIndent + strings.Repeat("  ", it.depth)
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
	case it.section != "":
		line = renderSectionHeader(it)
	case it.session != nil && it.session.IsTerminal():
		// A Terminals-section row: the §7 name, with a live glyph. Terminals carry no
		// AI status badge/gauge — they are plain shells.
		glyph, gst := "○", stMuted
		if liveStatus(it.session.Status) {
			glyph, gst = "▪", stRunning
		}
		name := it.termName
		if it.opened && !selected {
			// The opened terminal: badge its name so it stands out from the others
			// (the cursor row, guarded out here, gets the whole-row highlight instead).
			name = stOpenedName.Render(name)
		}
		line = "  " + gst.Render(glyph) + " " + name
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
		jobIDCol := fmt.Sprintf("%-12s", trunc(it.pjJob.ID, 12))
		if it.opened && !selected {
			// The opened pipeline agent's job row: badge its id like a docked agent.
			jobIDCol = stOpenedName.Render(jobIDCol)
		}
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
		line = fmt.Sprintf("    %s %s %s %s %s%s",
			st.Render(glyph), jobIDCol, st.Render(statusWord), agentCol, ctxCol, branchInfo) + deps
	case it.session == nil:
		line = dirChildIndent + stMuted.Render("(no agents — n to spawn here)")
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
		// Display name as first column if present. Build the 16-wide field from the
		// raw (unstyled) text first, then style the whole padded field — so any SGR
		// wraps the column cleanly instead of embedding a reset mid-line. On the
		// selected row the field stays unstyled so the whole-row cursor highlight
		// (applied below) reaches through it; that is what keeps unnamed agents lit.
		rawName := "—"
		if s.Name != "" {
			rawName = trunc(s.Name, 15)
		}
		nameCol := fmt.Sprintf("%-16s", rawName)
		switch {
		case selected:
			// leave unstyled — the cursor highlight owns the whole row
		case it.opened:
			// The opened agent: a bold magenta badge on the name so it is
			// unmistakable at a glance even when the cursor is elsewhere.
			nameCol = stOpenedName.Render(nameCol)
		case s.Name == "":
			nameCol = stMuted.Render(nameCol)
		}
		line = treePrefix(it) + nameCol + " " + fmt.Sprintf("%-14s %-11s %-6s %-5s %s%s",
			s.ID, st.Render(label),
			cst.Render(fmt.Sprintf("%-6s", cl)), age(s.UpdatedAt),
			stMuted.Render(fmt.Sprintf("%-7s", trunc(backendOr(s), 7))), branchInfo)
		// §4.1: a cross-project child surfaced under its own dir keeps a lineage
		// backlink so the orchestration is still visible without cross-dir nesting.
		if it.fromParent != "" {
			line += stMuted.Render("  ↳ from " + trunc(it.fromParent, 16))
		}
	}
	cur := "  "
	switch {
	case selected:
		// The cursor wins the gutter when it sits on the opened row — you are
		// looking right at it, so its own marker would be redundant.
		cur = stCursor.Render("› ")
		if it.session != nil || it.section != "" || it.pipeline != nil || it.pjJob != nil {
			line = stCursor.Render(line)
		}
	case it.opened:
		// The row shown in a cockpit pane right now (openedAgent/openedTerminal),
		// while the cursor is elsewhere: a distinct ◆ gutter marker. The row's name
		// carries the bold magenta badge (stOpenedName, applied above), so it stays
		// findable as you navigate or Alt-rotate the panes without tinting the whole
		// line — a line-level tint would be cut short by the row's own SGR resets.
		cur = stOpened.Render("◆ ")
	}
	return cur + line
}

// renderSectionHeader renders a top-level section header inline (a collapse glyph,
// the section name, and a count badge). The control pane now composes sections as
// bordered frames (renderFrames), where the header becomes the frame title; this
// inline form is retained for the flat renderList path (used by tests and any
// non-framed listing).
func renderSectionHeader(it item) string {
	glyph := "▾"
	if it.collapsed {
		glyph = "▸"
	}
	var suffix string = fmt.Sprintf(" (%d)", it.secCount)
	label := glyph + " " + it.section + suffix
	return stHeader.Render(label)
}

// detailControls is the number of interactive rows in the detail view's controls
// block, in render order: 0 auto-approve, 1 force-compact, 2 events. The detail
// selection cursor (Model.detailSel) walks [0, detailControls).
const detailControls = 3

const (
	detailSelAutoApprove  = iota // toggle the per-agent auto-approve override
	detailSelForceCompact        // cycle the per-agent force-compact override
	detailSelEvents              // open the event list (modeEvents)
)

// boolOnOff renders a bool as an on/off word for the detail controls.
func boolOnOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

// forceCompactState renders an agent's force-compact override as the word
// SetForceCompact accepts: a nil pointer is "inherit" (follow the global
// token_force_compact), true is "on", false is "off".
func forceCompactState(s *store.Session) string {
	if s.ForceCompact == nil {
		return "inherit"
	}
	return boolOnOff(*s.ForceCompact)
}

// nextForceCompact advances the force-compact override one step in the toggle
// cycle inherit → on → off → inherit, so repeated presses walk every state.
func nextForceCompact(cur string) string {
	switch cur {
	case "inherit":
		return "on"
	case "on":
		return "off"
	default:
		return "inherit"
	}
}

// roleOr returns the agent's role, defaulting to "general" (no persona) when
// unset — mirroring backendOr so the field never renders blank.
func roleOr(s *store.Session) string {
	if s.Role == "" {
		return "general"
	}
	return s.Role
}

// fmtTime renders an absolute timestamp for the detail view; a zero time is "—".
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02 15:04")
}

// detailControlLine renders one interactive controls row: "<label>  <value>". The
// focused row (selected) carries a ▸ cursor and is highlighted; the rest align
// under it with a muted label. Passing selected=false everywhere (sel < 0) renders
// the block read-only, as the pipeline-job detail uses it.
func detailControlLine(selected bool, label, val string) string {
	if selected {
		return stCursor.Render(fmt.Sprintf("▸ %-13s %s", label, val))
	}
	return "  " + stMuted.Render(fmt.Sprintf("%-13s", label)) + " " + val
}

// detailBody renders the modeDetails overlay: every populated store.Session field
// for the selected agent, grouped (controls, summary, location, refs, rate-limit,
// lifecycle, plumbing, pane). Empty fields/sections are omitted so the view stays
// tight. The controls block (auto-approve, force-compact, events) is interactive
// when sel >= 0: sel marks the focused row with a ▸ cursor that ↑/↓ move, space
// toggles/cycles, and enter (on events) opens the event list. sel < 0 renders the
// same three read-only, as the pipeline-job detail does. Pure over the session — no
// fetch. Reuses badge/contextLabel/age/abbrevHome/trunc.
func detailBody(s *store.Session, sel, width int) string {
	if s == nil {
		return stMuted.Render("(no agent selected)")
	}
	// field renders a flush "label     value" summary line (label padded to 10).
	field := func(label, val string) string {
		return stMuted.Render(fmt.Sprintf("%-10s", label)) + val + "\n"
	}
	// sub renders an indented "  label    value" line for the grouped sections.
	sub := func(label, val string) string {
		return stMuted.Render(fmt.Sprintf("  %-9s ", label)) + val
	}

	var b strings.Builder
	label, st := badge(s.Status, s.ExitCode)
	permMode := s.PermissionMode
	if permMode == "" {
		permMode = "default"
	}
	title := s.ID
	if s.Name != "" {
		title = s.ID + " (" + s.Name + ")"
	}
	b.WriteString(stPaneTitle.Render(title) + "  " + st.Render(label) + " · " + permMode + "\n\n")

	// controls — the three per-agent overrides/actions (interactive when sel >= 0).
	b.WriteString(stPaneTitle.Render("controls") + "\n")
	evVal := fmt.Sprintf("%d", len(s.Events))
	if sel >= 0 {
		evVal += "  ↵ open"
	}
	b.WriteString(detailControlLine(sel == detailSelAutoApprove, "auto-approve", boolOnOff(s.AutoApprove)) + "\n")
	b.WriteString(detailControlLine(sel == detailSelForceCompact, "force-compact", forceCompactState(s)) + "\n")
	b.WriteString(detailControlLine(sel == detailSelEvents, "events", evVal) + "\n")

	// summary
	b.WriteString("\n" + stPaneTitle.Render("summary") + "\n")
	nameStr := s.Name
	if nameStr == "" {
		nameStr = "—"
	}
	b.WriteString(field("name", nameStr))
	if s.Subject != "" {
		b.WriteString(field("subject", s.Subject))
	}
	b.WriteString(stMuted.Render("type      ") + typeOr(s) + "   " + stMuted.Render("age ") + age(s.UpdatedAt) + "\n")
	b.WriteString(field("backend", backendOr(s)))
	if s.Model != "" {
		b.WriteString(field("model", s.Model))
	}
	b.WriteString(field("role", roleOr(s)))
	if len(s.Tags) > 0 {
		b.WriteString(field("tags", strings.Join(s.Tags, ", ")))
	}
	if cl, _ := contextLabel(s.ContextTokens, s.ContextState); cl != "" {
		v := cl
		if s.ContextState != "" {
			v += " (" + s.ContextState + ")"
		}
		if !s.ContextCheckedAt.IsZero() {
			v += stMuted.Render("  checked " + age(s.ContextCheckedAt))
		}
		b.WriteString(field("context", v))
	}
	b.WriteString(field("created", fmtTime(s.CreatedAt)))

	// location
	var loc []string
	dir := s.Repo
	if dir == "" {
		dir = s.Workdir
	}
	if dir != "" {
		loc = append(loc, sub("dir", abbrevHome(dir)))
	}
	// Show worktree first (if exists), then branch — the working context at a glance.
	if s.Worktree != "" {
		wt := filepath.Base(s.Worktree) + " " + stMuted.Render("("+abbrevHome(s.Worktree)+")")
		if s.WorktreeCreated {
			wt += stMuted.Render(" · warden-created")
		} else {
			wt += stMuted.Render(" · adopted")
		}
		loc = append(loc, sub("worktree", wt))
	}
	if s.Branch != "" {
		br := s.Branch
		if s.BranchCreated {
			br += stMuted.Render(" · warden-created")
		}
		loc = append(loc, sub("branch", br))
	}
	if len(loc) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("location") + "\n" + strings.Join(loc, "\n") + "\n")
	}

	// refs
	var refs []string
	if s.Ticket != "" {
		refs = append(refs, sub("ticket", s.Ticket))
	}
	if s.PR != "" {
		refs = append(refs, sub("pr", s.PR))
	}
	if s.PipelineID != "" {
		line := sub("pipeline", s.PipelineID)
		if s.JobID != "" {
			line += "  " + stMuted.Render("job ") + s.JobID
		}
		refs = append(refs, line)
	}
	if s.ParentID != "" {
		refs = append(refs, sub("parent", s.ParentID))
	}
	if len(refs) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("refs") + "\n" + strings.Join(refs, "\n") + "\n")
	}

	// rate-limit — only when the agent has actually hit a limit.
	if s.RateLimitedAt != nil {
		var rl []string
		rl = append(rl, sub("since", fmtTime(*s.RateLimitedAt)))
		if s.RateLimitRestoreAt != nil {
			rl = append(rl, sub("resume", fmtTime(*s.RateLimitRestoreAt)))
		}
		if s.RateLimitRetryCount > 0 {
			rl = append(rl, sub("retries", fmt.Sprintf("%d", s.RateLimitRetryCount)))
		}
		b.WriteString("\n" + stPaneTitle.Render("rate-limit") + "\n" + strings.Join(rl, "\n") + "\n")
	}

	// lifecycle — restart/compact bookkeeping, shown only when any of it is set.
	var life []string
	if s.AutoRestart {
		life = append(life, sub("restart", fmt.Sprintf("auto ×%d", s.RestartCount)))
	}
	if s.LastRestartAt != nil {
		life = append(life, sub("restarted", fmtTime(*s.LastRestartAt)))
	}
	if s.LastCompactAt != nil {
		life = append(life, sub("compacted", fmtTime(*s.LastCompactAt)))
	}
	if len(life) > 0 {
		b.WriteString("\n" + stPaneTitle.Render("lifecycle") + "\n" + strings.Join(life, "\n") + "\n")
	}

	// plumbing
	b.WriteString("\n" + stPaneTitle.Render("plumbing") + "\n")
	b.WriteString(fmt.Sprintf("  pid %d · tmux %s\n", s.PID, s.TmuxSession))
	if s.ExitCode != nil {
		b.WriteString(sub("exit", fmt.Sprintf("%d", *s.ExitCode)) + "\n")
	}
	if s.ClaudeSessionID != "" {
		b.WriteString(sub("session", trunc(s.ClaudeSessionID, 20)) + "\n")
	}
	if s.Prompt != "" {
		b.WriteString(sub("prompt", "\""+trunc(s.Prompt, max(0, width-14))+"\"") + "\n")
	}

	// pane — the last captured pane excerpt, flattened to one line.
	if ex := oneLine(s.LastPaneExcerpt); ex != "" {
		b.WriteString("\n" + stPaneTitle.Render("pane") + "\n")
		b.WriteString("  " + stMuted.Render(trunc(ex, max(10, width-4))) + "\n")
	}
	return b.String()
}

// eventsBody renders the modeEvents view: the selected agent's event log, newest
// first, each row "<mm-dd hh:mm:ss>  <type>  <detail>". Pure over the session — the
// list refreshes as the poller updates the session. Detail is flattened to one line
// and truncated to the pane width.
func eventsBody(s *store.Session, width int) string {
	if s == nil {
		return stMuted.Render("(no agent selected)")
	}
	var b strings.Builder
	b.WriteString(stPaneTitle.Render(fmt.Sprintf("Events (%d)", len(s.Events))) + "\n\n")
	if len(s.Events) == 0 {
		b.WriteString(stMuted.Render("(no events recorded yet)"))
		return b.String()
	}
	for i := len(s.Events) - 1; i >= 0; i-- { // newest first
		e := s.Events[i]
		ts := "—"
		if !e.TS.IsZero() {
			ts = e.TS.Format("01-02 15:04:05")
		}
		typ := fmt.Sprintf("%-16s", trunc(e.Type, 16))
		detail := oneLine(e.Detail)
		b.WriteString(stMuted.Render(ts) + "  " + typ + " " + trunc(detail, max(10, width-32)) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
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
