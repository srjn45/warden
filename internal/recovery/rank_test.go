package recovery

import (
	"testing"
	"time"

	"github.com/srjn45/warden/internal/backendusage"
	"github.com/stretchr/testify/require"
)

func pct(v float64) *float64 { return &v }

func TestRankOverlappingCodexWindowsUsesMinimumHeadroom(t *testing.T) {
	now := time.Now().UTC()
	got := Rank([]Candidate{{BackendID: "codex", ModelID: "gpt-5"}}, backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "codex", Usage: []backendusage.Limit{
		{ID: "short", Scope: "short", Label: "5 hour", UsedPercent: pct(20), ResetsAt: &now},
		{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: pct(85), ResetsAt: &now},
	}}}})
	require.Len(t, got, 1)
	require.Equal(t, float64(15), *got[0].Headroom)
	require.Len(t, got[0].Resets, 2)
}

func TestRankAntigravityAlternativePoolsAndUnknown(t *testing.T) {
	got := Rank([]Candidate{
		{BackendID: "antigravity", ModelID: "gemini-3-pro"},
		{BackendID: "antigravity", ModelID: "claude-sonnet"},
	}, backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "antigravity", Usage: []backendusage.Limit{
		{ID: "gemini", Scope: "gemini", Label: "Gemini", ModelFamilies: []string{"gemini"}, UsedPercent: pct(90)},
		{ID: "other", Scope: "non-gemini", Label: "Other", UsedPercent: nil},
	}}}})
	require.Equal(t, "gemini-3-pro", got[0].ModelID)
	require.Equal(t, float64(10), *got[0].Headroom)
	require.Equal(t, "claude-sonnet", got[1].ModelID)
	require.Nil(t, got[1].Headroom, "unknown usage must not be fabricated")
}
