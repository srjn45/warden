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
// contiguous. Groups are ordered by their most-recently-active agent (desc);
// within a group the input order is preserved (the daemon already sorts
// UpdatedAt-desc). Pure: returns a new slice, leaves the input untouched.
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
			groups[k] = &grp{max: s.UpdatedAt, seen: i}
			keys = append(keys, k)
			continue
		}
		if s.UpdatedAt.After(g.max) {
			g.max = s.UpdatedAt
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
		return groups[sourceDir(out[a])].rank < groups[sourceDir(out[b])].rank
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
	collapsed bool               // pipeline header row: jobs hidden (▸ vs ▾)
	pjPipe    string             // pipelineJob row: owning pipeline id
	pjJob     *pipeline.Job      // pipelineJob row: the job
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

// buildItems flattens grouped sessions plus opened directories into the list the
// cursor walks. Groups are ordered by most-recent activity (an agent group's key
// is its newest UpdatedAt; an empty opened dir's key is when it was opened — so a
// freshly-opened dir floats to the top). An opened dir that has agents emits its
// agents and no placeholder; an opened dir with none emits a single placeholder.
// Pure: returns a new slice, leaves inputs untouched.
// Callers pass sessions already grouped by groupSort; within-group agent order is preserved from the input.
func buildItems(sessions []*store.Session, opened map[string]time.Time) []item {
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
	for _, s := range sessions {
		note(sourceDir(s), s.UpdatedAt)
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
	for _, s := range sessions {
		byDir[sourceDir(s)] = append(byDir[sourceDir(s)], s)
	}
	var items []item
	for _, dir := range order {
		if agents := byDir[dir]; len(agents) > 0 {
			for _, s := range agents {
				items = append(items, item{session: s, dir: dir})
			}
			continue
		}
		items = append(items, item{dir: dir}) // empty opened dir → placeholder
	}
	return items
}

// pipelineItems flattens pipelines into a header row per pipeline followed by an
// indented row per job. Each job row holds a distinct *Job pointer. A pipeline
// whose id is marked in `collapsed` emits only its header row (jobs hidden).
func pipelineItems(ps []*pipeline.Pipeline, collapsed map[string]bool) []item {
	var out []item
	for _, p := range ps {
		c := collapsed[p.ID]
		out = append(out, item{pipeline: p, collapsed: c})
		if c {
			continue
		}
		for i := range p.Jobs {
			j := p.Jobs[i] // fresh var each iteration → distinct pointer
			out = append(out, item{pjPipe: p.ID, pjJob: &j})
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
		line = fmt.Sprintf("    %s %-12s %s", st.Render(glyph), trunc(it.pjJob.ID, 12), st.Render(statusWord)) + deps
	case it.session == nil:
		line = stMuted.Render("(no agents — n to spawn here)")
	default:
		s := it.session
		label, st := badge(s.Status, s.ExitCode)
		cl, cst := contextLabel(s.ContextTokens, s.ContextState)
		line = fmt.Sprintf("%-12s %-11s %-6s %-5s",
			trunc(s.ID, 12), st.Render(label),
			cst.Render(fmt.Sprintf("%-6s", cl)), age(s.UpdatedAt))
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
