package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/backendusage"
	"github.com/srjn45/warden/internal/client"
	"github.com/stretchr/testify/require"
)

func TestEffectiveCandidateTier(t *testing.T) {
	roles := []backendstore.RoleTierMapping{
		{RoleName: "orchestrator", DefaultTier: backendstore.Tier1},
		{RoleName: "worker", DefaultTier: backendstore.Tier2},
	}
	require.Equal(t, "tier-3", effectiveCandidateTier("tier-3", "orchestrator", roles))
	require.Equal(t, "tier-1", effectiveCandidateTier("auto", "orchestrator", roles))
	require.Equal(t, "tier-2", effectiveCandidateTier("auto", "", roles), "no role → tier-2 default")
	require.Equal(t, "tier-2", effectiveCandidateTier("auto", "ghost", roles), "unknown role → tier-2")
}

func TestBuildSpawnCandidatesRanksAndMarksWinner(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	usedLow := 20.0
	usedHigh := 80.0
	usedFull := 100.0
	reset := now.Add(2 * time.Hour)
	models := []backendstore.ModelEntry{
		{BackendID: "cursor", ModelID: "auto", DisplayName: "Auto", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true, QuotaScope: "auto"},
		{BackendID: "cursor", ModelID: "claude-opus", DisplayName: "Opus", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true, QuotaScope: "api"},
		{BackendID: "claude", ModelID: "sonnet", DisplayName: "Sonnet", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true, QuotaScope: "session"},
		{BackendID: "claude", ModelID: "manual-only", Tier: backendstore.Tier2, Enabled: true, AutoAssign: false},
	}
	snap := backendusage.Snapshot{Backends: []backendusage.BackendResult{
		{ID: "cursor", Usage: []backendusage.Limit{
			{Scope: "auto", UsedPercent: &usedLow},
			{Scope: "api", UsedPercent: &usedFull, ResetsAt: &reset, LimitState: strPtr("limited")},
		}},
		{ID: "claude", Usage: []backendusage.Limit{
			{Scope: "session", UsedPercent: &usedHigh},
		}},
	}}
	backends := client.BackendsState{Backends: []client.Backend{
		{ID: "cursor", Installed: true, Enabled: true},
		{ID: "claude", Installed: true, Enabled: true},
	}}

	got := buildSpawnCandidates(models, snap, backends, now)
	require.Len(t, got, 3, "manual-only must be filtered out")
	require.Equal(t, "cursor", got[0].BackendID)
	require.Equal(t, "auto", got[0].ModelID)
	require.True(t, got[0].Winner, "highest headroom eligible is winner")
	require.False(t, got[0].Limited)

	var opus *spawnCandidate
	for i := range got {
		if got[i].ModelID == "claude-opus" {
			opus = &got[i]
			break
		}
	}
	require.NotNil(t, opus)
	require.True(t, opus.Limited)
	require.False(t, opus.Winner)
	require.Equal(t, reset, opus.LimitedUntil)
}

func TestFormatCandidateRowLimited(t *testing.T) {
	until := time.Date(2026, 9, 26, 18, 30, 0, 0, time.Local)
	line := formatCandidateRow(spawnCandidate{
		Tier: "tier-2", BackendID: "cursor", DisplayName: "Opus",
		Headroom: 0, UsedPct: 100, Limited: true, LimitedUntil: until,
	})
	require.Contains(t, line, "limited until 18:30")
	require.Contains(t, line, "cursor/")
}

func TestHeadroomBar(t *testing.T) {
	require.Equal(t, "[████░░░░]", headroomBar(0.5, 8))
	require.Equal(t, "[████████]", headroomBar(1, 8))
	require.Equal(t, "[░░░░░░░░]", headroomBar(0, 8))
}

func TestTierPickerAndSpawnWiring(t *testing.T) {
	f := &fakeAPI{
		models: []backendstore.ModelEntry{
			{BackendID: "claude", ModelID: "sonnet", DisplayName: "Sonnet", Tier: backendstore.Tier2, Enabled: true, AutoAssign: true, QuotaScope: "session"},
		},
		usageSnap: backendusage.Snapshot{SchemaVersion: 1},
		backends:  client.BackendsState{Backends: []client.Backend{{ID: "claude", Installed: true, Enabled: true}}},
	}
	m := newListPane(f, "", "")
	m.mode = modeNewAgent
	m.tierIdx = 2 // tier-2
	require.Equal(t, "tier-2", m.selectedSpawnTier())
	require.Equal(t, "tier-2", m.selectedSpawnTierLabel())

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlT})
	um := updated.(controlPaneModel)
	require.Equal(t, modeNewAgentTier, um.mode)
	require.NotNil(t, cmd)
	msg := cmd()
	cm, ok := msg.(spawnCandidatesMsg)
	require.True(t, ok)
	require.Empty(t, cm.err)
	require.NotEmpty(t, cm.candidates)

	um2, _ := um.Update(cm)
	view := um2.(controlPaneModel).candidateTableView()
	require.Contains(t, view, "claude/")
	require.True(t, strings.Contains(view, "selected") || strings.Contains(view, "█"), view)

	// Submit with explicit tier must pass Tier, not Backend.
	um2c := um2.(controlPaneModel)
	um2c.mode = modeNewAgent
	um2c.ta.SetValue("ship it")
	um2c.tierIdx = 1 // tier-1
	_, cmd = um2c.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
	require.NotNil(t, cmd)
	done := cmd().(spawnDoneMsg)
	require.NoError(t, done.err)
	require.Equal(t, "tier-1", f.spawned.Tier)
	require.Empty(t, f.spawned.Backend)
}

func TestSelectedSpawnTierAutoIsEmpty(t *testing.T) {
	m := newListPane(&fakeAPI{}, "", "")
	m.tierIdx = 0
	require.Empty(t, m.selectedSpawnTier())
	require.Equal(t, "auto", m.selectedSpawnTierLabel())
}

func strPtr(s string) *string { return &s }
