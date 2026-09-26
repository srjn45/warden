package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/client"
)

// spawnTierChoices is the primary TUI spawn selector (D8): auto lets role/task
// derive the tier at spawn; the rest pin ResolveOptions.Tier explicitly.
var spawnTierChoices = []string{"auto", "tier-1", "tier-2", "tier-3"}

// spawnCandidate is one AutoAssign model row in the live candidate table under
// the tier selector. Headroom/used come from live usage (scoped) when known.
type spawnCandidate struct {
	Tier         string
	BackendID    string
	ModelID      string
	DisplayName  string
	Headroom     float64 // 0..1; 1 = unused / unknown
	UsedPct      float64 // 0..100
	Limited      bool
	LimitedUntil time.Time
	Winner       bool
}

// selectedSpawnTier returns the ResolveOptions.Tier value for spawn: empty for
// "auto" (daemon derives from role/task), otherwise tier-1/2/3.
func (m controlPaneModel) selectedSpawnTier() string {
	if m.tierIdx <= 0 || m.tierIdx >= len(spawnTierChoices) {
		return ""
	}
	t := spawnTierChoices[m.tierIdx]
	if t == "auto" {
		return ""
	}
	return t
}

// selectedSpawnTierLabel is the display label for the chosen tier (never blank).
func (m controlPaneModel) selectedSpawnTierLabel() string {
	if m.tierIdx >= 0 && m.tierIdx < len(spawnTierChoices) {
		return spawnTierChoices[m.tierIdx]
	}
	return "auto"
}

// effectiveCandidateTier is the catalog tier used to list candidates. For an
// explicit selector it is that tier; for auto it is the role's default (or
// tier-2 when unknown), matching resolver DetermineTargetTier defaults.
func effectiveCandidateTier(selectedTier, roleName string, roleTiers []backendstore.RoleTierMapping) string {
	if selectedTier != "" && selectedTier != "auto" {
		return selectedTier
	}
	if roleName != "" {
		for _, m := range roleTiers {
			if m.RoleName == roleName && m.DefaultTier.Valid() {
				return string(m.DefaultTier)
			}
		}
	}
	return string(backendstore.Tier2)
}

// buildSpawnCandidates assembles AutoAssign rows for a tier from the model
// catalog + live usage snapshot + backend registry. Ranking is highest-headroom
// first; the top non-limited row is marked Winner (resolver primary pick).
func buildSpawnCandidates(models []backendstore.ModelEntry, snap backendusage.Snapshot, backends client.BackendsState, now time.Time) []spawnCandidate {
	backendByID := make(map[string]client.Backend, len(backends.Backends))
	for _, b := range backends.Backends {
		backendByID[b.ID] = b
	}
	usageByBackend := make(map[string]backendusage.BackendResult, len(snap.Backends))
	for _, b := range snap.Backends {
		usageByBackend[b.ID] = b
	}

	out := make([]spawnCandidate, 0, len(models))
	for _, m := range models {
		if !m.AutoAssign || !m.Enabled {
			continue
		}
		c := spawnCandidate{
			Tier:        string(m.Tier),
			BackendID:   m.BackendID,
			ModelID:     m.ModelID,
			DisplayName: m.DisplayName,
			Headroom:    1,
			UsedPct:     0,
		}
		if c.DisplayName == "" {
			c.DisplayName = m.ModelID
		}

		scope := m.QuotaScope
		if scope == "" {
			scope = backendstore.DefaultQuotaScope
		}
		if br, ok := usageByBackend[m.BackendID]; ok {
			if lim, found := matchScopeLimit(br.Usage, scope); found {
				applyLimitToCandidate(&c, lim, now)
			}
		}
		if be, ok := backendByID[m.BackendID]; ok {
			if !be.LimitedUntil.IsZero() && be.LimitedUntil.After(now) {
				// Backend-wide stamp only when all scopes limited (D6); still
				// surfaces as limited in the table when present.
				c.Limited = true
				if c.LimitedUntil.IsZero() || be.LimitedUntil.Before(c.LimitedUntil) {
					c.LimitedUntil = be.LimitedUntil
				}
			}
			if !be.Installed || !be.Enabled {
				c.Limited = true // grey out; not a hard-limit clock
			}
		}
		out = append(out, c)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Limited != out[j].Limited {
			return !out[i].Limited // eligible first
		}
		if math.Abs(out[i].Headroom-out[j].Headroom) > 0.0001 {
			return out[i].Headroom > out[j].Headroom
		}
		if out[i].BackendID != out[j].BackendID {
			return out[i].BackendID < out[j].BackendID
		}
		return out[i].ModelID < out[j].ModelID
	})

	for i := range out {
		if !out[i].Limited {
			out[i].Winner = true
			break
		}
	}
	return out
}

