package insights

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/llm"
)

// Narrate renders a short natural-language summary of a Report. It routes through
// the local model when one is supplied, but the deterministic template is the
// floor: a nil Completer, any Complete error, or an empty / whitespace-only reply
// all fall back to DeterministicSummary(r). An empty model reply is never trusted
// (it carries no signal) — exactly the digest/llm Summarize contract. The model
// only ever enriches; it never blocks or replaces the deterministic output, so
// the return value is always non-empty.
func Narrate(ctx context.Context, c llm.Completer, r Report) string {
	floor := DeterministicSummary(r)
	if c == nil {
		return floor
	}
	out, err := c.Complete(ctx, NarratorPrompt(r))
	if err != nil {
		return floor
	}
	if s := cleanLine(out); s != "" {
		return s
	}
	return floor
}

// DeterministicSummary turns a Report into a compact plain-language paragraph
// using only the structured facts — no model. It is the narrator's guaranteed
// floor and stands alone as the `--json`-off default when local_llm is disabled.
func DeterministicSummary(r Report) string {
	if r.Sessions == 0 {
		return "No agent session history to analyze yet."
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("Analyzed %s (%d active).",
		plural(r.Sessions, "session"), r.ActiveSessions))

	if len(r.Durations) > 0 {
		slow := slowestType(r.Durations)
		seg := fmt.Sprintf("Slowest type: %s (median %s)", slow.Type, humanDurSec(slow.MedianSec))
		if len(slow.Outliers) > 0 {
			seg += fmt.Sprintf(", %s", plural(len(slow.Outliers), "outlier"))
		}
		parts = append(parts, seg+".")
	}
	if len(r.ErrorRates) > 0 && r.ErrorRates[0].Errored > 0 {
		e := r.ErrorRates[0]
		parts = append(parts, fmt.Sprintf("Highest error rate: %s %.0f%% (%d/%d).",
			e.Type, e.Rate*100, e.Errored, e.Total))
	}
	if len(r.BusiestPeriods) > 0 {
		b := r.BusiestPeriods[0]
		parts = append(parts, fmt.Sprintf("Busiest hour: %02d:00 UTC (%s).",
			b.Hour, plural(b.Count, "session")))
	}
	if len(r.Parallelizable) > 0 {
		var saved int64
		for _, p := range r.Parallelizable {
			saved += p.SavedSec
		}
		parts = append(parts, fmt.Sprintf("%s (≈%s wall-clock saveable).",
			plural(len(r.Parallelizable), "parallelization opportunity"), humanDurSec(saved)))
	}
	if len(r.CoEdits) > 0 {
		top := r.CoEdits[0]
		parts = append(parts, fmt.Sprintf("Most co-edited: %s + %s (%d sessions).",
			top.A, top.B, top.Count))
	}
	if len(r.Anomalies) > 0 {
		parts = append(parts, fmt.Sprintf("%s flagged.", plural(len(r.Anomalies), "agent anomaly")))
	}
	return strings.Join(parts, " ")
}

// NarratorPrompt builds the model prompt from the deterministic facts. Like the
// digest narrator it is blunt about omitting preamble — the local model otherwise
// tends to restate the request before the summary.
func NarratorPrompt(r Report) string {
	var b strings.Builder
	b.WriteString("Summarize, in 2-4 sentences, what these warden agent-history insights tell the operator. ")
	b.WriteString("Lead with the most actionable suggestion. Output ONLY the summary — start with the first word of it. ")
	b.WriteString("Do NOT restate this request, do NOT add any preamble, label, or quotes.\n\n")
	fmt.Fprintf(&b, "Sessions analyzed: %d (%d active)\n", r.Sessions, r.ActiveSessions)
	for _, d := range r.Durations {
		fmt.Fprintf(&b, "Duration[%s]: median %s, p90 %s, max %s, %d outliers\n",
			d.Type, humanDurSec(d.MedianSec), humanDurSec(d.P90Sec), humanDurSec(d.MaxSec), len(d.Outliers))
	}
	for _, e := range r.ErrorRates {
		if e.Errored > 0 {
			fmt.Fprintf(&b, "Errors[%s]: %d/%d (%.0f%%)\n", e.Type, e.Errored, e.Total, e.Rate*100)
		}
	}
	for _, p := range r.Parallelizable {
		fmt.Fprintf(&b, "Parallelize: %s\n", p.Reason)
	}
	for _, c := range r.CoEdits {
		fmt.Fprintf(&b, "Co-edited: %s + %s (%d sessions)\n", c.A, c.B, c.Count)
	}
	for _, a := range r.Anomalies {
		fmt.Fprintf(&b, "Anomaly[%s]: %s\n", a.Agent, strings.Join(a.Notes, "; "))
	}
	return b.String()
}

// slowestType returns the duration entry with the largest median.
func slowestType(ds []TypeDuration) TypeDuration {
	best := ds[0]
	for _, d := range ds[1:] {
		if d.MedianSec > best.MedianSec {
			best = d
		}
	}
	return best
}

// cleanLine collapses a model reply to a trimmed single paragraph (digest parity).
func cleanLine(out string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(out)), " ")
}

// plural renders "1 session" / "3 sessions" (naive +s, good enough for the nouns
// used here; "opportunity" → "opportunities" and "anomaly" → "anomalies" handled).
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	switch {
	case strings.HasSuffix(noun, "y"):
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(noun, "y"))
	default:
		return fmt.Sprintf("%d %ss", n, noun)
	}
}

// humanDurSec renders a second count as a compact "1h2m" / "3m" / "45s".
func humanDurSec(sec int64) string {
	d := time.Duration(sec) * time.Second
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
