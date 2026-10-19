package tui

import (
	"fmt"
	"strings"

	"github.com/srjn45/warden/internal/ptyobs"
)

// dangerBanner is the conspicuous bold-red line shown while a session runs
// dangerously-execute-all.
func dangerBanner() string { return stError.Render(ptyobs.DangerBanner) }

// runtimeBody renders the live PTY runtime status of one session: stream
// state with its typed reason and evidence, input owner, decision route and
// the effective permission posture.
func runtimeBody(v ptyobs.RuntimeView) string {
	var b strings.Builder
	if v.Danger {
		b.WriteString(dangerBanner() + "\n\n")
	}
	row := func(label, val string) {
		if val != "" {
			b.WriteString(stMuted.Render(fmt.Sprintf("%-10s", label)) + val + "\n")
		}
	}
	row("runtime", v.Mode)
	if !v.Supervised {
		row("stream", "not supervised (runtime.pty was off at spawn)")
		row("posture", v.Posture)
		return b.String()
	}
	state := v.State
	if v.Reason != "" {
		state += " (" + v.Reason + ")"
	}
	row("state", state)
	row("posture", v.Posture)
	row("input", v.InputOwner)
	row("route", v.Route)
	row("last", v.LastOutcome)
	if v.QuietSeconds > 0 {
		row("quiet", fmt.Sprintf("%.0fs", v.QuietSeconds))
	}
	if len(v.Evidence) > 0 {
		row("evidence", strings.Join(v.Evidence, "; "))
	}
	if v.Prompt != nil {
		row("prompt", v.Prompt.Text)
	}
	return b.String()
}
