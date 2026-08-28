package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// frameSeg is one top-level section rendered as its own bordered inner frame: the
// section header item, its global cursor index, and its body rows (the rows the
// cursor walks inside that frame). splitSections carves the flat item list into
// these segments.
type frameSeg struct {
	header    item
	hIdx      int    // global index of the header row
	bodyStart int    // global index of body[0] (== hIdx+1)
	body      []item // rows between this header and the next
}

// splitSections carves a section-tagged item list (as control_pane.items()
// produces: Projects · Terminals headers, each followed by its rows)
// into one frameSeg per section. A collapsed section contributes only its header
// (items() emits no body rows for it), so its segment has an empty body.
func splitSections(items []item) []frameSeg {
	var segs []frameSeg
	for i := range items {
		if items[i].section != "" {
			segs = append(segs, frameSeg{header: items[i], hIdx: i, bodyStart: i + 1})
			continue
		}
		if len(segs) == 0 {
			continue // defensive: rows before any section header have no frame
		}
		s := &segs[len(segs)-1]
		s.body = append(s.body, items[i])
	}
	return segs
}

// renderFrames composes the control pane as stacked bordered/titled inner frames —
// one per top-level section (Projects and Terminals) —
// filling exactly width×height. Each frame is a titleBox whose title carries the
// section's collapse glyph, name, and count; its body is the section's rows
// windowed independently around the global cursor (so the frame holding the cursor
// scrolls to reveal it while the others show from their top). A collapsed frame
// shows just its titled border. The frame heights are split by splitFrameHeights.
func renderFrames(items []item, cursor, width, height int) string {
	if height < 1 {
		height = 1
	}
	segs := splitSections(items)
	if len(segs) == 0 {
		return renderList(items, cursor, width, height) // defensive: no sections
	}
	weights := make([]int, len(segs))
	for i := range segs {
		weights[i] = len(segs[i].body)
	}
	heights := splitFrameHeights(weights, height)
	var b strings.Builder
	for i := range segs {
		s := segs[i]
		outerH := heights[i]
		var body string
		if !s.header.collapsed && len(s.body) > 0 {
			// The frame containing the cursor windows to keep it visible; elsewhere
			// localCursor is out of range, so renderList highlights nothing.
			localCursor := cursor - s.bodyStart
			body = renderList(s.body, localCursor, width-2, outerH-2)
			// Clamp each row to the frame interior so a too-wide row truncates rather
			// than wrapping — a wrapped line would push the box past its allotted height.
			body = lipgloss.NewStyle().MaxWidth(width - 2).Render(body)
		}
		b.WriteString(titleBox(frameTitle(s.header, cursor == s.hIdx), body, width, outerH))
		if i < len(segs)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// frameTitle is the border-inset title for one section's inner frame: its collapse
// glyph (▾ open / ▸ collapsed), name, and count badge. A "› " cursor caret is
// prepended when the global cursor sits on the section header, so folding a frame
// stays discoverable even though the header is now the box title rather than a row.
func frameTitle(it item, onHeader bool) string {
	glyph := "▾"
	if it.collapsed {
		glyph = "▸"
	}
	title := fmt.Sprintf("%s %s (%d)", glyph, it.section, it.secCount)
	if onHeader {
		title = "› " + title
	}
	return title
}

// splitFrameHeights divides total outer height across N frames by row-count weight.
// Every frame gets a floor of 3 (top border + one content/blank line + bottom
// border); the surplus is shared out proportionally to each frame's row count, so a
// busy Projects frame gets more room than an empty Terminals one, with the rounding
// remainder handed to the heaviest frame. When total < 3*N the floors still apply
// (the composite then overflows and the outer Control box clips it).
func splitFrameHeights(weights []int, total int) []int {
	n := len(weights)
	if n == 0 {
		return nil
	}
	h := make([]int, n)
	for i := range h {
		h[i] = 3
	}
	extra := total - 3*n
	if extra <= 0 {
		return h
	}
	sum := 0
	for _, w := range weights {
		sum += w
	}
	if sum == 0 {
		h[0] += extra // nothing to weight by → give it all to the first frame
		return h
	}
	allocated, heaviest := 0, 0
	for i, w := range weights {
		add := extra * w / sum
		h[i] += add
		allocated += add
		if w > weights[heaviest] {
			heaviest = i
		}
	}
	h[heaviest] += extra - allocated // rounding remainder → the busiest frame
	return h
}

// titleBox renders body inside a rounded-border box of exactly outerW x outerH,
// with title inset into the top border line (lipgloss v1.1 has no native border
// title). lipgloss pads/truncates body to the inner height.
func titleBox(title, body string, outerW, outerH int) string {
	if outerW < 4 {
		outerW = 4
	}
	if outerH < 3 {
		outerH = 3
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		Width(outerW - 2).
		Height(outerH - 2).
		Render(body)
	return spliceTitle(box, title)
}

// spliceTitle overwrites the top border line of a bordered box with " title ",
// preserving the leading corner and trailing border; truncates a long title.
func spliceTitle(box, title string) string {
	if title == "" {
		return box
	}
	parts := strings.SplitN(box, "\n", 2)
	top := []rune(parts[0])
	if len(top) < 5 {
		return box
	}
	inner := len(top) - 3 // writable columns [2 .. len-2] inclusive
	label := []rune(" " + trunc(title, max(0, inner-1)) + " ")
	if len(label) > inner {
		label = label[:inner]
	}
	for i, r := range label {
		top[2+i] = r
	}
	rest := ""
	if len(parts) == 2 {
		rest = "\n" + parts[1]
	}
	return string(top) + rest
}
