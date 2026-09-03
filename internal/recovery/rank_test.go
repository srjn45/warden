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

// ---------------------------------------------------------------------------
// Spec §11 matrix rows: selectors / headroom / ranking
// ---------------------------------------------------------------------------

// TestRankCodexOneKnownOneUnknownWindow — Codex windows: one known, one unknown.
// The known window (short, 60% used) sets headroom to 40. The unknown weekly
// window must be preserved in Resets with nil ResetsAt, not fabricated as 0 or 100.
func TestRankCodexOneKnownOneUnknownWindow(t *testing.T) {
	now := time.Now().UTC()
	got := Rank([]Candidate{{BackendID: "codex", ModelID: "gpt-5"}}, backendusage.Snapshot{
		Backends: []backendusage.BackendResult{{ID: "codex", Usage: []backendusage.Limit{
			{ID: "short", Scope: "short", Label: "Short", UsedPercent: pct(60), ResetsAt: &now},
			{ID: "weekly", Scope: "weekly", Label: "Weekly", UsedPercent: nil, ResetsAt: nil},
		}}},
	})
	require.Len(t, got, 1)
	require.Equal(t, float64(40), *got[0].Headroom, "headroom must be minimum of known windows only")
	require.Len(t, got[0].Resets, 2, "both windows must appear in Resets")
	var hasNilReset bool
	for _, r := range got[0].Resets {
		if r.ResetsAt == nil {
			hasNilReset = true
		}
	}
	require.True(t, hasNilReset, "unknown window must have nil ResetsAt, not a fabricated value")
}

// TestRankAntigravityGeminiPoolLimitedNonGeminiEligible — Antigravity pools:
// Gemini pool exhausted (100%). Non-Gemini scope has unknown usage (nil).
// A non-Gemini model must remain independently eligible as unknown.
func TestRankAntigravityGeminiPoolLimitedNonGeminiEligible(t *testing.T) {
	got := Rank([]Candidate{
		{BackendID: "antigravity", ModelID: "gemini-flash"},
		{BackendID: "antigravity", ModelID: "claude-haiku"},
	}, backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "antigravity", Usage: []backendusage.Limit{
		{ID: "gemini", Scope: "gemini", Label: "Gemini", ModelFamilies: []string{"gemini"}, UsedPercent: pct(100)},
		{ID: "non-gemini", Scope: "non-gemini", Label: "Non-Gemini", UsedPercent: nil},
	}}}})
	byModel := map[string]Candidate{}
	for _, c := range got {
		byModel[c.ModelID] = c
	}
	require.Equal(t, float64(0), *byModel["gemini-flash"].Headroom, "gemini pool exhausted → headroom 0")
	require.Nil(t, byModel["claude-haiku"].Headroom, "non-gemini pool unknown → nil headroom (independently eligible)")
}

// TestRankAntigravityNonGeminiPoolLimitedGeminiEligible — Antigravity pools:
// Non-Gemini scope is fully used (100%). Gemini pool has positive headroom (70%).
// A Gemini model must remain independently eligible with its own headroom.
func TestRankAntigravityNonGeminiPoolLimitedGeminiEligible(t *testing.T) {
	got := Rank([]Candidate{
		{BackendID: "antigravity", ModelID: "gemini-pro"},
		{BackendID: "antigravity", ModelID: "claude-sonnet"},
	}, backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "antigravity", Usage: []backendusage.Limit{
		{ID: "gemini", Scope: "gemini", Label: "Gemini", ModelFamilies: []string{"gemini"}, UsedPercent: pct(30)},
		{ID: "non-gemini", Scope: "non-gemini", Label: "Non-Gemini", UsedPercent: pct(100)},
	}}}})
	byModel := map[string]Candidate{}
	for _, c := range got {
		byModel[c.ModelID] = c
	}
	require.Equal(t, float64(70), *byModel["gemini-pro"].Headroom, "gemini pool has 70%% headroom")
	require.Equal(t, float64(0), *byModel["claude-sonnet"].Headroom, "non-gemini pool exhausted → 0")
}

// TestRankPoolWindowPlusGlobalConstraint — Overlap: pool window plus global constraint.
// A pool window (50% used) and a global window (100% used, no selectors) both
// apply to the same model. The global exhaustion overrides the pool headroom.
// Every model matched by the global window has headroom=0.
func TestRankPoolWindowPlusGlobalConstraint(t *testing.T) {
	got := Rank([]Candidate{
		{BackendID: "backend", ModelID: "fast-model"},
		{BackendID: "backend", ModelID: "other-model"},
	}, backendusage.Snapshot{Backends: []backendusage.BackendResult{{ID: "backend", Usage: []backendusage.Limit{
		{ID: "pool", Scope: "pool", Label: "Pool", Models: []string{"fast-model"}, UsedPercent: pct(50)},
		{ID: "global", Scope: "global", Label: "Global", UsedPercent: pct(100)},
	}}}})
	byModel := map[string]Candidate{}
	for _, c := range got {
		byModel[c.ModelID] = c
	}
	require.Equal(t, float64(0), *byModel["fast-model"].Headroom, "global exhaustion overrides pool headroom")
	require.Equal(t, float64(0), *byModel["other-model"].Headroom, "global window constrains all matched models")
}

// TestRankZeroWindowsEligibleAsUnknown — Zero windows: backend reports none.
// A backend with no usage windows means usage is unknown. Candidates must
// remain eligible with nil headroom — not assigned fabricated 0 or 100.
func TestRankZeroWindowsEligibleAsUnknown(t *testing.T) {
	got := Rank([]Candidate{{BackendID: "codex", ModelID: "gpt-5"}}, backendusage.Snapshot{
		Backends: []backendusage.BackendResult{{ID: "codex", Usage: []backendusage.Limit{}}},
	})
	require.Len(t, got, 1)
	require.Nil(t, got[0].Headroom, "zero windows must yield nil headroom, not fabricated usage")
	require.Empty(t, got[0].Resets, "zero windows must yield empty Resets")
}

// TestRankRefreshFailureNoFabricatedUsage — Refresh failure: service unavailable/partial.
// An empty snapshot (simulating a refresh failure where no backends reported)
// must not fabricate usage. All candidates remain unknown and eligible.
func TestRankRefreshFailureNoFabricatedUsage(t *testing.T) {
	got := Rank([]Candidate{
		{BackendID: "codex", ModelID: "gpt-5"},
		{BackendID: "claude", ModelID: "claude-opus"},
	}, backendusage.Snapshot{Backends: nil})
	require.Len(t, got, 2)
	for _, c := range got {
		require.Nil(t, c.Headroom, "refresh failure must not fabricate usage; headroom must be nil for %s/%s", c.BackendID, c.ModelID)
		require.Empty(t, c.Resets, "refresh failure must not fabricate reset times")
	}
}