func matchScopeLimit(limits []backendusage.Limit, scope string) (backendusage.Limit, bool) {
	for _, lim := range limits {
		if lim.Scope == scope {
			return lim, true
		}
	}
	// Fall back to a single untitled/default row when the provider only
	// reports one window (e.g. Claude session) and the model is "default".
	if scope == backendstore.DefaultQuotaScope && len(limits) == 1 {
		return limits[0], true
	}
	return backendusage.Limit{}, false
}

func applyLimitToCandidate(c *spawnCandidate, lim backendusage.Limit, now time.Time) {
	if lim.UsedPercent != nil {
		c.UsedPct = *lim.UsedPercent
		c.Headroom = math.Max(0, 1-(*lim.UsedPercent)/100)
	}
	if lim.RemainingPercent != nil {
		c.Headroom = math.Max(0, (*lim.RemainingPercent)/100)
		c.UsedPct = math.Max(0, 100-(*lim.RemainingPercent))
	}
	if lim.ResetsAt != nil && !lim.ResetsAt.IsZero() {
		// Treat a fully-exhausted window with a future reset as limited.
		if c.Headroom <= 0.001 && lim.ResetsAt.After(now) {
			c.Limited = true
			c.LimitedUntil = *lim.ResetsAt
		}
	}
	if lim.LimitState != nil {
		switch strings.ToLower(*lim.LimitState) {
		case "limited", "exhausted", "rate_limited":
			c.Limited = true
			if lim.ResetsAt != nil {
				c.LimitedUntil = *lim.ResetsAt
			}
		}
	}
}

// tierPickerView renders the auto/tier-1/2/3 selector with the current choice marked.
func (m controlPaneModel) tierPickerView() string {
	var b strings.Builder
	for i, t := range spawnTierChoices {
		if i == m.tierIdx {
			b.WriteString(stCursor.Render("› " + t))
		} else {
			b.WriteString(stMuted.Render("  " + t))
		}
		if i < len(spawnTierChoices)-1 {
			b.WriteString("  ")
		}
	}
	return b.String()
}

// candidateTableView renders the live candidate table under the tier selector.
// Limited rows are greyed with "limited until HH:MM"; the winning candidate is
// highlighted. An empty/loading/error state is a single muted line.
func (m controlPaneModel) candidateTableView() string {
	if m.candidatesErr != "" {
		return stMuted.Render("(" + m.candidatesErr + ")")
	}
	if m.candidatesLoading {
		return stMuted.Render("(loading candidates…)")
	}
	if len(m.candidates) == 0 {
		return stMuted.Render("(no auto-assign candidates for this tier)")
	}
	var b strings.Builder
	for i, c := range m.candidates {
		line := formatCandidateRow(c)
		switch {
		case c.Limited:
			b.WriteString(stMuted.Render(line))
		case c.Winner:
			b.WriteString(stCursor.Render(line))
		default:
			b.WriteString(line)
		}
		if i < len(m.candidates)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func formatCandidateRow(c spawnCandidate) string {
	model := c.DisplayName
	if model == "" {
		model = c.ModelID
	}
	status := "ok"
	if c.Limited {
		if !c.LimitedUntil.IsZero() {
			status = "limited until " + c.LimitedUntil.Local().Format("15:04")
		} else {
			status = "unavailable"
		}
	} else if c.Winner {
		status = "selected"
	}
	bar := headroomBar(c.Headroom, 8)
	return fmt.Sprintf("%-7s  %s/%-22s  %s  %5.0f%%  %s",
		c.Tier, c.BackendID, truncateRunes(model, 22), bar, c.UsedPct, status)
}

// headroomBar renders a fixed-width bar for remaining capacity (full = unused).
func headroomBar(headroom float64, width int) string {
	if width <= 0 {
		return ""
	}
	if headroom < 0 {
		headroom = 0
	}
	if headroom > 1 {
		headroom = 1
	}
	filled := int(math.Round(headroom * float64(width)))
	if filled > width {
		filled = width
	}
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < width; i++ {
		if i < filled {
			b.WriteRune('█')
		} else {
			b.WriteRune('░')
		}
	}
	b.WriteByte(']')
	return b.String()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
