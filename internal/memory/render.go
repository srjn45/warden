package memory

import (
	"fmt"
	"sort"
	"strings"
)

// DefaultBudget is the hard byte ceiling for a rendered projection (§4.3): a tight
// "where things live" list, not a wiki. ~2 KB ≈ a few hundred tokens — generous for
// Claude's cached system prompt, and the mandatory cap for BYO-model backends that
// may re-bill the full block every turn. PR-1 injects within this budget.
const DefaultBudget = 2048

// projectionHeader leads the rendered block so an agent knows what it is reading and
// to treat it as navigational learned context, not authority.
const projectionHeader = "warden project memory (durable cross-agent facts — navigational, verify before relying on any one):"

// unverifiedCaveat is appended to an unverified entry's line so a reading agent sees
// the freshness warning inline (§4.2).
const unverifiedCaveat = " [unverified — learned context, may be stale, verify before relying]"

// Render produces the budgeted projection string for these entries — the exact text
// PR-1 feeds to the injection seam (Claude's --append-system-prompt / the file-drop
// block). It is compact and navigational: a header plus one bullet per entry, with
// unverified entries flagged inline.
//
// budget is a HARD byte cap (<= 0 means DefaultBudget). When the entries don't fit,
// the lowest-value ones are dropped FIRST — lowest trust, then oldest — and a
// "(N more trimmed to fit budget)" note is added; the kept entries still render in
// their authored order. An empty memory renders "" (PR-1 then projects nothing,
// keeping Claude's launch byte-identical).
func (m *Memory) Render(budget int) string {
	if budget <= 0 {
		budget = DefaultBudget
	}

	// Only LIVE entries are projected: a tombstoned (superseded/aged-out) or
	// stale-flagged entry stays in the committed file for the diff reviewer but is
	// never fed to an agent (§4.2). Trim accounting counts only live entries too.
	live := 0
	for i := range m.Entries {
		if m.Entries[i].Live() {
			live++
		}
	}
	if live == 0 {
		return ""
	}

	// Decide which entries to keep within budget. We rank a COPY by trim-priority
	// (highest-value first) and greedily keep while the rendered whole stays under
	// budget, but we emit the kept set in the original authored order.
	order := make([]int, len(m.Entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return higherValue(m.Entries[order[a]], m.Entries[order[b]])
	})

	keep := make([]bool, len(m.Entries))
	used := len(projectionHeader)
	kept := 0
	for _, idx := range order {
		if !m.Entries[idx].Live() {
			continue
		}
		line := "\n" + renderLine(m.Entries[idx])
		// Reserve room for a possible trim note if anything ends up dropped.
		if used+len(line) > budget {
			break
		}
		used += len(line)
		keep[idx] = true
		kept++
	}

	var b strings.Builder
	b.WriteString(projectionHeader)
	for i, e := range m.Entries {
		if keep[i] {
			b.WriteByte('\n')
			b.WriteString(renderLine(e))
		}
	}
	if dropped := live - kept; dropped > 0 {
		note := fmt.Sprintf("\n(… %d more entr%s trimmed to fit the memory budget)", dropped, plural(dropped))
		// Only append the note if it itself fits; the cap is hard.
		if b.Len()+len(note) <= budget {
			b.WriteString(note)
		}
	}
	return b.String()
}

// RenderDefault renders with DefaultBudget — the common path for callers that don't
// tune the cap.
func (m *Memory) RenderDefault() string { return m.Render(DefaultBudget) }

// renderLine formats one entry as a projection bullet: the fact, then the unverified
// caveat when applicable. Metadata (date/provenance) is deliberately omitted from the
// projection — it is bookkeeping for the committed diff, not tokens an agent needs.
func renderLine(e Entry) string {
	line := "- " + strings.TrimSpace(e.Text)
	if e.Unverified() {
		line += unverifiedCaveat
	}
	return line
}

// higherValue reports whether entry x should be KEPT before y when trimming to fit:
// higher trust first, then newer timestamp first. Plain (TrustNone) human bullets
// rank between trusted and unverified — a human wrote them, but they carry no
// explicit corroboration.
func higherValue(x, y Entry) bool {
	if tx, ty := trustRank(x.Trust), trustRank(y.Trust); tx != ty {
		return tx > ty
	}
	// Newer timestamp first; empty timestamp sorts oldest. YYYY-MM-DD compares
	// lexicographically.
	return x.Timestamp > y.Timestamp
}

func trustRank(t string) int {
	switch t {
	case TrustTrusted:
		return 2
	case TrustNone:
		return 1
	default: // unverified
		return 0
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
