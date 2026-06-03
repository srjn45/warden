package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srajanpathak/agentctl/internal/store"
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

// sourceDir is the grouping key: the directory the agentctl command was
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

// item is one navigable row: a real agent (session != nil) or a placeholder for
// an opened directory that currently has no agents (session == nil). dir is the
// group directory and is always set.
type item struct {
	session *store.Session
	dir     string
}

// dirKey is the placeholder identity for an opened dir. The NUL separator can't
// occur in a session ID, so a placeholder key never collides with an agent's.
func dirKey(dir string) string { return "dir\x00" + dir }

// itemKey is the stable identity used to re-pin the cursor across refreshes.
func itemKey(it item) string {
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
	hasAgents := map[string]bool{}
	byDir := map[string][]*store.Session{}
	for _, s := range sessions {
		d := sourceDir(s)
		hasAgents[d] = true
		byDir[d] = append(byDir[d], s)
	}
	var items []item
	for _, dir := range order {
		if hasAgents[dir] {
			for _, s := range byDir[dir] {
				items = append(items, item{session: s, dir: dir})
			}
			continue
		}
		items = append(items, item{dir: dir}) // empty opened dir → placeholder
	}
	return items
}

// listRow is one rendered line: a group header (header != "") or an agent
// (header == "", idx points into the sessions slice). buildRows assumes the
// sessions are already in grouped order (groupSort), so groups are contiguous.
type listRow struct {
	header string
	idx    int
}

func buildRows(sessions []*store.Session) []listRow {
	var rows []listRow
	prev := ""
	for i := range sessions {
		dir := sourceDir(sessions[i])
		if i == 0 || dir != prev {
			count := 0
			for j := i; j < len(sessions) && sourceDir(sessions[j]) == dir; j++ {
				count++
			}
			rows = append(rows, listRow{header: fmt.Sprintf("%s (%d)", abbrevHome(dir), count)})
			prev = dir
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
	return 0 // unreachable in practice (cursor is clamped, sessions non-empty); 0 is a safe default
}

func headerAbove(rows []listRow, at int) string {
	for i := at; i >= 0; i-- {
		if rows[i].header != "" {
			return rows[i].header
		}
	}
	return ""
}

func countAgents(rows []listRow) int {
	n := 0
	for _, r := range rows {
		if r.header == "" {
			n++
		}
	}
	return n
}

// renderList renders the agent list windowed to exactly `height` lines and
// `width` columns of inner content. Agents are grouped by source directory with
// dimmed, flush-left header rows; the selected agent is always kept visible, and
// a sticky header is shown when the window starts in the middle of a group.
func renderList(sessions []*store.Session, cursor, width, height int) string {
	if height < 1 {
		height = 1
	}
	if len(sessions) == 0 {
		return padTo(stMuted.Render("No agents — press n to create one"), height)
	}
	rows := buildRows(sessions)
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
			s := sessions[r.idx]
			label, st := badge(s.Status)
			line := fmt.Sprintf("%-12s %-9s %-11s %-5s %s",
				trunc(s.ID, 12), trunc(typeOr(s), 9), st.Render(label), age(s.UpdatedAt),
				trunc(s.Subject, max(0, width-44)))
			cur := "  "
			if r.idx == cursor {
				cur = stCursor.Render("› ")
				line = stCursor.Render(line)
			}
			b.WriteString(cur + line + "\n")
		}
		used++
		end = i + 1
	}

	if hidden && height > 1 {
		var parts []string
		if a := countAgents(rows[:top]); a > 0 {
			parts = append(parts, fmt.Sprintf("▲ %d more", a))
		}
		if a := countAgents(rows[end:]); a > 0 {
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
