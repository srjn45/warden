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

func TestRankCursorTripleBucketAppliesDistinctHeadrooms(t *testing.T) {
	limits := []backendusage.Limit{
		{ID: "cursor:included", Scope: "included", Label: "Included", ModelFamilies: []string{"composer", "cursor-grok"}, UsedPercent: pct(10)},
		{ID: "cursor:auto", Scope: "auto", Label: "Auto", Models: []string{"auto"}, UsedPercent: pct(4)},
		{ID: "cursor:api", Scope: "api", Label: "API", ModelFamilies: []string{"claude", "gpt", "gemini", "kimi", "glm"}, UsedPercent: pct(100)},
	}
	snap := backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "cursor", Usage: limits}}}
	got := Rank([]Candidate{
		{BackendID: "cursor", ModelID: "claude-opus-5-thinking-high"},
		{BackendID: "cursor", ModelID: "cursor-grok-4.6-high-fast"},
		{BackendID: "cursor", ModelID: "auto"},
		{BackendID: "cursor", ModelID: "composer-2.5-fast"},
	}, snap)

	byID := map[string]Candidate{}
	for _, c := range got {
		byID[c.ModelID] = c
	}

	opus := byID["claude-opus-5-thinking-high"]
	require.Equal(t, float64(0), *opus.Headroom)
	require.Equal(t, []string{"cursor:api"}, resetIDs(opus))

	grok := byID["cursor-grok-4.6-high-fast"]
	require.Equal(t, float64(90), *grok.Headroom)
	require.Equal(t, []string{"cursor:included"}, resetIDs(grok))

	auto := byID["auto"]
	require.Equal(t, float64(96), *auto.Headroom)
	require.Equal(t, []string{"cursor:auto"}, resetIDs(auto))

	composer := byID["composer-2.5-fast"]
	require.Equal(t, float64(90), *composer.Headroom)
	require.Equal(t, []string{"cursor:included"}, resetIDs(composer))
}

func resetIDs(c Candidate) []string {
	ids := make([]string, 0, len(c.Resets))
	for _, r := range c.Resets {
		ids = append(ids, r.LimitID)
	}
	return ids
}
