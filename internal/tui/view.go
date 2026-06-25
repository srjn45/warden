package tui

import (
	"fmt"

	"github.com/srjn45/warden/internal/client"
)

// pressureChip renders the header memory-pressure gauge. Empty until the first
// sample arrives (Level 0 = no sample yet); amber when elevated, grey otherwise.
func pressureChip(p client.PressureStatus) string {
	if p.Level == 0 {
		return ""
	}
	style := stIdle
	if p.Elevated {
		style = stAttention
	}
	return style.Render(fmt.Sprintf("pressure: %s · %d/%d", p.LevelName, p.AgentCount, p.MaxAgents))
}

func helpText() string {
	return stPaneTitle.Render("Keys") + "\n" +
		"  ↑/↓ or j/k   move selection\n" +
		"  ←/→ or h/l   collapse / expand the pipeline under the cursor\n" +
		"  enter        open the selected agent in the detail pane\n" +
		"  n            new agent (prompt)\n" +
		"  o            open a directory as a group (spawn target for n)\n" +
		"  s            send a message to the selected agent\n" +
		"  a            full-screen attach to its tmux session (or a running pipeline job's session)\n" +
		"  d            completion digest for the selected agent (scrollable; d/esc to close)\n" +
		"  i            agent details for the selected agent (scrollable; i/esc to close)\n" +
		"  p            answer pending approvals (or enter on the ⏳ row; 1-9 to answer, tab for next)\n" +
		"  c            shared-context + message-traffic inspector\n" +
		"  r            retry a failed/needs-attention pipeline job\n" +
		"  x            kill agent / cancel pipeline / close dir (context-sensitive)\n" +
		"  D            delete a stopped pipeline's record (confirm y/N)\n" +
		"  ?            toggle this help\n" +
		"  q            quit\n" +
		"\n" +
		stPaneTitle.Render("Typing in the Claude pane") + "\n" +
		"  enter        submit the prompt\n" +
		"  alt+enter    insert a newline (works on every terminal)\n" +
		"  shift+enter  insert a newline (only on terminals that report it;\n" +
		"               VTE/GNOME terminals can't — use alt+enter there)\n"
}
