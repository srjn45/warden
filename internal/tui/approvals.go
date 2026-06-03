package tui

import (
	"fmt"
	"strings"

	"github.com/srajanpathak/agentctl/internal/approval"
)

// renderApprovalsQueue renders the waiting-agent queue shown in the detail pane
// when the inbox row is selected. cursor is the sub-cursor index; focused marks
// whether the pane has key focus (drives the caret + hint).
func renderApprovalsQueue(views []approval.View, cursor int, focused bool, width, height int) string {
	if len(views) == 0 {
		return padTo(stMuted.Render("Nothing waiting. ✅"), height)
	}
	hint := "tab to act"
	if focused {
		hint = "↑/↓ move · 1-9 answer · a attach · tab/esc leave"
	}
	var b strings.Builder
	b.WriteString(stMuted.Render("approvals — "+hint) + "\n\n")
	for i, v := range views {
		caret := "  "
		if i == cursor {
			caret = stCursor.Render("› ")
		}
		head := fmt.Sprintf("%s%s", caret, stPaneTitle.Render(v.ID))
		if v.Action != "" {
			head += "  " + stMuted.Render(trunc(v.Action, max(0, width-len(v.ID)-4)))
		}
		b.WriteString(head + "\n")
		if v.Recognized {
			if v.Question != "" {
				b.WriteString("    " + stMuted.Render(trunc(v.Question, max(0, width-4))) + "\n")
			}
			var opts []string
			for j, label := range v.Options {
				opts = append(opts, fmt.Sprintf("[%d] %s", j+1, label))
			}
			b.WriteString("    " + trunc(strings.Join(opts, "  "), max(0, width-4)) + "\n")
		} else {
			b.WriteString("    " + stError.Render("⚠ unrecognized — press a to attach") + "\n")
		}
		b.WriteString("\n")
	}
	return padTo(strings.TrimRight(b.String(), "\n"), height)
}
