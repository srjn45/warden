package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
)

// tierCycle is the order the `t` key rotates a backend through. The reserved
// `local` tier is system-set and never appears here (the local row rejects `t`).
var tierCycle = []string{
	backendstore.TierFree,
	backendstore.TierSubscription,
	backendstore.TierPayPerUse,
	backendstore.TierUnclassified,
}

// nextTier returns the tier that follows cur in the cycle, wrapping around. A tier
// not in the cycle (e.g. an unset row) starts at the first entry (free).
func nextTier(cur string) string {
	for i, t := range tierCycle {
		if t == cur {
			return tierCycle[(i+1)%len(tierCycle)]
		}
	}
	return tierCycle[0]
}

// sortBackendsState returns state with its backend rows in stable id-ascending
// order, so the Backends-page cursor indexes the same rows the daemon lists (it
// already sorts, but this keeps the TUI robust to ordering changes).
func sortBackendsState(state client.BackendsState) client.BackendsState {
	rows := append([]client.Backend(nil), state.Backends...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	state.Backends = rows
	return state
}

// enabledWord is the status-line verb for a toggle.
func enabledWord(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}

// boolMark renders a bool cell as ✓/- (the same glyphs as the CLI table).
func boolMark(v bool) string {
	if v {
		return "✓"
	}
	return "-"
}

// backendLimited shows how long a backend stays rate-limited (rounded), or "-"
// when it is not currently limited. Kept short — no absolute time — for the narrow
// control pane. The local row is never limited.
func backendLimited(until time.Time) string {
	if until.IsZero() {
		return "-"
	}
	rem := time.Until(until)
	if rem <= 0 {
		return "-"
	}
	return rem.Round(time.Second).String()
}

// backendsBody renders the Backends page body: a table (ID, installed, tier, default, enabled, limited)
// with the row under cursor marked. It includes the reserved local row exactly as
// the daemon returns it. The table is compact for the narrow control pane; titleBox
// clamps any overflow.
func backendsBody(state client.BackendsState, cursor int) string {
	var b strings.Builder

	rows := state.Backends
	if len(rows) == 0 {
		b.WriteString(stMuted.Render("(no backends — press r to rescan)"))
		return b.String()
	}

	idW := len("ID")
	for _, be := range rows {
		if len(be.ID) > idW {
			idW = len(be.ID)
		}
	}
	const tierW = 12 // holds "subscription" / "unclassified"

	// header — muted, aligned to the row columns below.
	b.WriteString(stMuted.Render(fmt.Sprintf("  %-*s  %-4s  %-*s  %-3s  %-3s  %s",
		idW, "ID", "INST", tierW, "TIER", "DEF", "EN", "LIMITED")))
	b.WriteString("\n")

	for i, be := range rows {
		marker := "  "
		if i == cursor {
			marker = "› "
		}
		row := fmt.Sprintf("%s%-*s  %-4s  %-*s  %-3s  %-3s  %s",
			marker,
			idW, be.ID,
			boolMark(be.Installed),
			tierW, be.Tier,
			boolMark(be.Default),
			boolMark(be.Enabled),
			backendLimited(be.LimitedUntil),
		)
		if i == cursor {
			b.WriteString(stCursor.Render(row))
		} else {
			b.WriteString(row)
		}
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
