package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/srjn45/warden/internal/fastbrain"
)

// fbDecisionRows bounds how many recent decisions the page asks for and shows.
const fbDecisionRows = 8

// fastBrainMsg carries a Fast-Brain telemetry snapshot + recent decisions to
// the FastBrain page. action marks the result of a pause/resume so its errors
// reach the status line; a passive refresh keeps the last good data on a blip.
type fastBrainMsg struct {
	tel       fastbrain.TelemetrySnapshot
	decisions []fastbrain.Decision
	err       error
	action    bool
}

func fastBrainCmd(a api) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		tel, err := a.FastBrainMetrics(ctx)
		if err != nil {
			return fastBrainMsg{err: err}
		}
		ds, err := a.FastBrainDecisions(ctx, fbDecisionRows)
		return fastBrainMsg{tel: tel, decisions: ds, err: err}
	}
}

// fastBrainControlCmd pauses/resumes one kind (default TTL) then reloads.
func fastBrainControlCmd(a api, kind string, paused bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		if _, err := a.SetFastBrainControl(ctx, kind, paused, 0); err != nil {
			return fastBrainMsg{err: err, action: true}
		}
		tel, err := a.FastBrainMetrics(ctx)
		if err != nil {
			return fastBrainMsg{err: err, action: true}
		}
		ds, _ := a.FastBrainDecisions(ctx, fbDecisionRows)
		return fastBrainMsg{tel: tel, decisions: ds, action: true}
	}
}

func (m controlPaneModel) fbRow() (fastbrain.KindControl, bool) {
	if m.fbCursor < 0 || m.fbCursor >= len(m.fbTel.Controls) {
		return fastbrain.KindControl{}, false
	}
	return m.fbTel.Controls[m.fbCursor], true
}

// fastBrainBody renders the page: admission summary, circuits, per-kind
// controls (cursor row marked), then the recent decision trace. Only bounded
// vocabulary is shown — never prompts, paths or agent IDs.
func fastBrainBody(t fastbrain.TelemetrySnapshot, ds []fastbrain.Decision, cursor int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "active %d/%d · admitted %d · cache hits %d · coalesced %d · preempted %d · abandoned %d\n",
		t.Active, t.MaxConcurrent, t.Admitted, t.CacheHits, t.Coalesced, t.Preempted, t.Abandoned)
	for _, c := range t.Circuits {
		fmt.Fprintf(&b, "circuit %s: %s", c.Provider, c.State)
		if c.ConsecutiveFailures > 0 {
			fmt.Fprintf(&b, " (%d fails)", c.ConsecutiveFailures)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + stPaneTitle.Render("Kinds (p pause/resume)") + "\n")
	attempts := map[fastbrain.DecisionKind]uint64{}
	for _, k := range t.Kinds {
		attempts[k.Kind] += k.Attempts
	}
	if len(t.Controls) == 0 {
		b.WriteString(stMuted.Render("(no kinds)") + "\n")
	}
	for i, c := range t.Controls {
		mark, state := "  ", "on"
		if i == cursor {
			mark = "▸ "
		}
		if c.Paused {
			state = stAttention.Render("paused")
			if c.Source != "" {
				state += " (" + c.Source + ")"
			}
		}
		fmt.Fprintf(&b, "%s%-24s %-8s calls %d\n", mark, string(c.Kind), state, attempts[c.Kind])
	}
	b.WriteString("\n" + stPaneTitle.Render("Recent decisions") + "\n")
	if len(ds) == 0 {
		b.WriteString(stMuted.Render("(none yet)") + "\n")
	}
	for _, d := range ds {
		prov := d.Provider
		if prov == "" {
			prov = "-"
		}
		fmt.Fprintf(&b, "%s %-22s %-9s %-9s %s wait %dms run %dms\n",
			d.Time.Format("15:04:05"), string(d.Kind), d.Outcome, d.FinalAction, prov, d.QueueWaitMs, d.RunMs)
	}
	return b.String()
}
